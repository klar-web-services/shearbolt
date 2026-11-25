package setup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
)

// gated is a golden fixture after setup ran on it: before/ with after/ on top.
func gated(t *testing.T, fixture string) string {
	t.Helper()
	root := t.TempDir()
	copyTree(t, filepath.Join(goldenDir, fixture, "before"), root)
	copyTree(t, filepath.Join(goldenDir, fixture, "after"), root)
	return root
}

func TestZosiaOnGitLabAndBitbucketChangesNoFile(t *testing.T) {
	for fixture, want := range map[string]string{"gitlab": "masked CI/CD variable ZOSIA_SECRET", "bitbucket": "secured repository variable ZOSIA_SECRET"} {
		t.Run(fixture, func(t *testing.T) {
			root := gated(t, fixture)
			p := Zosia(root, true, false)
			if p.Error != nil || len(p.Changes) != 0 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_SECRET"}) || !strings.Contains(p.Notes[0].Message, want) {
				t.Errorf("enable: error %+v, %d changes, notes %+v", p.Error, len(p.Changes), p.Notes)
			}
			p = Zosia(root, false, false)
			if p.Error != nil || len(p.Changes) != 0 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_VARIABLE", "ZOSIA_SECRET_STAYS"}) {
				t.Errorf("disable: error %+v, %d changes, notes %+v", p.Error, len(p.Changes), p.Notes)
			}
		})
	}
}

func TestZosiaWithoutAGate(t *testing.T) {
	root := t.TempDir()
	copyTree(t, filepath.Join(goldenDir, "gitlab", "before"), root)
	if p := Zosia(root, true, false); p.Error == nil || p.Error.Code != "NO_GATE" || !strings.Contains(p.Error.Message, "shearbolt setup") {
		t.Errorf("without a gate: %+v", p.Error)
	}
	if p := Zosia(t.TempDir(), true, false); p.Error == nil || p.Error.Code != "NO_CI" {
		t.Errorf("without CI: %+v", p.Error)
	}
}

// rewrite replaces old by new in a file of root, which must hold old.
func rewrite(t *testing.T, root, rel, old, new string) {
	t.Helper()
	path := filepath.Join(root, rel)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.ReplaceAll(string(src), old, new)
	if out == string(src) {
		t.Fatalf("%s does not hold %q", rel, old)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pinned is the gated GitLab fixture with its Shearbolt release moved to version.
func pinned(t *testing.T, version string) string {
	t.Helper()
	root := gated(t, "gitlab")
	rewrite(t, root, ".gitlab-ci.yml", "SHEARBOLT_VERSION=1.2.3", "SHEARBOLT_VERSION="+version)
	return root
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Releases 1.11.0 and 1.12.0 needed `policy: true` in the zosia block. Since 1.13.0 a CI scan with
// the secret takes its policy from Zosia anyway: enable writes no such line, and disable removes
// the ones that are there.
func TestZosiaPolicyLineIsNoLongerWrittenAndStillRemoved(t *testing.T) {
	root := pinned(t, "1.13.0")
	before := read(t, root, "shearbolt.yaml")
	on := Zosia(root, true, true)
	if on.Error != nil || len(on.Changes) != 0 || !slices.Equal(codes(on.Notes), []string{"ZOSIA_SECRET", "ZOSIA_POLICY_NOT_NEEDED"}) {
		t.Fatalf("enable --policy: error %+v, changes %+v, notes %+v", on.Error, on.Changes, on.Notes)
	}
	if p := Zosia(root, true, false); p.Error != nil || len(p.Changes) != 0 || slices.Contains(codes(p.Notes), "ZOSIA_POLICY_NOT_NEEDED") {
		t.Fatalf("enable: error %+v, %d changes, notes %+v", p.Error, len(p.Changes), p.Notes)
	}

	// A file from an older release: disable --policy removes the line and says nothing about the secret.
	if err := os.WriteFile(filepath.Join(root, "shearbolt.yaml"), []byte(before+"zosia:\n  policy: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "shearbolt.yaml"), func(string) (string, bool) { return "", false }, time.Now())
	if err != nil || cfg.Policy != config.DefaultPolicy() {
		t.Fatalf("a file with the old line: %v, %+v", err, cfg)
	}
	off := Zosia(root, false, true)
	if off.Error != nil || len(off.Changes) != 1 || off.Changes[0].Content != before || !slices.Equal(codes(off.Notes), []string{"ZOSIA_POLICY_REMOVED"}) {
		t.Fatalf("disable --policy: error %+v, changes %+v, notes %+v", off.Error, off.Changes, off.Notes)
	}
	// Plain disable removes it as well, with what enable added.
	all := Zosia(root, false, false)
	if all.Error != nil || len(all.Changes) != 1 || all.Changes[0].Content != before ||
		!slices.Equal(codes(all.Notes), []string{"ZOSIA_VARIABLE", "ZOSIA_POLICY_REMOVED", "ZOSIA_SECRET_STAYS"}) {
		t.Fatalf("disable: error %+v, changes %+v, notes %+v", all.Error, all.Changes, all.Notes)
	}
	if err := Write(root, off); err != nil {
		t.Fatal(err)
	}
	if p := Zosia(root, false, true); p.Error != nil || len(p.Changes) != 0 || len(p.Notes) != 0 {
		t.Errorf("disable --policy with nothing to remove: error %+v, changes %+v, notes %+v", p.Error, p.Changes, p.Notes)
	}
}

// A gate whose shearbolt.yaml is not certain is named to who asked about the line, and only then.
func TestZosiaPolicyLineWhenAGatesConfigIsNotCertain(t *testing.T) {
	root := gated(t, "go-github")
	rewrite(t, root, ".github/workflows/release.yml", "uses: klar-web-services/shearbolt@v1", "uses: klar-web-services/shearbolt@v1\n        with:\n          config: ${{ matrix.config }}")
	if p := Zosia(root, false, true); p.Error != nil || len(p.Changes) != 0 || !slices.Contains(codes(p.Notes), "ZOSIA_POLICY_BY_HAND") {
		t.Fatalf("disable --policy: error %+v, changes %+v, notes %+v", p.Error, p.Changes, p.Notes)
	}
	if p := Zosia(root, false, false); slices.Contains(codes(p.Notes), "ZOSIA_POLICY_BY_HAND") {
		t.Errorf("disable: notes %+v", p.Notes)
	}
}

func TestZosiaDisableSaysWhenABlockStays(t *testing.T) {
	root := pinned(t, "1.11.0")
	// An anchor on the block: removing the entry would break whatever refers to it.
	path := filepath.Join(root, "shearbolt.yaml")
	if err := os.WriteFile(path, []byte(read(t, root, "shearbolt.yaml")+"zosia: &z\n  policy: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []bool{false, true} {
		off := Zosia(root, false, policy)
		if off.Error != nil || len(off.Changes) != 0 || !slices.Contains(codes(off.Notes), "ZOSIA_POLICY_KEPT") {
			t.Fatalf("disable (--policy %v): error %+v, changes %+v, notes %+v", policy, off.Error, off.Changes, off.Notes)
		}
	}
}

func TestInstallPinIsTheOldestPinnedRelease(t *testing.T) {
	for src, want := range map[string]string{
		"":                                "",
		"- curl … | sh":                   "",
		"| SHEARBOLT_VERSION=1.10.0 sh":   "1.10.0",
		"SHEARBOLT_VERSION=\"1.11.0\" sh": "1.11.0",
		"SHEARBOLT_VERSION=1.11.0\nSHEARBOLT_VERSION=1.9.2\nSHEARBOLT_VERSION=2.0.0": "1.9.2",
		"SHEARBOLT_VERSION=$VERSION":                 "",
		"variables:\n  SHEARBOLT_VERSION: \"1.9.0\"": "1.9.0",
		"env:\n  SHEARBOLT_VERSION : 1.10.3":         "1.10.3",
	} {
		if got := installPin([]byte(src)); got != want {
			t.Errorf("%q: %q, want %q", src, got, want)
		}
	}
	for v, want := range map[string]bool{"": true, "1.12.0": true, "1.13.3": true, "2.0.0": true, "1.11.0": false, "1.9.9": false, "0.9.0": false} {
		if knowsZosiaSend(v) != want {
			t.Errorf("knowsZosiaSend(%q) = %v", v, !want)
		}
	}
}

func TestSetZosiaKey(t *testing.T) {
	const base = "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\n"
	for name, c := range map[string]struct {
		src, key, value string
		want, state     string
	}{
		"add":                           {base, "policy", "true", base + "zosia:\n  policy: true\n", "added"},
		"add, no newline at the end":    {strings.TrimSuffix(base, "\n"), "policy", "true", base + "zosia:\n  policy: true", "added"},
		"add, Windows line ends":        {strings.ReplaceAll(base, "\n", "\r\n"), "policy", "true", strings.ReplaceAll(base+"zosia:\n  policy: true\n", "\n", "\r\n"), "added"},
		"add before a trailing comment": {base + "# the end\n", "policy", "true", base + "zosia:\n  policy: true\n# the end\n", "added"},
		"switch on":                     {base + "zosia:\n  policy: false # not yet\n", "policy", "true", base + "zosia:\n  policy: true # not yet\n", "added"},
		"already":                       {base + "zosia:\n  policy: true\n", "policy", "true", base + "zosia:\n  policy: true\n", "already"},
		"remove":                        {base + "zosia:\n  policy: true\n", "policy", "", base, "removed"},
		"remove in the middle":          {"version: 1\nzosia:\n  policy: true\nproject: p\n", "policy", "", "version: 1\nproject: p\n", "removed"},
		"remove one that is off":        {base + "zosia:\n  policy: false\n", "policy", "", base, "removed"},
		"remove a flow block":           {base + "zosia: {policy: true}\n", "policy", "", base, "removed"},
		"nothing to remove":             {base, "policy", "", base, ""},

		"send: add":                       {base, "send", "false", base + "zosia:\n  send: false\n", "added"},
		"send: add below policy":          {base + "zosia:\n  policy: true\n", "send", "false", base + "zosia:\n  policy: true\n  send: false\n", "added"},
		"policy: add below send":          {base + "zosia:\n  send: false\n", "policy", "true", base + "zosia:\n  send: false\n  policy: true\n", "added"},
		"send: switch off":                {base + "zosia:\n  send: true\n", "send", "false", base + "zosia:\n  send: false\n", "added"},
		"send: already":                   {base + "zosia:\n  send: false\n", "send", "false", base + "zosia:\n  send: false\n", "already"},
		"send: remove, the policy stays":  {base + "zosia:\n  policy: true\n  send: false\n", "send", "", base + "zosia:\n  policy: true\n", "removed"},
		"policy: remove, the send stays":  {base + "zosia:\n  policy: true\n  send: false\n", "policy", "", base + "zosia:\n  send: false\n", "removed"},
		"send: remove the block's last":   {base + "zosia:\n  send: false\n", "send", "", base, "removed"},
		"send: none in a block to remove": {base + "zosia:\n  policy: true\n", "send", "", base + "zosia:\n  policy: true\n", ""},
	} {
		got, state, err := setZosiaKey([]byte(c.src), c.key, c.value)
		if err != nil || string(got) != c.want || state != c.state {
			t.Errorf("%s: %q, state %q, error %v; want %q, %q", name, got, state, err, c.want, c.state)
		}
	}
	for name, c := range map[string]struct{ src, key, value string }{
		"an empty flow block":             {base + "zosia: {}\n", "policy", "true"},
		"a flow block that is off":        {base + "zosia: {policy: false}\n", "policy", "true"},
		"a flow block with the other key": {base + "zosia: {policy: true}\n", "send", "false"},
		"one key of a flow block of two":  {base + "zosia: {policy: true, send: false}\n", "send", ""},
		"no mapping":                      {"- a\n", "policy", "true"},
	} {
		if got, _, err := setZosiaKey([]byte(c.src), c.key, c.value); err == nil {
			t.Errorf("%s: no error, got %q", name, got)
		}
	}
}

// sending is the gated monorepo fixture after `zosia enable`: its three gates hand over the secret.
func sending(t *testing.T) string {
	t.Helper()
	root := gated(t, "monorepo")
	if err := Write(root, Zosia(root, true, false)); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestZosiaProjectsAreTheConfigsTheGatesRead(t *testing.T) {
	root := gated(t, "monorepo")
	// Before enable no gate hands over the secret, so no project sends.
	before, _, err := ZosiaProjects(root)
	if err != nil || len(before) != 3 || before[0].Sends || before[1].Sends || before[2].Sends {
		t.Fatalf("before enable: %+v, %v", before, err)
	}
	root = sending(t)
	got, notes, err := ZosiaProjects(root)
	want := []ZosiaProject{
		{Config: "apps/api/shearbolt.yaml", Name: "api", Sends: true},
		{Config: "apps/web/shearbolt.yaml", Name: "web", Sends: true},
		{Config: "crates/cli/shearbolt.yaml", Name: "cli", Sends: true},
	}
	if err != nil || !slices.Equal(got, want) || len(notes) != 0 {
		t.Fatalf("projects %+v, notes %+v, error %v", got, notes, err)
	}
	if _, _, err := ZosiaProjects(t.TempDir()); err == nil {
		t.Error("no error without CI")
	}
}

func TestZosiaSendSwitchesProjectsOffAndOn(t *testing.T) {
	root := sending(t)
	off := ZosiaSend(root, []string{"apps/api/shearbolt.yaml"})
	if off.Error != nil || len(off.Changes) != 2 || !slices.Equal(codes(off.Notes), []string{"ZOSIA_SEND_OFF", "ZOSIA_SEND_OFF"}) {
		t.Fatalf("error %+v, changes %+v, notes %+v", off.Error, off.Changes, off.Notes)
	}
	for i, path := range []string{"apps/web/shearbolt.yaml", "crates/cli/shearbolt.yaml"} {
		c := off.Changes[i]
		if c.Path != path || !strings.HasSuffix(c.Content, "\nzosia:\n  send: false\n") || !strings.Contains(c.Diff, "+zosia:\n+  send: false\n") {
			t.Errorf("%s would become:\n%s", c.Path, c.Content)
		}
	}
	if err := Write(root, off); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "apps/web/shearbolt.yaml"), func(string) (string, bool) { return "", false }, time.Now())
	if err != nil || cfg.Zosia.Sends() {
		t.Fatalf("the edited file: %v, %+v", err, cfg)
	}
	got, _, _ := ZosiaProjects(root)
	if len(got) != 3 || !got[0].Sends || got[1].Sends || !got[1].Off || got[2].Sends || !got[2].Off {
		t.Fatalf("projects after: %+v", got)
	}
	if again := ZosiaSend(root, []string{"apps/api/shearbolt.yaml"}); again.Error != nil || len(again.Changes) != 0 || len(again.Notes) != 0 {
		t.Errorf("the same again: error %+v, changes %+v, notes %+v", again.Error, again.Changes, again.Notes)
	}
	// Another choice takes the line from a project that sends again, and the block with its last line.
	swap := ZosiaSend(root, []string{"apps/web/shearbolt.yaml", "crates/cli/shearbolt.yaml"})
	if swap.Error != nil || len(swap.Changes) != 3 || !slices.Equal(codes(swap.Notes), []string{"ZOSIA_SEND_OFF", "ZOSIA_SEND_ON", "ZOSIA_SEND_ON"}) {
		t.Fatalf("error %+v, changes %+v, notes %+v", swap.Error, swap.Changes, swap.Notes)
	}
	if strings.Contains(swap.Changes[1].Content, "zosia") || strings.Contains(swap.Changes[2].Content, "zosia") {
		t.Errorf("a project that sends again keeps a zosia block:\n%s", swap.Changes[1].Content)
	}
	// What is no project of the repository is an error, and nothing is changed for it.
	if p := ZosiaSend(root, []string{"apps/api/shearbolt.yaml", "docs/shearbolt.yaml"}); p.Error == nil || p.Error.Code != "NO_PROJECT" || len(p.Changes) != 0 {
		t.Errorf("an unknown path: %+v, %d changes", p.Error, len(p.Changes))
	}
}

func TestZosiaSendLeavesAProjectThatCannotBeSwitchedOff(t *testing.T) {
	for version, fixed := range map[string]bool{"1.11.0": true, "1.12.0": false} {
		root := pinned(t, version)
		got, _, err := ZosiaProjects(root)
		if err != nil || len(got) != 1 || !got[0].Sends || (got[0].Fixed != "") != fixed {
			t.Fatalf("%s: projects %+v, %v", version, got, err)
		}
		p := ZosiaSend(root, nil)
		if fixed && (p.Error != nil || len(p.Changes) != 0 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_SEND_GATE_TOO_OLD"}) || !strings.Contains(p.Notes[0].Message, "1.12.0")) {
			t.Errorf("%s: error %+v, changes %+v, notes %+v", version, p.Error, p.Changes, p.Notes)
		}
		if !fixed && (p.Error != nil || len(p.Changes) != 1 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_SEND_OFF"})) {
			t.Errorf("%s: error %+v, changes %+v, notes %+v", version, p.Error, p.Changes, p.Notes)
		}
	}
	// A release that cannot send needs no place, and one before 1.4.0 cannot. Nothing is said of it:
	// it is not switched off, and it sends nothing either.
	old := pinned(t, "1.3.0")
	if got, _, _ := ZosiaProjects(old); len(got) != 1 || got[0].Sends {
		t.Errorf("a gate from before 1.4.0: %+v", got)
	}
	if p := ZosiaSend(old, nil); p.Error != nil || len(p.Changes) != 0 || len(p.Notes) != 0 {
		t.Errorf("a gate from before 1.4.0, switched off: error %+v, changes %+v, notes %+v", p.Error, p.Changes, p.Notes)
	}
	// The action at a commit: its release cannot be told, so the line is not risked.
	root := sending(t)
	rewrite(t, root, ".github/workflows/ci.yml", "klar-web-services/shearbolt@v1", "klar-web-services/shearbolt@"+strings.Repeat("a1b2", 10))
	if got, _, _ := ZosiaProjects(root); len(got) != 3 || got[0].Fixed == "" {
		t.Errorf("the action at a commit: %+v", got)
	}
	// A block the editor refuses.
	root = sending(t)
	rewrite(t, root, "apps/web/shearbolt.yaml", "project: web\n", "project: web\nzosia: {policy: true}\n")
	if got, _, _ := ZosiaProjects(root); len(got) != 3 || got[1].Fixed == "" || got[0].Fixed != "" {
		t.Errorf("a flow block: %+v", got)
	}
}

func TestZosiaDisableKeepsSendAndRemovesAnOldPolicyLine(t *testing.T) {
	root := sending(t)
	if err := Write(root, ZosiaSend(root, []string{"apps/api/shearbolt.yaml", "apps/web/shearbolt.yaml"})); err != nil {
		t.Fatal(err)
	}
	// As a release before 1.13.0 left it: both lines in one block.
	rewrite(t, root, "crates/cli/shearbolt.yaml", "zosia:\n  send: false\n", "zosia:\n  send: false\n  policy: true\n")
	for _, policy := range []bool{true, false} {
		off := Zosia(root, false, policy)
		if off.Error != nil {
			t.Fatal(off.Error)
		}
		var cli string
		for _, c := range off.Changes {
			if c.Path == "crates/cli/shearbolt.yaml" {
				cli = c.Content
			}
		}
		if !strings.HasSuffix(cli, "\nzosia:\n  send: false\n") {
			t.Errorf("disable (--policy %v): crates/cli/shearbolt.yaml would become:\n%s", policy, cli)
		}
		// Plain disable says that the line stays; disable --policy was not asked about sending.
		if kept := slices.Contains(codes(off.Notes), "ZOSIA_SEND_KEPT"); kept == policy {
			t.Errorf("disable (--policy %v): notes %+v", policy, off.Notes)
		}
	}
}
