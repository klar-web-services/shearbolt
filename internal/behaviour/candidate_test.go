//go:build behaviour

package behaviour

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run in the release workflow (and locally) against a built candidate:
//
//	SHEARBOLT_CANDIDATE=<binary> SHEARBOLT_CANDIDATE_VERSION=<version> \
//	SHEARBOLT_TEST_DB_ARCHIVE=$(scripts/db-snapshot.sh fetch ~/.cache/shearbolt-db-snapshot) \
//	  go test -tags behaviour -count=1 -timeout 30m ./internal/behaviour/ -v
//
// The Shearbolt cache (the pinned Grype and the imported database, about 3.5 GB) goes to
// $SHEARBOLT_CACHE_DIR, else to the user cache dir's shearbolt-behaviour.

func absEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("set %s", name)
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// TestCandidate runs every release case against the candidate. It does not read
// SHEARBOLT_CANDIDATE_VERSION, so the negative control exercises only the cases.
func TestCandidate(t *testing.T) {
	binary := absEnv(t, "SHEARBOLT_CANDIDATE")
	archive := absEnv(t, "SHEARBOLT_TEST_DB_ARCHIVE")
	cache := os.Getenv("SHEARBOLT_CACHE_DIR")
	if cache == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		cache = filepath.Join(base, "shearbolt-behaviour")
	}
	cache, err := filepath.Abs(cache)
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := filepath.Abs(filepath.Join("..", "..", "testdata", "projects"))
	if err != nil {
		t.Fatal(err)
	}
	env := Env{Binary: binary, DBArchive: archive, CacheDir: cache, Fixtures: fixtures, WorkDir: t.TempDir()}
	for _, c := range Cases(time.Now()) {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			if err := Run(ctx, env, c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCandidateVersion checks the version stamped into the candidate with -ldflags.
func TestCandidateVersion(t *testing.T) {
	binary := absEnv(t, "SHEARBOLT_CANDIDATE")
	version := os.Getenv("SHEARBOLT_CANDIDATE_VERSION")
	if version == "" {
		t.Fatal("set SHEARBOLT_CANDIDATE_VERSION")
	}
	out, err := exec.Command(binary, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", binary, err)
	}
	if line, _, _ := strings.Cut(string(out), "\n"); line != "shearbolt "+version {
		t.Fatalf("first line of %s version is %q, want %q", binary, line, "shearbolt "+version)
	}
}
