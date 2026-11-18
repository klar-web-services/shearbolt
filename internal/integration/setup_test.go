//go:build integration

package integration

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/cli"
	"github.com/klar-web-services/shearbolt/internal/model"
)

// The scans run in local mode, so a missing artifact is skipped.
func setupFixture(t *testing.T, name string) string {
	t.Helper()
	t.Setenv("CI", "")
	t.Setenv("SHEARBOLT_IMAGE", "") // the generated ${SHEARBOLT_IMAGE:-…} takes its default
	dir := t.TempDir()
	src := fixture("setup", name, "before")
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		return copyFile(p, filepath.Join(dir, rel), info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var out, errb bytes.Buffer
	if code := cli.Run([]string{"setup", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("setup --yes: exit = %d\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
	f, err := os.OpenFile(filepath.Join(dir, "shearbolt.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(snapshotPolicy); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func copyFile(src, dst string, mode fs.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode) // WriteFile's mode is subject to the umask
}

func setupScan(t *testing.T) (int, model.Run) {
	t.Helper()
	archive := useSnapshotEnv(t)
	code, r := runScan(t)
	checkSnapshotRun(t, archive, r)
	return code, r
}

// The image storefront:local is neither in the local Docker nor pullable, so its target is
// skipped.
func TestSetupNextJSConfigScans(t *testing.T) {
	setupFixture(t, "nextjs-build-push")
	code, r := setupScan(t)
	if code != 1 || r.Verdict != "block" || len(r.Errors) > 0 {
		t.Fatalf("exit = %d, verdict = %q, want 1 and block; errors = %+v", code, r.Verdict, r.Errors)
	}
	if status, packages := targetStatus(r, "source"), target(r, "source").Packages; status != "scanned" || packages == 0 {
		t.Errorf("source: status %q with %d packages, want scanned with packages", status, packages)
	}
	if status := targetStatus(r, "storefront"); status != "skipped" {
		t.Errorf("storefront: status %q, want skipped", status)
	}
	if !slices.ContainsFunc(r.Findings, func(f model.Finding) bool {
		return f.ID == "GHSA-f82v-jwr5-mffw" && f.Package == "next" && f.Version == "15.0.0" && f.Target == "source" && f.Decision.Action == model.Block
	}) || !blockedIn(r, "source", "next", "15.0.0", "package-lock.json") {
		t.Errorf("next 15.0.0 (GHSA-f82v-jwr5-mffw) is not blocked in source from package-lock.json; findings = %+v", r.Findings)
	}
}

func TestSetupRustConfigScans(t *testing.T) {
	dir := setupFixture(t, "rust-make")
	binary := filepath.Join(dir, "target", "release", "demo")
	if err := copyFile(fixture("rust", "bin", "demo-auditable-linux-amd64"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	code, r := setupScan(t)
	if code != 1 || !blocked(r, "demo", "smallvec", "1.6.0") || hasCode(r.Warnings, "NOT_AUDITABLE", "") {
		t.Fatalf("auditable binary: exit = %d, warnings = %+v, errors = %+v, findings = %+v", code, r.Warnings, r.Errors, r.Findings)
	}

	if err := copyFile(fixture("rust", "bin", "demo-plain-linux-amd64"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	code, r = setupScan(t)
	if code != 1 || !hasCode(r.Warnings, "NOT_AUDITABLE", "demo") || !blocked(r, "source", "smallvec", "1.6.0") || blocked(r, "demo", "smallvec", "1.6.0") {
		t.Fatalf("plain binary: exit = %d, warnings = %+v, errors = %+v, findings = %+v", code, r.Warnings, r.Errors, r.Findings)
	}
}
