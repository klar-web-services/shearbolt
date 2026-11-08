package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/testutil"
)

func testDeps(t *testing.T, vars map[string]string) deps {
	t.Helper()
	fake := testutil.FakeGrype(t)
	all := map[string]string{"SHEARBOLT_CACHE_DIR": t.TempDir()}
	for k, v := range vars {
		all[k] = v
	}
	d := defaultDeps()
	d.ensureGrype = func(context.Context, string) (string, error) { return fake, nil }
	d.lookupEnv = func(k string) (string, bool) { v, ok := all[k]; return v, ok }
	d.getenv = func(k string) string { return all[k] }
	d.environ = func() []string {
		env := os.Environ()
		for k, v := range all {
			env = append(env, k+"="+v)
		}
		return env
	}
	d.color = func(io.Writer) bool { return false }
	return d
}

func projectDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func scanCmd(t *testing.T, d deps, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"scan"}, args...), &out, &errb, d)
	return code, out.String(), errb.String()
}

func evidence(t *testing.T) string { return filepath.Join(t.TempDir(), "evidence") }

func TestScanBlocksAndExitsOne(t *testing.T) {
	doc := testutil.GrypeJSON(time.Now(), testutil.Match{ID: "GHSA-1", Package: "lodash", Version: "4.17.20", FixState: "fixed", FixVersions: []string{"4.17.21"}})
	code, out, errb := scanCmd(t, testDeps(t, map[string]string{"FAKE_GRYPE_JSON": doc}), "--source", projectDir(t), "--output-dir", evidence(t))
	if code != 1 || !strings.Contains(out, "Blocked") || !strings.Contains(out, "lodash 4.17.20 → 4.17.21") {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

func TestScanJSONPrintsRunJSON(t *testing.T) {
	code, out, _ := scanCmd(t, testDeps(t, nil), "--source", projectDir(t), "--output-dir", evidence(t), "--json")
	var r model.Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if code != 0 || r.Verdict != "pass" {
		t.Fatalf("exit = %d, verdict = %s", code, r.Verdict)
	}
}

func TestScanRejectsConfigWithAdHocTargets(t *testing.T) {
	code, _, errb := scanCmd(t, testDeps(t, nil), "--config", "x.yaml", "--source", ".")
	if code != 2 || !strings.Contains(errb, "not both") {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
}

func TestScanWithoutConfigIsExplained(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, errb := scanCmd(t, testDeps(t, nil))
	setupAt := strings.Index(errb, "Run `shearbolt setup`")
	readmeAt := strings.Index(errb, "https://github.com/klar-web-services/shearbolt#readme")
	if code != 2 || !strings.Contains(errb, "no shearbolt.yaml here") || setupAt < 0 || readmeAt < setupAt || !strings.Contains(errb, "--source") {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
}

func TestScanWithMissingExplicitConfigNamesThePath(t *testing.T) {
	code, _, errb := scanCmd(t, testDeps(t, nil), "--config", "missing.yaml")
	if code != 2 || !strings.Contains(errb, "missing.yaml") || !strings.Contains(errb, "does not exist") || !strings.Contains(errb, "Run `shearbolt setup`") || !strings.Contains(errb, "https://github.com/klar-web-services/shearbolt#readme") || !strings.Contains(errb, "--source") {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
}

func TestScanUnknownTarget(t *testing.T) {
	dir := projectDir(t)
	cfg := filepath.Join(dir, "shearbolt.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errb := scanCmd(t, testDeps(t, nil), "--config", cfg, "--target", "nope")
	if code != 2 || !strings.Contains(errb, "nope") {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
}

func TestScanAppendsGitHubSummary(t *testing.T) {
	summary := filepath.Join(t.TempDir(), "summary.md")
	code, _, _ := scanCmd(t, testDeps(t, map[string]string{"GITHUB_STEP_SUMMARY": summary}), "--source", projectDir(t), "--output-dir", evidence(t))
	data, err := os.ReadFile(summary)
	if code != 0 || err != nil || !strings.Contains(string(data), "Passed") {
		t.Fatalf("exit = %d, err = %v, summary = %s", code, err, data)
	}
}

func TestScanConfigErrorGoesToGitHubSummary(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "shearbolt.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\nproject: p\ntargets:\n  - name: web\n    image: app:${GITHUB_SHA}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(dir, "summary.md")
	code, _, _ := scanCmd(t, testDeps(t, map[string]string{"GITHUB_STEP_SUMMARY": summary}), "--config", cfg)
	data, err := os.ReadFile(summary)
	if code != 2 || err != nil || !strings.Contains(string(data), "invalid configuration") || !strings.Contains(string(data), "GITHUB_SHA") {
		t.Fatalf("exit = %d, err = %v, summary = %s", code, err, data)
	}
}

func TestScanGrypeSetupFailure(t *testing.T) {
	d := testDeps(t, nil)
	d.ensureGrype = func(context.Context, string) (string, error) {
		return "", errors.New("download Grype 0.121.2: offline")
	}
	ev := evidence(t)
	code, out, _ := scanCmd(t, d, "--source", projectDir(t), "--output-dir", ev)
	if code != 2 || !strings.Contains(out, "Scan incomplete") {
		t.Fatalf("exit = %d, stdout = %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(ev, "run.json")); err != nil {
		t.Fatalf("evidence must be written even when Grype is unavailable: %v", err)
	}
}

func TestCIModeSwitch(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	for _, tc := range []struct {
		ci   string
		want int
	}{{"", 0}, {"true", 2}, {"false", 0}} {
		vars := map[string]string{}
		if tc.ci != "" {
			vars["CI"] = tc.ci
		}
		code, out, errb := scanCmd(t, testDeps(t, vars), "--source", projectDir(t), "--file", missing, "--output-dir", evidence(t))
		if code != tc.want {
			t.Errorf("CI=%q: exit = %d, want %d\nstdout: %s\nstderr: %s", tc.ci, code, tc.want, out, errb)
		}
	}
}

func TestSelectTargetsIgnoresDuplicates(t *testing.T) {
	all := []config.Target{{Name: "a"}, {Name: "b"}}
	got, err := selectTargets(all, []string{"a", "a", "b", "a"})
	if err != nil || len(got) != 2 {
		t.Fatalf("got = %+v, err = %v", got, err)
	}
}

// With SHEARBOLT_DB_ARCHIVE, the first scan imports the archive and the next one, on the same
// cache with the same archive, skips the import. run.json's database fields are the same either way.
func TestScanImportsTheSameArchiveOnce(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "db.tar.zst")
	if err := os.WriteFile(archive, []byte("snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	built := time.Now().UTC().Truncate(time.Second)
	d := testDeps(t, map[string]string{
		"SHEARBOLT_DB_ARCHIVE": archive,
		"FAKE_GRYPE_CALLS":     calls,
		"FAKE_GRYPE_STATUS":    `{"schemaVersion":"v6.1.10","from":"manual import","built":"` + built.Format(time.RFC3339) + `","valid":true}`,
		"FAKE_GRYPE_JSON":      testutil.GrypeJSON(built),
	})
	project := projectDir(t)
	var runs []model.Run
	for range 2 {
		code, out, errb := scanCmd(t, d, "--json", "--source", project, "--output-dir", evidence(t))
		if code != 0 {
			t.Fatalf("exit %d\nstderr: %s", code, errb)
		}
		var r model.Run
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "db import "); n != 1 {
		t.Fatalf("%d imports over two scans, want 1:\n%s", n, data)
	}
	if runs[0].DBArchive != archive || runs[1].DBArchive != archive {
		t.Fatalf("dbArchive = %q, %q, want %q", runs[0].DBArchive, runs[1].DBArchive, archive)
	}
	if runs[0].Database == nil || runs[1].Database == nil || *runs[0].Database != *runs[1].Database {
		t.Fatalf("database differs between the importing and the skipping scan: %+v vs %+v", runs[0].Database, runs[1].Database)
	}
}
