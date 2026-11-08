//go:build integration

// Package integration runs Shearbolt against the real pinned Grype.
//
// The snapshot tests (snapshot_test.go) scan with the pinned vulnerability database snapshot
// (testdata/db-snapshot.sha256), so their results are deterministic. The one live test
// (live_test.go) uses today's database. Run with:
//
//	export SHEARBOLT_TEST_DB_ARCHIVE=$(scripts/db-snapshot.sh fetch ~/.cache/shearbolt-db-snapshot)
//	go test -tags integration -count=1 ./internal/integration/ -v -timeout 30m
//
// Caches live below $SHEARBOLT_CACHE_DIR (default: the user cache dir's shearbolt-integration):
// snapshot/ for snapshot scans and live/ for the live test. The first snapshot scan imports the
// archive; the later ones find the import marker and skip it.
package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/cli"
	"github.com/klar-web-services/shearbolt/internal/grype"
	"github.com/klar-web-services/shearbolt/internal/model"
)

// nodeImage is node:22-alpine pinned by digest (multi-arch index); it ships a Node.js runtime binary.
const nodeImage = "docker.io/library/node@sha256:ab07539e0988b63558ff621f5fbe1077054c39d9809112974fb79993949d41cd"

// snapshotPolicy lets snapshot scans pass the database age check (spec section 4).
const snapshotPolicy = "policy:\n  maxDatabaseAgeHours: 1000000\n"

var (
	cacheBase string // the absolute base cache folder for all integration scans
	testdata  string // the absolute testdata folder, fixed before any test changes directory

	// firstSnapshotDB holds run.json's database fields from the first snapshot scan, which
	// imports the archive; every later snapshot scan skips the import and must report the same.
	firstSnapshotDB *model.Database
)

func TestMain(m *testing.M) {
	// Do not append every test scan to the CI job summary.
	os.Unsetenv("GITHUB_STEP_SUMMARY")
	dir := os.Getenv("SHEARBOLT_CACHE_DIR")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			panic(err)
		}
		dir = filepath.Join(base, "shearbolt-integration")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		panic(err)
	}
	cacheBase = abs
	if testdata, err = filepath.Abs(filepath.Join("..", "..", "testdata")); err != nil {
		panic(err)
	}
	// A relative archive path would break once a test changes directory (t.Chdir).
	if archive := os.Getenv("SHEARBOLT_TEST_DB_ARCHIVE"); archive != "" {
		abs, err := filepath.Abs(archive)
		if err != nil {
			panic(err)
		}
		os.Setenv("SHEARBOLT_TEST_DB_ARCHIVE", abs)
	}
	// Make the first snapshot scan import, so the import and the skipped imports are compared.
	if err := os.Remove(filepath.Join(cacheBase, "snapshot", grype.ImportMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		panic(err)
	}
	os.Exit(m.Run())
}

// fixture is the absolute path of a file or folder below testdata, whatever the working directory.
func fixture(parts ...string) string {
	return filepath.Join(append([]string{testdata}, parts...)...)
}

// quoted is a fixture path as a YAML double-quoted scalar.
func quoted(parts ...string) string { return strconv.Quote(fixture(parts...)) }

// sourceTarget is one `source` target of a config's targets block.
func sourceTarget(name string, parts ...string) string {
	return fmt.Sprintf("  - name: %s\n    source: %s\n", name, quoted(parts...))
}

// useSnapshotEnv makes the scans of the calling test use the pinned database snapshot: it sets
// SHEARBOLT_DB_ARCHIVE to $SHEARBOLT_TEST_DB_ARCHIVE and SHEARBOLT_CACHE_DIR to <base>/snapshot,
// so no import lands in the live cache. Every test that scans with a policy written for the
// snapshot must call it (snapshotScan does; Plan 2's setup integration tests call it directly).
func useSnapshotEnv(t *testing.T) string {
	t.Helper()
	archive := os.Getenv("SHEARBOLT_TEST_DB_ARCHIVE")
	if archive == "" {
		t.Fatal("set SHEARBOLT_TEST_DB_ARCHIVE: export SHEARBOLT_TEST_DB_ARCHIVE=$(scripts/db-snapshot.sh fetch ~/.cache/shearbolt-db-snapshot)")
	}
	t.Setenv("SHEARBOLT_DB_ARCHIVE", archive)
	t.Setenv("SHEARBOLT_CACHE_DIR", filepath.Join(cacheBase, "snapshot"))
	return archive
}

// snapshotScan scans one config, made of the given targets block and the snapshot policy, on
// the pinned snapshot.
func snapshotScan(t *testing.T, targets string) (int, model.Run) {
	t.Helper()
	return snapshotScanWithPolicy(t, targets, snapshotPolicy)
}

// snapshotScanWithPolicy is snapshotScan with a policy block of the caller's own, which must
// keep maxDatabaseAgeHours: 1000000.
func snapshotScanWithPolicy(t *testing.T, targets, policy string) (int, model.Run) {
	t.Helper()
	archive := useSnapshotEnv(t)
	config := filepath.Join(t.TempDir(), "shearbolt.yaml")
	if err := os.WriteFile(config, []byte("version: 1\nproject: fixture\ntargets:\n"+targets+policy), 0o644); err != nil {
		t.Fatal(err)
	}
	code, r := runScan(t, "--config", config)
	checkSnapshotRun(t, archive, r)
	return code, r
}

// checkSnapshotRun requires that a scan used the snapshot archive and reports the same database
// as the first (importing) snapshot scan.
func checkSnapshotRun(t *testing.T, archive string, r model.Run) {
	t.Helper()
	if r.DBArchive != archive {
		t.Fatalf("dbArchive = %q, want the snapshot %q", r.DBArchive, archive)
	}
	if d := r.Database; d != nil {
		if firstSnapshotDB == nil {
			firstSnapshotDB = d
		} else if !d.Built.Equal(firstSnapshotDB.Built) || d.SchemaVersion != firstSnapshotDB.SchemaVersion || d.Source != firstSnapshotDB.Source {
			t.Fatalf("database = %+v, but the first (importing) snapshot scan reported %+v", *d, *firstSnapshotDB)
		}
	}
}

// liveScan scans with today's database (ad hoc flags, default policy) in <base>/live.
func liveScan(t *testing.T, args ...string) (int, model.Run) {
	t.Helper()
	t.Setenv("SHEARBOLT_CACHE_DIR", filepath.Join(cacheBase, "live"))
	t.Setenv("SHEARBOLT_DB_ARCHIVE", "") // restores any previous value after the test
	os.Unsetenv("SHEARBOLT_DB_ARCHIVE")
	return runScan(t, args...)
}

// runScan runs `shearbolt scan --json` and parses run.json. EvidenceDir is set to the evidence folder.
func runScan(t *testing.T, args ...string) (int, model.Run) {
	t.Helper()
	evidence := filepath.Join(t.TempDir(), "evidence")
	var out, errb bytes.Buffer
	code := cli.Run(append([]string{"scan", "--json", "--output-dir", evidence}, args...), &out, &errb)
	var r model.Run
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("stdout is not run.json (exit %d): %v\nstderr: %s", code, err, errb.String())
	}
	r.EvidenceDir = evidence
	return code, r
}

// blocked reports a blocking finding for pkg@version, in target unless target is empty.
func blocked(r model.Run, target, pkg, version string) bool {
	return slices.ContainsFunc(r.Findings, func(f model.Finding) bool {
		return f.Package == pkg && f.Version == version && f.Decision.Action == model.Block && (target == "" || f.Target == target)
	})
}

// blockedIn is like blocked, but also requires a location ending in the given lockfile path.
func blockedIn(r model.Run, target, pkg, version, lockfile string) bool {
	return slices.ContainsFunc(r.Findings, func(f model.Finding) bool {
		return f.Package == pkg && f.Version == version && f.Decision.Action == model.Block && (target == "" || f.Target == target) &&
			slices.ContainsFunc(f.Locations, func(l string) bool { return strings.HasSuffix(l, lockfile) })
	})
}

// hasCode reports a diagnostic with the code, for target unless target is empty.
func hasCode(ds []model.Diagnostic, code, target string) bool {
	return slices.ContainsFunc(ds, func(d model.Diagnostic) bool { return d.Code == code && (target == "" || d.Target == target) })
}

// targetStatus is the status of the named target, or "" if the run has no such target.
func targetStatus(r model.Run, name string) string {
	for _, t := range r.Targets {
		if t.Name == name {
			return t.Status
		}
	}
	return ""
}

func target(r model.Run, name string) model.TargetResult {
	for _, t := range r.Targets {
		if t.Name == name {
			return t
		}
	}
	return model.TargetResult{}
}
