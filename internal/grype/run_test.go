package grype

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/testutil"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newRunner(t *testing.T, extraEnv ...string) (*Runner, string, string) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	r := &Runner{
		Binary:   testutil.FakeGrype(t),
		CacheDir: filepath.Join(dir, "cache"),
		Environ:  append(append(os.Environ(), "FAKE_GRYPE_LOG="+log), extraEnv...),
	}
	return r, dir, log
}

func TestScanRunsGrypeIsolated(t *testing.T) {
	r, dir, log := newRunner(t, "GRYPE_DB_AUTO_UPDATE=false", "SYFT_EXCLUDE=x")
	req := Request{Source: "dir:/src", Exclude: []string{"**/node_modules/**"}, RawJSON: filepath.Join(dir, "out.json"), SBOM: filepath.Join(dir, "out.cdx.json")}
	if err := r.Scan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(read(t, log)), "\n")
	want := []string{"dir:/src", "-o", "json=" + req.RawJSON, "-o", "cyclonedx-json=" + req.SBOM, "--exclude", "**/node_modules/**", "-c"}
	if len(args) < len(want) || !slices.Equal(args[:len(want)], want) {
		t.Fatalf("args = %q", args)
	}
	if env := read(t, log+".env"); strings.Contains(env, "GRYPE_DB_AUTO_UPDATE") || strings.Contains(env, "SYFT_EXCLUDE") {
		t.Fatal("GRYPE_*/SYFT_* variables must not reach Grype")
	}
	if cwd, _ := os.Getwd(); strings.TrimSpace(read(t, log+".pwd")) == cwd {
		t.Fatal("Grype must not run in the caller's working directory")
	}
	work := strings.TrimSpace(read(t, log+".pwd"))
	var xdg string
	for _, kv := range strings.Split(read(t, log+".env"), "\n") {
		if v, ok := strings.CutPrefix(kv, "XDG_CONFIG_HOME="); ok {
			xdg = v
		}
	}
	if xdg == "" || !strings.HasPrefix(xdg, work+string(filepath.Separator)) {
		t.Fatalf("XDG_CONFIG_HOME = %q, want a path inside %q", xdg, work)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("working directory %s must be removed after Scan (err = %v)", work, err)
	}
	cfg := read(t, log+".cfg")
	for _, s := range []string{"check-for-app-update: false", "auto-update: true", "validate-age: false", filepath.Join(dir, "cache", "grype-db")} {
		if !strings.Contains(cfg, s) {
			t.Errorf("Grype config lacks %q:\n%s", s, cfg)
		}
	}
	if _, err := os.Stat(req.RawJSON); err != nil {
		t.Fatal(err)
	}
}

func TestScanWithoutDatabaseUpdates(t *testing.T) {
	r, dir, log := newRunner(t)
	r.NoDBUpdate = true
	if err := r.Scan(context.Background(), Request{Source: "dir:/src", RawJSON: filepath.Join(dir, "a"), SBOM: filepath.Join(dir, "b")}); err != nil {
		t.Fatal(err)
	}
	if cfg := read(t, log+".cfg"); !strings.Contains(cfg, "auto-update: false") {
		t.Fatalf("config = %s", cfg)
	}
}

func TestScanFailureIncludesGrypeMessage(t *testing.T) {
	r, dir, _ := newRunner(t, "FAKE_GRYPE_EXIT=1", "FAKE_GRYPE_STDERR=could not fetch image")
	err := r.Scan(context.Background(), Request{Source: "registry:x", RawJSON: filepath.Join(dir, "a"), SBOM: filepath.Join(dir, "b")})
	if err == nil || !strings.Contains(err.Error(), "could not fetch image") {
		t.Fatalf("err = %v", err)
	}
}

func TestScanTimesOut(t *testing.T) {
	r, dir, _ := newRunner(t, "FAKE_GRYPE_SLEEP=5")
	r.Timeout = 300 * time.Millisecond
	err := r.Scan(context.Background(), Request{Source: "dir:/src", RawJSON: filepath.Join(dir, "a"), SBOM: filepath.Join(dir, "b")})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestImportDB(t *testing.T) {
	r, _, log := newRunner(t)
	if err := r.ImportDB(context.Background(), "/snapshots/db.tar.zst"); err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(read(t, log)), "\n")
	if !slices.Equal(args[:3], []string{"db", "import", "/snapshots/db.tar.zst"}) {
		t.Fatalf("args = %q", args)
	}
}

func TestScanWithRelativePaths(t *testing.T) {
	bin := testutil.FakeGrype(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	rel, err := filepath.Rel(cwd, bin)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(cwd, "args.log")
	r := &Runner{Binary: rel, CacheDir: "cache", Environ: append(os.Environ(), "FAKE_GRYPE_LOG="+log)}
	if err := r.Scan(context.Background(), Request{Source: "dir:.", RawJSON: "out.json", SBOM: "out.cdx.json"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"out.json", "out.cdx.json"} {
		if _, err := os.Stat(filepath.Join(cwd, f)); err != nil {
			t.Errorf("%s not written relative to the caller's directory: %v", f, err)
		}
	}
	if r.CacheDir != "cache" || r.Binary != rel {
		t.Error("Runner fields must not be mutated")
	}
	cfg, err := os.ReadFile(log + ".cfg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), filepath.Join(cwd, "cache", "grype-db")) {
		t.Errorf("config does not contain the absolute database dir:\n%s", cfg)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if first := strings.SplitN(string(args), "\n", 2)[0]; first != "dir:"+cwd {
		t.Errorf("first argument = %q, want %q", first, "dir:"+cwd)
	}
}

func TestScanCancelledByCaller(t *testing.T) {
	r, dir, _ := newRunner(t, "FAKE_GRYPE_SLEEP=5")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := r.Scan(ctx, Request{Source: "dir:/src", RawJSON: filepath.Join(dir, "a"), SBOM: filepath.Join(dir, "b")})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want scan cancelled", err)
	}
}
