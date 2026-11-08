package behaviour

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/model"
)

func TestCasesCoverTheReleaseChecks(t *testing.T) {
	want := map[string]int{"vulnerable": 1, "clean": 0, "broken-config": 2, "kev": 1, "expired-exception": 1, "in-date-exception": 0}
	got := map[string]int{}
	for _, c := range Cases(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)) {
		if _, dup := got[c.Name]; dup {
			t.Errorf("case %s is listed twice", c.Name)
		}
		got[c.Name] = c.WantExit
		if c.Check == nil {
			t.Errorf("case %s has no check", c.Name)
		}
	}
	for name, exit := range want {
		if e, ok := got[name]; !ok || e != exit {
			t.Errorf("case %s: present %v, exit %d; want exit %d", name, ok, e, exit)
		}
	}
}

// The in-date exception expires 30 days after today, within the default 90-day limit.
func TestInDateExceptionIsDatedFromToday(t *testing.T) {
	for _, c := range Cases(time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)) {
		if c.Name == "in-date-exception" && !slices.Contains(strings.Split(c.Config, "\n"), "    expires: 2026-12-08") {
			t.Fatalf("in-date-exception config:\n%s", c.Config)
		}
		if c.Name == "expired-exception" && !slices.Contains(strings.Split(c.Config, "\n"), "    expires: 2026-01-01") {
			t.Fatalf("expired-exception config:\n%s", c.Config)
		}
	}
}

// fake writes an executable shell script that ignores its arguments.
func fake(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shearbolt")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeEnv(t *testing.T, binary string) Env {
	return Env{Binary: binary, DBArchive: "/nonexistent/db.tar.zst", CacheDir: t.TempDir(), Fixtures: "/nonexistent/projects", WorkDir: t.TempDir()}
}

// failures runs every case against a binary and returns the names of the cases that failed.
func failures(t *testing.T, binary string) []string {
	t.Helper()
	var failed []string
	for _, c := range Cases(time.Now()) {
		if err := Run(context.Background(), fakeEnv(t, binary), c); err != nil {
			failed = append(failed, c.Name)
		}
	}
	return failed
}

// Constant fakes must fail: a release check that a binary with a fixed answer can pass proves nothing.
func TestConstantFakesFail(t *testing.T) {
	for name, body := range map[string]string{
		"exit 0":    "exit 0",
		"exit 1":    "exit 1",
		"exit 2":    "exit 2",
		"fake pass": `printf '%s\n' '{"verdict":"pass","exitCode":0,"findings":[]}'; exit 0`,
	} {
		t.Run(name, func(t *testing.T) {
			if failed := failures(t, fake(t, body)); len(failed) == 0 {
				t.Fatal("every case passed")
			}
		})
	}
}

func TestAlwaysZeroFailsTheSecurityCases(t *testing.T) {
	failed := failures(t, fake(t, "exit 0"))
	for _, name := range []string{"vulnerable", "broken-config", "kev", "expired-exception"} {
		if !slices.Contains(failed, name) {
			t.Errorf("an always-0 binary passes %s", name)
		}
	}
}

// A binary that cannot start is an error, not a pass.
func TestMissingBinaryFails(t *testing.T) {
	if failed := failures(t, filepath.Join(t.TempDir(), "missing")); len(failed) != len(Cases(time.Now())) {
		t.Fatalf("only %v failed", failed)
	}
}

// Run gives the binary the documented environment: CI=true, the snapshot and cache settings, and
// no GITHUB_STEP_SUMMARY.
func TestRunEnvironment(t *testing.T) {
	t.Setenv("GITHUB_STEP_SUMMARY", "/should/not/leak")
	t.Setenv("CI", "")
	t.Setenv("SHEARBOLT_DB_ARCHIVE", "/parent/value")
	out := filepath.Join(t.TempDir(), "env.txt")
	bin := fake(t, `env > '`+out+`'; exit 2`)
	env := fakeEnv(t, bin)
	if err := Run(context.Background(), env, Case{Name: "probe", Config: "version: 1\n", WantExit: 2}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for _, want := range []string{"CI=true", "SHEARBOLT_DB_ARCHIVE=" + env.DBArchive, "SHEARBOLT_CACHE_DIR=" + env.CacheDir} {
		if !slices.Contains(lines, want) {
			t.Errorf("child environment lacks %s", want)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "GITHUB_STEP_SUMMARY=") || l == "SHEARBOLT_DB_ARCHIVE=/parent/value" || l == "CI=" {
			t.Errorf("child environment has %s", l)
		}
	}
}

// canned writes a fake binary that prints stdout and stderr verbatim and exits with code.
func canned(t *testing.T, stdout, stderr string, code int) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fake(t, fmt.Sprintf("cat '%s/stdout'; cat '%s/stderr' >&2; exit %d", dir, dir, code))
}

func runJSON(t *testing.T, r model.Run) string {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func finding(id, pkg, version string, action model.Action, reasons ...string) model.Finding {
	return model.Finding{ID: id, Package: pkg, Version: version, Decision: model.Decision{Action: action, Reasons: reasons}}
}

const otherJquery = "GHSA-gxr4-xjj5-5px2"

// output is what a fake prints and how it exits.
type output struct {
	stdout, stderr string
	code           int
	signal         bool // the fake kills itself instead of exiting
}

// perfectOutputs are canned outputs that pass each case; the defects below change exactly one thing.
func perfectOutputs(t *testing.T) map[string]func() (model.Run, output) {
	lodash := finding("GHSA-35jh-r3h4-6jhm", "lodash", "4.17.20", model.Block, "fix available")
	kev := finding(kevID, "jquery", "3.4.1", model.Block, "known exploited (CISA KEV)")
	other := finding(otherJquery, "jquery", "3.4.1", model.Warn, "fix available, but policy.blockFixable is never")
	return map[string]func() (model.Run, output){
		"vulnerable": func() (model.Run, output) {
			return model.Run{Verdict: "block", ExitCode: 1, DBArchive: "/cache/db.tar.zst", Findings: []model.Finding{lodash}}, output{code: 1}
		},
		"clean": func() (model.Run, output) {
			return model.Run{Verdict: "pass", ExitCode: 0, Targets: []model.TargetResult{{Name: "repo", Status: "scanned", Packages: 1}}}, output{code: 0}
		},
		"kev": func() (model.Run, output) {
			return model.Run{Verdict: "block", ExitCode: 1, Findings: []model.Finding{kev, other}}, output{code: 1}
		},
		"expired-exception": func() (model.Run, output) {
			return model.Run{Verdict: "block", ExitCode: 1, Findings: []model.Finding{kev, other},
				Warnings:   []model.Diagnostic{{Code: "EXCEPTION_EXPIRED"}},
				Exceptions: []model.ExceptionStatus{{ID: kevID, Status: "expired"}}}, output{code: 1}
		},
		"in-date-exception": func() (model.Run, output) {
			excepted := kev
			excepted.Decision = model.Decision{Action: model.Excepted, Reasons: []string{"exception approved"}}
			return model.Run{Verdict: "pass", ExitCode: 0, Findings: []model.Finding{excepted, other},
				Exceptions: []model.ExceptionStatus{{ID: kevID, Status: "applied"}}}, output{code: 0}
		},
	}
}

const brokenConfigStderr = "shearbolt scan: invalid configuration in shearbolt.yaml:\n  - line 8: unknown field \"colour\" (check its spelling and indentation)\n"

func caseNamed(t *testing.T, name string) Case {
	t.Helper()
	for _, c := range Cases(time.Now()) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no case %s", name)
	return Case{}
}

func binaryFor(t *testing.T, o output) string {
	t.Helper()
	if o.signal {
		return fake(t, "kill -9 $$")
	}
	return canned(t, o.stdout, o.stderr, o.code)
}

// The perfect canned outputs pass, so each defect below is the only reason its case fails.
func TestPerfectCannedOutputsPass(t *testing.T) {
	for name, perfect := range perfectOutputs(t) {
		t.Run(name, func(t *testing.T) {
			r, o := perfect()
			o.stdout = runJSON(t, r)
			if err := Run(context.Background(), fakeEnv(t, binaryFor(t, o)), caseNamed(t, name)); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("broken-config", func(t *testing.T) {
		if err := Run(context.Background(), fakeEnv(t, canned(t, "", brokenConfigStderr, 2)), caseNamed(t, "broken-config")); err != nil {
			t.Fatal(err)
		}
	})
}

// Each content check can fail: an output with the case's expected exit code that is wrong in
// exactly one way fails its case.
func TestCannedOutputsWrongInOneWayFail(t *testing.T) {
	perfect := perfectOutputs(t)
	type defect struct {
		name, caseName string
		change         func(r *model.Run, o *output)
	}
	defects := []defect{
		{"no findings at all", "vulnerable", func(r *model.Run, _ *output) { r.Findings = []model.Finding{} }},
		{"lodash blocking without dbArchive", "vulnerable", func(r *model.Run, _ *output) { r.DBArchive = "" }},
		{"lodash only warned", "vulnerable", func(r *model.Run, _ *output) { r.Findings[0].Decision.Action = model.Warn }},
		{"verdict pass with exit 1", "vulnerable", func(r *model.Run, _ *output) { r.Verdict = "pass" }},
		{"run.json exitCode differs from the exit code", "vulnerable", func(r *model.Run, _ *output) { r.ExitCode = 0 }},
		{"signal death", "vulnerable", func(_ *model.Run, o *output) { o.signal = true }},
		{"target skipped", "clean", func(r *model.Run, _ *output) { r.Targets[0].Status = "skipped" }},
		{"no packages", "clean", func(r *model.Run, _ *output) { r.Targets[0].Packages = 0 }},
		{"a different blocking finding", "kev", func(r *model.Run, _ *output) {
			r.Findings[0].Decision.Action, r.Findings[1].Decision.Action = model.Warn, model.Block
		}},
		{"a second blocking finding", "kev", func(r *model.Run, _ *output) { r.Findings[1].Decision.Action = model.Block }},
		{"blocking without the known exploited reason", "kev", func(r *model.Run, _ *output) { r.Findings[0].Decision.Reasons = []string{"fix available"} }},
		{"no EXCEPTION_EXPIRED warning", "expired-exception", func(r *model.Run, _ *output) { r.Warnings = []model.Diagnostic{} }},
		{"exception not expired", "expired-exception", func(r *model.Run, _ *output) { r.Exceptions[0].Status = "unused" }},
		{"the KEV finding not blocking", "expired-exception", func(r *model.Run, _ *output) {
			r.Findings[0].Decision.Action, r.Findings[1].Decision.Action = model.Warn, model.Block
		}},
		{"the KEV finding not excepted", "in-date-exception", func(r *model.Run, _ *output) { r.Findings[0].Decision.Action = model.Warn }},
		{"exception not applied", "in-date-exception", func(r *model.Run, _ *output) { r.Exceptions[0].Status = "unused" }},
	}
	for _, d := range defects {
		t.Run(d.caseName+"/"+d.name, func(t *testing.T) {
			r, o := perfect[d.caseName]()
			d.change(&r, &o)
			o.stdout = runJSON(t, r)
			if err := Run(context.Background(), fakeEnv(t, binaryFor(t, o)), caseNamed(t, d.caseName)); err == nil {
				t.Fatal("the case passed")
			}
		})
	}
	brokenRun := runJSON(t, model.Run{Verdict: "error", ExitCode: 2})
	for name, o := range map[string]output{
		"prints run.json":                           {stdout: brokenRun, stderr: brokenConfigStderr, code: 2},
		"stderr does not name the field":            {stderr: "shearbolt scan: invalid configuration in shearbolt.yaml:\n  - line 8: something\n", code: 2},
		"stderr does not say invalid configuration": {stderr: "shearbolt scan: parse shearbolt.yaml: field colour not found\n", code: 2},
		"signal death":                              {signal: true},
	} {
		t.Run("broken-config/"+name, func(t *testing.T) {
			if err := Run(context.Background(), fakeEnv(t, binaryFor(t, o)), caseNamed(t, "broken-config")); err == nil {
				t.Fatal("the case passed")
			}
		})
	}
}
