package scan

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/testutil"
)

const composeTarget = "targets:\n  - name: stack\n    compose: docker-compose.yml\n"

const twoImages = `services:
  web:
    image: nginx:1.27
  db:
    image: postgres:16
    platform: linux/arm64
`

func composeFiles(content string) map[string]string {
	return map[string]string{"docker-compose.yml": content}
}

func clean(purls ...string) output {
	return output{testutil.GrypeJSON(fresh), testutil.SBOMJSON(purls...), nil}
}

func warningCodes(r *model.Run) string { return codes(r.Warnings) }

func TestComposeScansEveryImage(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles(twoImages))
	nginx := testutil.Match{ID: "GHSA-1", Package: "openssl", Version: "3.0.0", FixState: "fixed", FixVersions: []string{"3.0.1"}}
	sc := &fakeScanner{outputs: map[string]output{
		"registry:nginx:1.27":  {testutil.GrypeJSON(fresh, nginx), testutil.SBOMJSON("pkg:apk/alpine/musl@1.2.5", "pkg:apk/alpine/openssl@3.0.0"), nil},
		"registry:postgres:16": clean("pkg:deb/debian/libc6@2.36"),
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 1 || len(r.Errors) != 0 {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
	if len(sc.calls) != 2 || sc.calls[0].Source != "registry:nginx:1.27" || sc.calls[1].Source != "registry:postgres:16" {
		t.Fatalf("calls = %+v", sc.calls)
	}
	if sc.calls[0].Platform != "" || sc.calls[1].Platform != "linux/arm64" {
		t.Errorf("platforms = %q, %q", sc.calls[0].Platform, sc.calls[1].Platform)
	}
	tr := r.Targets[0]
	if tr.Kind != "compose" || tr.Ref != "docker-compose.yml" || tr.Status != "scanned" || tr.Packages != 3 || len(tr.Raw) != 0 {
		t.Fatalf("target = %+v", tr)
	}
	if len(tr.Images) != 2 {
		t.Fatalf("images = %+v", tr.Images)
	}
	a, b := tr.Images[0], tr.Images[1]
	if a.Ref != "nginx:1.27" || !slices.Equal(a.Services, []string{"web"}) || a.Line != 3 || a.Status != "scanned" || a.Packages != 2 ||
		!slices.Equal(a.Raw, []string{"raw/stack+1.grype.json", "raw/stack+1.cdx.json"}) {
		t.Errorf("first image = %+v", a)
	}
	if b.Ref != "postgres:16" || b.Platform != "linux/arm64" || !slices.Equal(b.Services, []string{"db"}) || b.Line != 5 || b.Packages != 1 ||
		!slices.Equal(b.Raw, []string{"raw/stack+2.grype.json", "raw/stack+2.cdx.json"}) {
		t.Errorf("second image = %+v", b)
	}
	if len(r.Findings) != 1 || r.Findings[0].Image != "nginx:1.27" || r.Findings[0].Target != "stack" {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

func TestComposeUsesTheLocalDaemonImage(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles("services:\n  web:\n    image: nginx:1.27\n"))
	sc := &fakeScanner{outputs: map[string]output{"docker:nginx:1.27": clean("pkg:apk/alpine/musl@1.2.5")}}
	r := run(t, cfg, sc, func(o *Options) { o.HasImage = func(context.Context, string) bool { return true } })
	if r.ExitCode != 0 || len(sc.calls) != 1 || sc.calls[0].Source != "docker:nginx:1.27" {
		t.Fatalf("exit = %d, errors = %+v, calls = %+v", r.ExitCode, r.Errors, sc.calls)
	}
}

func TestComposeSkipsUnpullableImagesLocally(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles(twoImages))
	sc := &fakeScanner{outputs: map[string]output{
		"registry:nginx:1.27":  {err: errors.New("UNAUTHORIZED: authentication required\nmore")},
		"registry:postgres:16": clean("pkg:deb/debian/libc6@2.36"),
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 0 || r.Targets[0].Status != "scanned" {
		t.Fatalf("exit = %d, target = %+v, errors = %+v", r.ExitCode, r.Targets[0], r.Errors)
	}
	img := r.Targets[0].Images[0]
	if img.Status != "skipped" || !strings.Contains(img.Note, "`docker pull nginx:1.27`") || strings.Contains(img.Note, "more") || len(img.Raw) != 0 {
		t.Fatalf("image = %+v", img)
	}
	all := &fakeScanner{outputs: map[string]output{
		"registry:nginx:1.27":  {err: errors.New("denied")},
		"registry:postgres:16": {err: errors.New("denied")},
	}}
	r = run(t, cfg, all, nil)
	if r.ExitCode != 2 || r.Targets[0].Status != "skipped" || codes(r.Errors) != "ALL_SKIPPED" {
		t.Fatalf("exit = %d, target = %+v, errors = %s", r.ExitCode, r.Targets[0], codes(r.Errors))
	}
}

func TestComposeScanFailureInCIIsAnError(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles(twoImages))
	sc := &fakeScanner{outputs: map[string]output{
		"registry:nginx:1.27":  {err: errors.New("UNAUTHORIZED")},
		"registry:postgres:16": clean("pkg:deb/debian/libc6@2.36"),
	}}
	r := run(t, cfg, sc, func(o *Options) { o.CIMode = true })
	if r.ExitCode != 2 || codes(r.Errors) != "SCAN_FAILED" || r.Errors[0].Target != "stack" ||
		!strings.Contains(r.Errors[0].Message, "UNAUTHORIZED. If the image is in a private registry, log in to it before the shearbolt step.") || !strings.Contains(r.Errors[0].Message, "image nginx:1.27") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
	if r.Targets[0].Status != "error" || r.Targets[0].Images[0].Status != "error" || r.Targets[0].Images[1].Status != "scanned" || len(r.Targets[0].Images[0].Raw) != 0 {
		t.Fatalf("target = %+v", r.Targets[0])
	}
}

func TestComposeBuildOnlyFileHasNoImages(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles("services:\n  app:\n    build: .\n"))
	sc := &fakeScanner{}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "COMPOSE_NO_IMAGES" || len(sc.calls) != 0 {
		t.Fatalf("exit = %d, errors = %s, calls = %d", r.ExitCode, codes(r.Errors), len(sc.calls))
	}
	if warningCodes(r) != "COMPOSE_BUILD_NOT_SCANNED" || !strings.Contains(r.Warnings[0].Message, "app") {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
}

func TestComposeWarnsAboutServicesWithoutImage(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles("services:\n  web:\n    image: nginx:1.27\n  worker:\n    command: run\n  cron:\n    command: tick\n"))
	sc := &fakeScanner{outputs: map[string]output{"registry:nginx:1.27": clean("pkg:apk/alpine/musl@1.2.5")}}
	r := run(t, cfg, sc, nil)
	if warningCodes(r) != "COMPOSE_SERVICE_NO_IMAGE" || !strings.Contains(r.Warnings[0].Message, "worker, cron") {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
}

func TestComposeVariables(t *testing.T) {
	files := composeFiles("services:\n  api:\n    image: \"ghcr.io/acme/api:${API_TAG}\"\n  web:\n    image: nginx:1.27\n")
	cfg, _ := project(t, composeTarget, files)
	outputs := map[string]output{
		"registry:nginx:1.27":         clean("pkg:apk/alpine/musl@1.2.5"),
		"registry:ghcr.io/acme/api:7": clean("pkg:apk/alpine/musl@1.2.5"),
	}
	r := run(t, cfg, &fakeScanner{outputs: outputs}, func(o *Options) {
		o.Env = func(string) (string, bool) { return "", false }
	})
	if r.ExitCode != 2 || codes(r.Errors) != "COMPOSE_VARIABLE" || !strings.HasPrefix(r.Errors[0].Message, "docker-compose.yml:3: service api needs API_TAG") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
	if r.Targets[0].Status != "error" || len(r.Targets[0].Images) != 1 || r.Targets[0].Images[0].Ref != "nginx:1.27" || r.Targets[0].Images[0].Status != "scanned" {
		t.Fatalf("target = %+v", r.Targets[0])
	}
	r = run(t, cfg, &fakeScanner{outputs: outputs}, func(o *Options) {
		o.Env = func(k string) (string, bool) { return "7", k == "API_TAG" }
	})
	if r.ExitCode != 0 || len(r.Targets[0].Images) != 2 || r.Targets[0].Images[0].Ref != "ghcr.io/acme/api:7" {
		t.Fatalf("exit = %d, target = %+v, errors = %+v", r.ExitCode, r.Targets[0], r.Errors)
	}
}

func TestComposeFileLevelErrors(t *testing.T) {
	for name, tc := range map[string]struct{ content, code, prefix string }{
		"include": {"include:\n  - other.yml\nservices:\n  web:\n    image: nginx\n", "COMPOSE_UNSUPPORTED", "docker-compose.yml:1: "},
		"invalid": {"services: [\n", "COMPOSE_INVALID", "docker-compose.yml:"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := project(t, composeTarget, composeFiles(tc.content))
			sc := &fakeScanner{}
			r := run(t, cfg, sc, nil)
			if r.ExitCode != 2 || codes(r.Errors) != tc.code || !strings.HasPrefix(r.Errors[0].Message, tc.prefix) || len(sc.calls) != 0 {
				t.Fatalf("exit = %d, errors = %+v, calls = %d", r.ExitCode, r.Errors, len(sc.calls))
			}
		})
	}
	cfg, _ := project(t, composeTarget, nil)
	for _, ci := range []bool{false, true} {
		r := run(t, cfg, &fakeScanner{}, func(o *Options) { o.CIMode = ci })
		if r.ExitCode != 2 || codes(r.Errors) != "COMPOSE_UNREADABLE" || r.Errors[0].Message != "compose file docker-compose.yml cannot be read: no such file or directory" {
			t.Fatalf("ci = %v: exit = %d, errors = %+v", ci, r.ExitCode, r.Errors)
		}
	}
}

func TestComposeExcludesServices(t *testing.T) {
	cfg, _ := project(t, composeTarget+"    exclude: [db]\n", composeFiles(twoImages))
	sc := &fakeScanner{outputs: map[string]output{"registry:nginx:1.27": clean("pkg:apk/alpine/musl@1.2.5")}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 0 || len(sc.calls) != 1 || !slices.Equal(r.Targets[0].Exclude, []string{"db"}) {
		t.Fatalf("exit = %d, calls = %d, target = %+v, errors = %+v", r.ExitCode, len(sc.calls), r.Targets[0], r.Errors)
	}
}

func TestComposeImageWithoutPackagesIsAnError(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles(twoImages))
	sc := &fakeScanner{outputs: map[string]output{
		"registry:nginx:1.27":  clean(),
		"registry:postgres:16": clean("pkg:deb/debian/libc6@2.36"),
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "NO_PACKAGES" || !strings.Contains(r.Errors[0].Message, "nginx:1.27") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestComposeFindingsAcceptTargetExceptions(t *testing.T) {
	yaml := composeTarget + "exceptions:\n  - id: GHSA-1\n    package: openssl@3.0.0\n    target: stack\n    reason: test\n    approvedBy: me\n    expires: 2026-12-01\n"
	cfg, _ := project(t, yaml, composeFiles("services:\n  web:\n    image: nginx:1.27\n"))
	m := testutil.Match{ID: "GHSA-1", Package: "openssl", Version: "3.0.0", FixState: "fixed", FixVersions: []string{"3.0.1"}}
	sc := &fakeScanner{outputs: map[string]output{"registry:nginx:1.27": {testutil.GrypeJSON(fresh, m), testutil.SBOMJSON("pkg:apk/alpine/openssl@3.0.0"), nil}}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 0 || len(r.Findings) != 1 || r.Findings[0].Decision.Action != model.Excepted || r.Findings[0].Image != "nginx:1.27" {
		t.Fatalf("exit = %d, findings = %+v, errors = %+v", r.ExitCode, r.Findings, r.Errors)
	}
}

func TestComposeSkipDoesNotHideAVariableProblem(t *testing.T) {
	files := composeFiles("services:\n  api:\n    image: \"ghcr.io/acme/api:${API_TAG}\"\n  web:\n    image: nginx:1.27\n")
	cfg, _ := project(t, composeTarget, files)
	sc := &fakeScanner{outputs: map[string]output{"registry:nginx:1.27": {err: errors.New("denied")}}}
	r := run(t, cfg, sc, func(o *Options) { o.Env = func(string) (string, bool) { return "", false } })
	if r.ExitCode != 2 || codes(r.Errors) != "COMPOSE_VARIABLE" || r.Targets[0].Status != "error" || r.Targets[0].Images[0].Status != "skipped" {
		t.Fatalf("exit = %d, errors = %s, target = %+v", r.ExitCode, codes(r.Errors), r.Targets[0])
	}
}

func TestComposeDaemonImageFailureIsNotSkipped(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles("services:\n  web:\n    image: nginx:1.27\n"))
	sc := &fakeScanner{outputs: map[string]output{"docker:nginx:1.27": {err: errors.New("layer unreadable")}}}
	r := run(t, cfg, sc, func(o *Options) { o.HasImage = func(context.Context, string) bool { return true } })
	if r.ExitCode != 2 || codes(r.Errors) != "SCAN_FAILED" || r.Targets[0].Images[0].Status != "error" {
		t.Fatalf("exit = %d, errors = %s, target = %+v", r.ExitCode, codes(r.Errors), r.Targets[0])
	}
	if strings.Contains(r.Errors[0].Message, "private registry") {
		t.Errorf("message = %s", r.Errors[0].Message)
	}
}

func TestComposeCancellationSkipsNothingAndAddsNoLoginHint(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles("services:\n  a:\n    image: app:local\n  b:\n    image: nginx:1.27\n"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := &fakeScanner{cancel: cancel, outputs: map[string]output{"registry:app:local": {err: errors.New("context canceled")}}}
	o := Options{Config: cfg, OutputDir: filepath.Join(t.TempDir(), "evidence"), Scanner: sc, Now: func() time.Time { return now },
		HasImage: func(context.Context, string) bool { return false }}
	r := Run(ctx, o)
	if r.ExitCode != 2 || !strings.Contains(codes(r.Errors), "CANCELLED") {
		t.Fatalf("exit = %d, errors = %s", r.ExitCode, codes(r.Errors))
	}
	for _, img := range r.Targets[0].Images {
		if img.Status != "error" || len(img.Raw) != 0 {
			t.Errorf("image = %+v", img)
		}
	}
	for _, e := range r.Errors {
		if strings.Contains(e.Message, "private registry") {
			t.Errorf("hint after cancellation: %s", e.Message)
		}
	}
}

func TestComposeOnlyVariableProblemsIsNotNoImages(t *testing.T) {
	cfg, _ := project(t, composeTarget, composeFiles("services:\n  api:\n    image: \"ghcr.io/acme/api:${API_TAG}\"\n"))
	r := run(t, cfg, &fakeScanner{}, func(o *Options) { o.Env = func(string) (string, bool) { return "", false } })
	if r.ExitCode != 2 || codes(r.Errors) != "COMPOSE_VARIABLE" {
		t.Fatalf("exit = %d, errors = %s", r.ExitCode, codes(r.Errors))
	}
}
