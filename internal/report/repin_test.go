package report

import (
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/model"
)

const digest = "f65ed258206d1eb6aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestShortImage(t *testing.T) {
	if len(digest) != 64 {
		t.Fatal("bad fixture")
	}
	if got := shortImage("nginx:1.30@sha256:" + digest); got != "nginx:1.30@sha256:f65ed258206d" {
		t.Errorf("got %q", got)
	}
	if got := shortImage("nginx:1.30"); got != "nginx:1.30" {
		t.Errorf("got %q", got)
	}
}

func cf(target, image, id, fix string) model.Finding {
	f := model.Finding{ID: id, Target: target, Image: image, Package: "p", Version: "1.0.0", Type: "npm", Severity: "high", FixState: "not-fixed",
		Decision: model.Decision{Action: model.Block, Reasons: []string{"r"}}}
	if fix != "" {
		f.FixState, f.FixVersions = "fixed", []string{fix}
	}
	return f
}

func composeRun(findings ...model.Finding) *model.Run {
	img := "nginx:1.30@sha256:" + digest
	return &model.Run{Verdict: "block", Project: "p", Findings: findings,
		Targets: []model.TargetResult{
			{Name: "stack", Kind: "compose", Ref: "docker-compose.yml", Status: "scanned", Images: []model.ImageResult{
				{Ref: img, Services: []string{"web"}, Line: 4, Status: "scanned"},
				{Ref: "busybox:1", Services: []string{"a", "b"}, Line: 9, Status: "scanned"},
			}},
			{Name: "other", Kind: "compose", Ref: "test/compose.yml", Status: "scanned", Images: []model.ImageResult{
				{Ref: img, Services: []string{"web"}, Line: 2, Status: "scanned"},
			}},
		}}
}

func TestRepinsGroupsPerTargetAndImage(t *testing.T) {
	img := "nginx:1.30@sha256:" + digest
	r := composeRun(cf("stack", img, "A", "1.0.1"), cf("other", img, "B", ""), cf("stack", img, "C", ""))
	got := Repins(r, r.Findings)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Target != "stack" || got[0].Count != 2 || got[0].Unfixed != 1 || got[0].File != "docker-compose.yml" || got[0].Line != 4 || got[0].Services[0] != "web" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Target != "other" || got[1].Count != 1 || got[1].Unfixed != 1 || got[1].File != "test/compose.yml" || got[1].Line != 2 {
		t.Errorf("second = %+v", got[1])
	}
	if n := len(Repins(r, []model.Finding{{Target: "stack"}})); n != 0 {
		t.Errorf("non-compose finding gave %d repins", n)
	}
}

func TestRepinsMergesServicesOfSharedRef(t *testing.T) {
	r := composeRun(cf("stack", "busybox:1", "A", ""))
	r.Targets[0].Images = []model.ImageResult{
		{Ref: "busybox:1", Services: []string{"a", "b"}, Line: 9, Status: "scanned"},
		{Ref: "busybox:1", Services: []string{"b", "c"}, Line: 12, Status: "scanned"},
	}
	got := Repins(r, r.Findings)
	if len(got) != 1 || got[0].Line != 9 || strings.Join(got[0].Services, ",") != "a,b,c" {
		t.Errorf("got %+v", got)
	}
}

func TestRepinSentences(t *testing.T) {
	cases := []struct {
		count, unfixed int
		want           string
	}{
		{1, 0, "1 blocking vulnerability, fixed upstream"},
		{3, 0, "3 blocking vulnerabilities, all fixed upstream"},
		{1, 1, "1 blocking vulnerability with no fix yet, so a newer image may not clear it"},
		{2, 2, "2 blocking vulnerabilities with no fix yet, so a newer image may not clear them"},
		{3, 1, "3 blocking vulnerabilities, 1 of them with no fix yet, so a newer image may not clear all of them"},
	}
	for _, c := range cases {
		if got := repinSentence(Repin{Count: c.count, Unfixed: c.unfixed}); got != c.want {
			t.Errorf("%+v: got %q want %q", c, got, c.want)
		}
	}
	one := repinMarkdown(Repin{Image: "x", File: "c.yml", Line: 3, Services: []string{"web"}, Count: 1})
	want := "- Re-pin x (c.yml:3, service web): 1 blocking vulnerability, fixed upstream. Use a newer build of this tag or a newer tag, then scan again.\n"
	if one != want {
		t.Errorf("got %q", one)
	}
	two := repinMarkdown(Repin{Image: "x", File: "c.yml", Line: 3, Services: []string{"a", "b"}, Count: 1})
	if !strings.Contains(two, "(c.yml:3, services a, b)") {
		t.Errorf("got %q", two)
	}
}

func TestComposeFindingsGetNoUpgradeLine(t *testing.T) {
	img := "nginx:1.30@sha256:" + digest
	src := cf("src", "", "S", "1.0.1")
	src.Package = "lodash"
	in := cf("stack", img, "I", "1.0.1")
	in.Package = "inner"
	ups := Upgrades([]model.Finding{in, src})
	if len(ups) != 1 || ups[0].Package != "lodash" {
		t.Fatalf("ups = %+v", ups)
	}
	r := composeRun(in)
	text := Text(r, false)
	if !strings.Contains(text, "To unblock:\n  re-pin nginx:1.30@sha256:f65ed258206d (docker-compose.yml:4): 1 blocking vulnerability, fixed upstream\n") {
		t.Errorf("text:\n%s", text)
	}
	if strings.Contains(text, "upgrade ") {
		t.Errorf("unexpected upgrade line:\n%s", text)
	}
	md := Markdown(r)
	if !strings.Contains(md, "- Re-pin nginx:1.30@sha256:f65ed258206d (docker-compose.yml:4, service web): 1 blocking vulnerability, fixed upstream. Use a newer build of this tag or a newer tag, then scan again.\n\n| Package") {
		t.Errorf("markdown:\n%s", md)
	}
	t.Logf("MARKDOWN:\n%s\nTEXT:\n%s", md, text)
}

func TestWhere(t *testing.T) {
	img := "nginx:1.30@sha256:" + digest
	f := cf("stack", img, "I", "")
	if got := Where(f); got != "stack: nginx:1.30@sha256:f65ed258206d" {
		t.Errorf("got %q", got)
	}
	f.Locations = []string{"/a", "/b"}
	if got := Where(f); got != "stack: nginx:1.30@sha256:f65ed258206d, /a (+1 more)" {
		t.Errorf("got %q", got)
	}
	g := model.Finding{Target: "web", Locations: []string{"/a", "/b"}}
	if got := Where(g); got != "web: /a (+1 more)" {
		t.Errorf("got %q", got)
	}
	if got := Where(model.Finding{Target: "web"}); got != "web" {
		t.Errorf("got %q", got)
	}
}

func TestRepinEscaping(t *testing.T) {
	img := "nginx:1.30@sha256:" + digest
	r := composeRun(cf("stack", img, "I", "1.0.1"))
	r.Targets[0].Images[0].Services = []string{"we|b"}
	if md := Markdown(r); !strings.Contains(md, `service we\|b`) {
		t.Errorf("markdown:\n%s", md)
	}
	r = composeRun(cf("stack", "bad\x1b[31m:1", "I", "1.0.1"))
	r.Targets[0].Images[0].Ref = "bad\x1b[31m:1"
	if text := Text(r, false); strings.Contains(text, "\x1b") {
		t.Errorf("text has control char: %q", text)
	}
}

func TestSkippedImagesListed(t *testing.T) {
	r := composeRun()
	r.Verdict = "pass"
	r.Targets[0].Images[1].Status = "skipped"
	r.Targets[0].Images[1].Note = "image needs a build"
	if md := Markdown(r); !strings.Contains(md, "- `stack (busybox:1)`: image needs a build") {
		t.Errorf("markdown:\n%s", md)
	}
	if text := Text(r, false); !strings.Contains(text, "Skipped stack (busybox:1): image needs a build") {
		t.Errorf("text:\n%s", text)
	}
	// a skipped compose target lists its images too
	r.Targets[1].Status, r.Targets[1].Note = "skipped", "all skipped"
	r.Targets[1].Images[0].Status, r.Targets[1].Images[0].Note = "skipped", "no network"
	text := Text(r, false)
	if !strings.Contains(text, "Skipped other: all skipped") || !strings.Contains(text, "no network") {
		t.Errorf("text:\n%s", text)
	}
}
