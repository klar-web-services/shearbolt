package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/grype"
	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/testutil"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
var fresh = now.Add(-2 * time.Hour)

type output struct {
	json, sbom string
	err        error
}

type fakeScanner struct {
	outputs map[string]output // keyed by Request.Source
	calls   []grype.Request
	cancel  context.CancelFunc // called when the source "registry:app:local" is scanned
}

func (f *fakeScanner) Scan(_ context.Context, req grype.Request) error {
	f.calls = append(f.calls, req)
	if f.cancel != nil && req.Source == "registry:app:local" {
		f.cancel()
	}
	out, ok := f.outputs[req.Source]
	if !ok {
		return fmt.Errorf("unexpected source %s", req.Source)
	}
	if out.err != nil {
		return out.err
	}
	if err := os.WriteFile(req.RawJSON, []byte(out.json), 0o644); err != nil {
		return err
	}
	return os.WriteFile(req.SBOM, []byte(out.sbom), 0o644)
}

// project writes files (relative path -> content) and a shearbolt.yaml, then loads it.
func project(t *testing.T, yaml string, files map[string]string) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "shearbolt.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nproject: demo\n"+yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, func(string) (string, bool) { return "", false }, now)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, root
}

func run(t *testing.T, cfg *config.Config, sc *fakeScanner, mod func(*Options)) *model.Run {
	t.Helper()
	o := Options{Config: cfg, OutputDir: filepath.Join(t.TempDir(), "evidence"), Scanner: sc, Now: func() time.Time { return now },
		HasImage: func(context.Context, string) bool { return false }}
	if mod != nil {
		mod(&o)
	}
	return Run(context.Background(), o)
}

func codes(ds []model.Diagnostic) string {
	var c []string
	for _, d := range ds {
		c = append(c, d.Code)
	}
	return strings.Join(c, ",")
}

const sourceTarget = "targets:\n  - name: repo\n    source: .\n"

var lock = map[string]string{"package-lock.json": "{}"}

func TestCleanSourcePassesAndWritesEvidence(t *testing.T) {
	cfg, root := project(t, sourceTarget, lock)
	sc := &fakeScanner{outputs: map[string]output{"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMJSON("pkg:npm/a@1.0.0"), nil}}}
	r := run(t, cfg, sc, nil)
	if r.Verdict != "pass" || r.ExitCode != 0 || len(r.Errors) != 0 {
		t.Fatalf("verdict = %s, errors = %+v", r.Verdict, r.Errors)
	}
	for _, f := range []string{"run.json", "summary.md", "raw/repo.grype.json", "raw/repo.cdx.json"} {
		if _, err := os.Stat(filepath.Join(r.EvidenceDir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if len(sc.calls[0].Exclude) == 0 {
		t.Error("source scans must pass default excludes to Grype")
	}
}

func TestFixableFindingBlocks(t *testing.T) {
	cfg, root := project(t, sourceTarget, lock)
	doc := testutil.GrypeJSON(fresh, testutil.Match{ID: "GHSA-1", Package: "lodash", Version: "4.17.20", FixState: "fixed", FixVersions: []string{"4.17.21"}})
	r := run(t, cfg, &fakeScanner{outputs: map[string]output{"dir:" + root: {doc, testutil.SBOMJSON("pkg:npm/lodash@4.17.20"), nil}}}, nil)
	if r.Verdict != "block" || r.ExitCode != 1 || r.Findings[0].Target != "repo" {
		t.Fatalf("verdict = %s, findings = %+v", r.Verdict, r.Findings)
	}
}

func TestSourceWithoutLockfileIsAnError(t *testing.T) {
	cfg, _ := project(t, sourceTarget, map[string]string{"README.md": "hi"})
	sc := &fakeScanner{}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "NO_LOCKFILE" || len(sc.calls) != 0 {
		t.Fatalf("exit = %d, errors = %s, calls = %d", r.ExitCode, codes(r.Errors), len(sc.calls))
	}
}

// Packages from other files (here a workflow `uses:` line) must not hide an unread lockfile.
func TestUnreadLockfileIsAnErrorEvenWithOtherPackages(t *testing.T) {
	cfg, root := project(t, sourceTarget, lock)
	sbom := `{"bomFormat":"CycloneDX","components":[{"type":"library","name":"actions/checkout","purl":"pkg:github/actions/checkout@v4","properties":[{"name":"syft:location:0:path","value":"/.github/workflows/ci.yml"}]}]}`
	sc := &fakeScanner{outputs: map[string]output{"dir:" + root: {testutil.GrypeJSON(fresh), sbom, nil}}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" || !strings.Contains(r.Errors[0].Message, "package-lock.json") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestMonorepoLockfileWithoutPackagesIsAnError(t *testing.T) {
	cfg, root := project(t, sourceTarget, map[string]string{"package-lock.json": "{}", "web/package-lock.json": "{ broken"})
	sc := &fakeScanner{outputs: map[string]output{"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMJSON("pkg:npm/a@1.0.0"), nil}}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" || !strings.Contains(r.Errors[0].Message, "web/package-lock.json") || strings.Contains(r.Errors[0].Message, " package-lock.json") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func devOnlyRun(t *testing.T, files map[string]string) *model.Run {
	t.Helper()
	cfg, root := project(t, sourceTarget, files)
	outputs := map[string]output{"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMJSON(), nil}}
	for rel := range files {
		if dir := filepath.Dir(rel); dir != "." && (filepath.Base(rel) == "yarn.lock" || filepath.Base(rel) == "bun.lock") {
			outputs["dir:"+filepath.Join(root, dir)] = output{testutil.GrypeJSON(fresh), testutil.SBOMJSON(), nil} // nested project sub-scan, no packages
		}
	}
	if rel := "yarn.lock"; files[rel] != "" && yarnWorkspaceLockfile(filepath.Join(root, rel)) {
		outputs["file:"+filepath.Join(root, rel)] = output{testutil.GrypeJSON(fresh), testutil.SBOMJSON(), nil} // workspace file scan, no packages
	}
	return run(t, cfg, &fakeScanner{outputs: outputs}, nil)
}

func TestDevOnlyYarnLockfilePassesWithWarning(t *testing.T) {
	r := devOnlyRun(t, map[string]string{"yarn.lock": "# yarn", "package.json": `{"devDependencies":{"a":"1.0.0"}}`})
	if r.ExitCode != 0 || len(r.Errors) != 0 || codes(r.Warnings) != "DEV_ONLY_LOCKFILE" || !strings.Contains(r.Warnings[0].Message, "yarn.lock") {
		t.Fatalf("exit = %d, errors = %+v, warnings = %+v", r.ExitCode, r.Errors, r.Warnings)
	}
}

func TestYarnLockfileWithDependenciesStaysUnread(t *testing.T) {
	r := devOnlyRun(t, map[string]string{"yarn.lock": "# yarn", "package.json": `{"dependencies":{"x":"1.0.0"},"devDependencies":{"a":"1.0.0"}}`})
	if r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" || len(r.Warnings) != 0 {
		t.Fatalf("exit = %d, errors = %+v, warnings = %+v", r.ExitCode, r.Errors, r.Warnings)
	}
}

func TestYarnWorkspaceRootStaysUnread(t *testing.T) {
	r := devOnlyRun(t, map[string]string{"yarn.lock": "# yarn", "package.json": `{"workspaces":["packages/*"],"devDependencies":{"a":"1.0.0"}}`})
	if r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestBunLockfileWithoutPackageJSONStaysUnread(t *testing.T) {
	r := devOnlyRun(t, map[string]string{"bun.lock": "{}"})
	if r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

// The package.json must come from the lockfile's own folder, not the source root.
func TestDevOnlyCheckReadsTheLockfilesOwnFolder(t *testing.T) {
	r := devOnlyRun(t, map[string]string{
		"package.json":     `{"devDependencies":{"a":"1.0.0"}}`,
		"app/package.json": `{"dependencies":{"x":"1.0.0"}}`,
		"app/yarn.lock":    "# yarn",
	})
	if r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" || !strings.Contains(r.Errors[0].Message, "app/yarn.lock") || len(r.Warnings) != 0 {
		t.Fatalf("exit = %d, errors = %+v, warnings = %+v", r.ExitCode, r.Errors, r.Warnings)
	}
}

// The exception covers only yarn.lock and bun.lock, which Grype reads together with package.json.
func TestDevOnlyExceptionIsLimitedToYarnAndBun(t *testing.T) {
	r := devOnlyRun(t, map[string]string{"pnpm-lock.yaml": "", "package.json": `{"devDependencies":{"a":"1.0.0"}}`})
	if r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" || len(r.Warnings) != 0 {
		t.Fatalf("exit = %d, errors = %+v, warnings = %+v", r.ExitCode, r.Errors, r.Warnings)
	}
}

func TestMissingFileSkippedLocallyErrorInCI(t *testing.T) {
	yaml := "targets:\n  - name: cli\n    file: bin/cli\n  - name: repo\n    source: .\n"
	cfg, root := project(t, yaml, lock)
	outputs := map[string]output{"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMJSON("pkg:npm/a@1.0.0"), nil}}
	local := run(t, cfg, &fakeScanner{outputs: outputs}, nil)
	if local.ExitCode != 0 || local.Targets[0].Status != "skipped" || !strings.Contains(local.Targets[0].Note, "Build it first") {
		t.Fatalf("local: exit = %d, targets = %+v", local.ExitCode, local.Targets)
	}
	ci := run(t, cfg, &fakeScanner{outputs: outputs}, func(o *Options) { o.CIMode = true })
	if ci.ExitCode != 2 || codes(ci.Errors) != "MISSING_TARGET" {
		t.Fatalf("ci: exit = %d, errors = %s", ci.ExitCode, codes(ci.Errors))
	}
}

func TestAllTargetsSkippedIsAnError(t *testing.T) {
	cfg, _ := project(t, "targets:\n  - name: cli\n    file: bin/cli\n", nil)
	r := run(t, cfg, &fakeScanner{}, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "ALL_SKIPPED" {
		t.Fatalf("exit = %d, errors = %s", r.ExitCode, codes(r.Errors))
	}
}

func TestImageSourceSelection(t *testing.T) {
	yaml := "targets:\n  - name: web\n    image: app:local\n  - name: repo\n    source: .\n"
	cfg, root := project(t, yaml, lock)
	outputs := map[string]output{
		"dir:" + root:        {testutil.GrypeJSON(fresh), testutil.SBOMJSON("pkg:npm/a@1.0.0"), nil},
		"docker:app:local":   {testutil.GrypeJSON(fresh), testutil.SBOMJSON("pkg:apk/alpine/musl@1.2.5"), nil},
		"registry:app:local": {err: errors.New("UNAUTHORIZED: authentication required")},
	}
	present := run(t, cfg, &fakeScanner{outputs: outputs}, func(o *Options) { o.HasImage = func(context.Context, string) bool { return true } })
	if present.ExitCode != 0 || present.Targets[0].Status != "scanned" {
		t.Fatalf("daemon image: exit = %d, targets = %+v, errors = %+v", present.ExitCode, present.Targets, present.Errors)
	}
	broken := map[string]output{"dir:" + root: outputs["dir:"+root], "docker:app:local": {err: errors.New("Grype failed: layer unreadable")}}
	crash := run(t, cfg, &fakeScanner{outputs: broken}, func(o *Options) { o.HasImage = func(context.Context, string) bool { return true } })
	if crash.ExitCode != 2 || codes(crash.Errors) != "SCAN_FAILED" {
		t.Fatalf("a Grype failure on a local image must be an error, not a skip: exit = %d, targets = %+v", crash.ExitCode, crash.Targets)
	}
	local := run(t, cfg, &fakeScanner{outputs: outputs}, nil)
	if local.ExitCode != 0 || local.Targets[0].Status != "skipped" || !strings.Contains(local.Targets[0].Note, "docker build -t app:local") {
		t.Fatalf("unavailable image locally: exit = %d, targets = %+v", local.ExitCode, local.Targets)
	}
	ci := run(t, cfg, &fakeScanner{outputs: outputs}, func(o *Options) { o.CIMode = true })
	if ci.ExitCode != 2 || codes(ci.Errors) != "SCAN_FAILED" {
		t.Fatalf("unavailable image in CI: exit = %d, errors = %s", ci.ExitCode, codes(ci.Errors))
	}
}

func TestArtifactWithoutPackagesIsAnError(t *testing.T) {
	cfg, root := project(t, "targets:\n  - name: bundle\n    dir: dist\n", map[string]string{"dist/index.html": "<html>"})
	r := run(t, cfg, &fakeScanner{outputs: map[string]output{"dir:" + filepath.Join(root, "dist"): {testutil.GrypeJSON(fresh), testutil.SBOMJSON(), nil}}}, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "NO_PACKAGES" {
		t.Fatalf("exit = %d, errors = %s", r.ExitCode, codes(r.Errors))
	}
}

func TestRustFallbackCoveredBySourceTarget(t *testing.T) {
	yaml := "targets:\n  - name: cli\n    file: bin/cli\n    lockfileFallback: Cargo.lock\n  - name: repo\n    source: .\n"
	cfg, root := project(t, yaml, map[string]string{"bin/cli": "\x7fELF", "Cargo.lock": "version = 4"})
	vuln := testutil.GrypeJSON(fresh, testutil.Match{ID: "GHSA-43w2", Package: "smallvec", Version: "1.6.0", Type: "rust-crate", FixState: "fixed", FixVersions: []string{"1.6.1"}})
	r := run(t, cfg, &fakeScanner{outputs: map[string]output{
		"file:" + filepath.Join(root, "bin/cli"): {testutil.GrypeJSON(fresh), testutil.SBOMJSON(), nil},
		"dir:" + root:                            {vuln, testutil.SBOMJSON("pkg:cargo/smallvec@1.6.0"), nil},
	}}, nil)
	if r.ExitCode != 1 || len(r.Errors) != 0 || codes(r.Warnings) != "NOT_AUDITABLE" {
		t.Fatalf("exit = %d, errors = %+v, warnings = %+v", r.ExitCode, r.Errors, r.Warnings)
	}
	if len(r.Findings) != 1 || r.Findings[0].Target != "repo" {
		t.Fatalf("findings must not be duplicated: %+v", r.Findings)
	}
	if r.Targets[0].Identity.SHA256 == "" {
		t.Error("file targets record their SHA-256")
	}
}

func TestRustFallbackScansLockfileWithoutSourceTarget(t *testing.T) {
	yaml := "targets:\n  - name: cli\n    file: bin/cli\n    lockfileFallback: Cargo.lock\n"
	cfg, root := project(t, yaml, map[string]string{"bin/cli": "\x7fELF", "Cargo.lock": "version = 4"})
	vuln := testutil.GrypeJSON(fresh, testutil.Match{ID: "GHSA-43w2", Package: "smallvec", Version: "1.6.0", FixState: "fixed", FixVersions: []string{"1.6.1"}})
	sc := &fakeScanner{outputs: map[string]output{
		"file:" + filepath.Join(root, "bin/cli"):    {testutil.GrypeJSON(fresh), testutil.SBOMJSON(), nil},
		"file:" + filepath.Join(root, "Cargo.lock"): {vuln, testutil.SBOMJSON("pkg:cargo/smallvec@1.6.0"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 1 || len(r.Findings) != 1 || r.Findings[0].Target != "cli" || codes(r.Warnings) != "NOT_AUDITABLE" {
		t.Fatalf("exit = %d, findings = %+v, warnings = %+v, errors = %+v", r.ExitCode, r.Findings, r.Warnings, r.Errors)
	}
}

func TestStaleDatabaseIsAnErrorButKeepsFindings(t *testing.T) {
	cfg, root := project(t, sourceTarget, lock)
	doc := testutil.GrypeJSON(now.Add(-72*time.Hour), testutil.Match{ID: "GHSA-1", Package: "lodash", Version: "4.17.20", FixState: "fixed", FixVersions: []string{"4.17.21"}})
	r := run(t, cfg, &fakeScanner{outputs: map[string]output{"dir:" + root: {doc, testutil.SBOMJSON("pkg:npm/lodash@4.17.20"), nil}}}, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "STALE_DATABASE" || len(r.Findings) != 1 {
		t.Fatalf("exit = %d, errors = %s, findings = %d", r.ExitCode, codes(r.Errors), len(r.Findings))
	}
}

func TestSetupErrorStillWritesEvidence(t *testing.T) {
	cfg, _ := project(t, sourceTarget, lock)
	sc := &fakeScanner{}
	r := run(t, cfg, sc, func(o *Options) { o.SetupError = errors.New("download Grype: offline") })
	if r.ExitCode != 2 || codes(r.Errors) != "SETUP" || len(sc.calls) != 0 {
		t.Fatalf("exit = %d, errors = %s", r.ExitCode, codes(r.Errors))
	}
	if _, err := os.Stat(filepath.Join(r.EvidenceDir, "run.json")); err != nil {
		t.Fatal(err)
	}
}

func TestNonEmptyOutputDirIsRejected(t *testing.T) {
	cfg, _ := project(t, sourceTarget, lock)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, cfg, &fakeScanner{}, func(o *Options) { o.OutputDir = dir })
	if r.ExitCode != 2 || codes(r.Errors) != "EVIDENCE" {
		t.Fatalf("exit = %d, errors = %s", r.ExitCode, codes(r.Errors))
	}
	if r.EvidenceDir != "" {
		t.Errorf("EvidenceDir = %q, want empty after an EVIDENCE error", r.EvidenceDir)
	}
}

func TestCancelledScanIsAnError(t *testing.T) {
	yaml := "targets:\n  - name: repo\n    source: .\n  - name: web\n    image: app:local\n"
	cfg, root := project(t, yaml, lock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := &fakeScanner{cancel: cancel, outputs: map[string]output{
		"dir:" + root:        {testutil.GrypeJSON(fresh), testutil.SBOMJSON("pkg:npm/a@1.0.0"), nil},
		"registry:app:local": {err: errors.New("context canceled")},
	}}
	o := Options{Config: cfg, OutputDir: filepath.Join(t.TempDir(), "evidence"), Scanner: sc, Now: func() time.Time { return now },
		HasImage: func(context.Context, string) bool { return false }}
	r := Run(ctx, o)
	if r.ExitCode != 2 || !strings.Contains(codes(r.Errors), "CANCELLED") {
		t.Fatalf("exit = %d, errors = %s, targets = %+v", r.ExitCode, codes(r.Errors), r.Targets)
	}
}
