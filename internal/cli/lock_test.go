//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/scanlock"
)

// raceGrype keeps one database in $RACE_STATE/db: "db import" copies the archive there (the
// archive's content is its database build time), "db status" reports it after $RACE_SLEEP
// seconds (real Grype checks the 3.4 GB database for about a second), and a scan reports it.
const raceGrype = `#!/bin/sh
state="$RACE_STATE"
case "$1 $2" in
"db import") : > "$state/importing"; cat "$3" > "$state/db"; exit 0 ;;
"db status") sleep "$RACE_SLEEP"; printf '{"schemaVersion":"v6.1.10","from":"manual import","built":"%s","valid":true}\n' "$(cat "$state/db")"; exit 0 ;;
esac
built=$(cat "$state/db")
while [ $# -gt 0 ]; do
  case "$1" in
    -o)
      case "$2" in
        json=*) printf '{"matches":[],"source":{"type":"directory","target":"/src"},"descriptor":{"name":"grype","version":"0.121.2","db":{"status":{"built":"%s","schemaVersion":"v6.1.10","from":"manual import"}}}}' "$built" > "${2#json=}" ;;
        cyclonedx-json=*) printf '%s' '{"bomFormat":"CycloneDX","components":[{"type":"library","name":"left-pad","purl":"pkg:npm/left-pad@1.3.0","properties":[{"name":"syft:location:0:path","value":"/package-lock.json"}]}]}' > "${2#cyclonedx-json=}" ;;
      esac
      shift ;;
  esac
  shift
done
exit 0
`

const (
	builtX = "2026-01-01T00:00:00Z" // the database in archive X
	builtY = "2026-06-01T00:00:00Z" // the database in archive Y
)

type raceEnv struct {
	grype, state, cache, config string
	x, y                        string
}

func newRaceEnv(t *testing.T) raceEnv {
	t.Helper()
	dir := t.TempDir()
	e := raceEnv{grype: filepath.Join(dir, "grype"), state: filepath.Join(dir, "state"), cache: filepath.Join(dir, "cache"),
		config: filepath.Join(dir, "shearbolt.yaml"), x: filepath.Join(dir, "x.tar.zst"), y: filepath.Join(dir, "y.tar.zst")}
	files := map[string]string{
		e.grype:  raceGrype,
		e.x:      builtX,
		e.y:      builtY,
		e.config: "version: 1\nproject: race\ntargets:\n  - name: repo\n    source: " + projectDir(t) + "\npolicy:\n  maxDatabaseAgeHours: 1000000\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(e.state, 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

// scan runs one archive scan on the shared cache and returns its run and stderr.
func (e raceEnv) scan(t *testing.T, archive, sleep string) (model.Run, string) {
	vars := map[string]string{"SHEARBOLT_CACHE_DIR": e.cache, "SHEARBOLT_DB_ARCHIVE": archive, "RACE_STATE": e.state, "RACE_SLEEP": sleep}
	d := defaultDeps()
	d.ensureGrype = func(context.Context, string) (string, error) { return e.grype, nil }
	d.lookupEnv = func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	d.getenv = func(k string) string { return vars[k] }
	d.environ = func() []string {
		env := os.Environ()
		for k, v := range vars {
			env = append(env, k+"="+v)
		}
		return env
	}
	d.color = func(io.Writer) bool { return false }
	code, out, errb := scanCmd(t, d, "--json", "--config", e.config, "--output-dir", evidence(t))
	var r model.Run
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 {
		t.Errorf("scan with %s: exit %d (%v)\nstderr: %s", filepath.Base(archive), code, err, errb)
	}
	return r, errb
}

func built(r model.Run) string {
	if r.Database == nil {
		return ""
	}
	return r.Database.Built.UTC().Format(time.RFC3339)
}

// Two scans with different archives on one cache run one after the other. Without the lock,
// the second import lands while the first scan checks its database, so archive X's marker is
// tied to archive Y's database, and a later scan with X skips the import and scans with Y's data.
func TestScansOnOneCacheRunOneAfterTheOther(t *testing.T) {
	e := newRaceEnv(t)
	type result struct {
		run    model.Run
		stderr string
	}
	first := make(chan result, 1)
	go func() {
		r, errb := e.scan(t, e.x, "0.5")
		first <- result{r, errb}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(e.state, "importing")); err == nil {
			break // the first scan holds the lock and is importing X
		}
		if time.Now().After(deadline) {
			t.Fatal("the first scan never imported")
		}
		time.Sleep(5 * time.Millisecond)
	}
	second, secondErr := e.scan(t, e.y, "0.05")
	a := <-first

	if want := "waiting for another shearbolt scan that uses " + e.cache; !strings.Contains(secondErr, want) {
		t.Errorf("the second scan's stderr lacks %q:\n%s", want, secondErr)
	}
	if strings.Contains(a.stderr, "waiting") {
		t.Errorf("the first scan waited:\n%s", a.stderr)
	}
	if built(a.run) != builtX || built(second) != builtY {
		t.Errorf("databases scanned: first %s (want %s), second %s (want %s)", built(a.run), builtX, built(second), builtY)
	}
	if later, _ := e.scan(t, e.x, "0.05"); built(later) != builtX {
		t.Fatalf("a later scan with archive X used the database built %s, want %s", built(later), builtX)
	}
}

func TestSequentialScansDoNotWait(t *testing.T) {
	e := newRaceEnv(t)
	for _, archive := range []string{e.x, e.y, e.x} {
		if _, errb := e.scan(t, archive, "0"); strings.Contains(errb, "waiting") {
			t.Fatalf("a sequential scan waited:\n%s", errb)
		}
	}
}

// A scan whose cache is locked elsewhere says so and waits.
func TestScanWaitsForALockedCache(t *testing.T) {
	e := newRaceEnv(t)
	release, err := scanlock.Acquire(e.cache, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, release)
	start := time.Now()
	_, errb := e.scan(t, e.x, "0")
	if !strings.Contains(errb, "waiting for another shearbolt scan that uses "+e.cache) || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("after %s, stderr:\n%s", time.Since(start), errb)
	}
}

// The lock file stays in the cache; it never lands in the evidence folder.
func TestLockFileStaysInTheCache(t *testing.T) {
	e := newRaceEnv(t)
	out := evidence(t)
	vars := map[string]string{"SHEARBOLT_CACHE_DIR": e.cache}
	d := testDeps(t, vars)
	if code, _, errb := scanCmd(t, d, "--config", e.config, "--output-dir", out); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(e.cache, scanlock.File)); err != nil {
		t.Fatalf("no lock file in the cache: %v", err)
	}
	filepath.WalkDir(out, func(path string, _ os.DirEntry, err error) error {
		if err == nil && filepath.Base(path) == scanlock.File {
			t.Errorf("lock file in the evidence: %s", path)
		}
		return nil
	})
}

// An output folder that is, contains or sits inside the cache is refused before anything runs.
func TestOutputDirMustNotOverlapTheCache(t *testing.T) {
	e := newRaceEnv(t)
	for name, out := range map[string]string{
		"the cache":        e.cache,
		"inside the cache": filepath.Join(e.cache, "runs", "1"),
		"around the cache": filepath.Dir(e.cache),
	} {
		t.Run(name, func(t *testing.T) {
			d := testDeps(t, map[string]string{"SHEARBOLT_CACHE_DIR": e.cache})
			code, _, errb := scanCmd(t, d, "--config", e.config, "--output-dir", out)
			if code != 2 || !strings.Contains(errb, "Shearbolt cache") {
				t.Fatalf("exit %d, stderr:\n%s", code, errb)
			}
		})
	}
}
