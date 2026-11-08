// Package behaviour runs black-box release checks against a built shearbolt binary (spec section 14).
//
// The release workflow runs these cases against the candidate binary on the pinned database
// snapshot (candidate_test.go, build tag behaviour). The cases are written so that a binary with
// a constant answer cannot pass them (behaviour_test.go).
package behaviour

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/model"
)

// Case is one invocation of `<binary> scan --ci --json --config <cfg> --output-dir <dir>` and its expected outcome.
type Case struct {
	Name     string
	Config   string                                    // shearbolt.yaml; "FIXTURES" is replaced by Env.Fixtures
	WantExit int                                       // the expected process exit code
	Check    func(run *model.Run, stderr string) error // run is nil when stdout is not run.json
}

// Env says which binary to check and where its inputs and outputs live.
type Env struct {
	Binary    string // the shearbolt binary under test
	DBArchive string // the pinned Grype database archive (SHEARBOLT_DB_ARCHIVE)
	CacheDir  string // the Shearbolt cache (SHEARBOLT_CACHE_DIR); each scan imports the archive here
	Fixtures  string // testdata/projects
	WorkDir   string // a folder for configs and evidence
}

const (
	kevID           = "GHSA-jpcq-cgw6-v4j6" // jquery 3.4.1, known exploited through CVE-2020-11023
	snapshotPolicy  = "policy:\n  maxDatabaseAgeHours: 1000000\n"
	kevPolicy       = "policy:\n  blockFixable: never\n  blockEpssAtLeast: 1\n  maxDatabaseAgeHours: 1000000\n"
	expiredDate     = "2026-01-01"
	inDateAfterDays = 30
)

func config(project, policy, extra string) string {
	return "version: 1\nproject: behaviour\ntargets:\n  - name: repo\n    source: \"FIXTURES/" + project + "\"\n" + policy + extra
}

func kevException(expires string) string {
	return "exceptions:\n  - id: " + kevID + "\n    package: jquery@3.4.1\n    reason: behaviour test of exception expiry\n    approvedBy: release workflow\n    expires: " + expires + "\n"
}

// Cases returns the release cases; today dates the in-date exception.
func Cases(today time.Time) []Case {
	return []Case{
		{Name: "vulnerable", Config: config("npm-vulnerable", snapshotPolicy, ""), WantExit: 1, Check: func(run *model.Run, _ string) error {
			if err := isRun(run, "block"); err != nil {
				return err
			}
			if !slices.ContainsFunc(run.Findings, func(f model.Finding) bool {
				return f.Package == "lodash" && f.Version == "4.17.20" && f.Decision.Action == model.Block
			}) {
				return errors.New("lodash 4.17.20 is not blocked")
			}
			if run.DBArchive == "" {
				return errors.New("dbArchive is empty, so the pinned snapshot was not used")
			}
			return nil
		}},
		{Name: "clean", Config: config("npm-clean", snapshotPolicy, ""), WantExit: 0, Check: func(run *model.Run, _ string) error {
			if err := isRun(run, "pass"); err != nil {
				return err
			}
			i := slices.IndexFunc(run.Targets, func(t model.TargetResult) bool { return t.Name == "repo" })
			if i < 0 || run.Targets[i].Status != "scanned" || run.Targets[i].Packages == 0 {
				return fmt.Errorf("target repo is not scanned with packages: %+v", run.Targets)
			}
			return nil
		}},
		{Name: "broken-config", Config: config("npm-clean", snapshotPolicy, "colour: red\n"), WantExit: 2, Check: func(run *model.Run, stderr string) error {
			if run != nil {
				return errors.New("printed run.json for an invalid configuration")
			}
			if !strings.Contains(stderr, "invalid configuration") || !strings.Contains(stderr, "colour") {
				return fmt.Errorf("stderr does not report the unknown field colour as invalid configuration: %q", stderr)
			}
			return nil
		}},
		{Name: "kev", Config: config("npm-kev", kevPolicy, ""), WantExit: 1, Check: func(run *model.Run, _ string) error {
			if err := isRun(run, "block"); err != nil {
				return err
			}
			var blocking []model.Finding
			for _, f := range run.Findings {
				if f.Decision.Action == model.Block {
					blocking = append(blocking, f)
				}
			}
			if len(blocking) != 1 || blocking[0].ID != kevID ||
				!slices.ContainsFunc(blocking[0].Decision.Reasons, func(r string) bool { return strings.Contains(r, "known exploited") }) {
				return fmt.Errorf("blocking findings = %+v, want only %s with a known exploited reason", blocking, kevID)
			}
			return nil
		}},
		{Name: "expired-exception", Config: config("npm-kev", kevPolicy, kevException(expiredDate)), WantExit: 1, Check: func(run *model.Run, _ string) error {
			if err := isRun(run, "block"); err != nil {
				return err
			}
			if !slices.ContainsFunc(run.Warnings, func(d model.Diagnostic) bool { return d.Code == "EXCEPTION_EXPIRED" }) {
				return fmt.Errorf("no EXCEPTION_EXPIRED warning: %+v", run.Warnings)
			}
			if len(run.Exceptions) == 0 || run.Exceptions[0].Status != "expired" {
				return fmt.Errorf("exceptions = %+v, want the first one expired", run.Exceptions)
			}
			if action(run, kevID) != model.Block {
				return fmt.Errorf("%s is %q, want block", kevID, action(run, kevID))
			}
			return nil
		}},
		// Control: the same exception in date passes, so the expired case fails only because of the expiry.
		{Name: "in-date-exception", Config: config("npm-kev", kevPolicy, kevException(today.AddDate(0, 0, inDateAfterDays).Format("2006-01-02"))), WantExit: 0, Check: func(run *model.Run, _ string) error {
			if err := isRun(run, "pass"); err != nil {
				return err
			}
			if action(run, kevID) != model.Excepted {
				return fmt.Errorf("%s is %q, want excepted", kevID, action(run, kevID))
			}
			if len(run.Exceptions) == 0 || run.Exceptions[0].Status != "applied" {
				return fmt.Errorf("exceptions = %+v, want the first one applied", run.Exceptions)
			}
			return nil
		}},
	}
}

func isRun(run *model.Run, verdict string) error {
	if run == nil {
		return errors.New("stdout is not run.json")
	}
	if run.Verdict != verdict {
		return fmt.Errorf("verdict %q, want %q", run.Verdict, verdict)
	}
	return nil
}

// action is the decision for the finding with the given ID, or "" if there is none.
func action(run *model.Run, id string) model.Action {
	for _, f := range run.Findings {
		if f.ID == id {
			return f.Decision.Action
		}
	}
	return ""
}

// Run executes one case and returns nil when the binary behaved as expected. The child environment is
// the parent's minus GITHUB_STEP_SUMMARY, plus CI=true, SHEARBOLT_DB_ARCHIVE and SHEARBOLT_CACHE_DIR.
func Run(ctx context.Context, env Env, c Case) error {
	if strings.ContainsAny(env.Fixtures, "\"\\\n") {
		return fmt.Errorf("%s: the fixtures path %q cannot be written into a config", c.Name, env.Fixtures)
	}
	dir, err := os.MkdirTemp(env.WorkDir, c.Name+"-")
	if err != nil {
		return err
	}
	cfg := filepath.Join(dir, "shearbolt.yaml")
	if err := os.WriteFile(cfg, []byte(strings.ReplaceAll(c.Config, "FIXTURES", env.Fixtures)), 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, env.Binary, "scan", "--ci", "--json", "--config", cfg, "--output-dir", filepath.Join(dir, "evidence"))
	cmd.Dir = dir
	cmd.Env = childEnv(env)
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() < 0 {
			return fmt.Errorf("%s: running %s: %w%s", c.Name, env.Binary, err, tail(stderr.String()))
		}
		code = exit.ExitCode()
	}
	var run *model.Run
	if out := bytes.TrimSpace(stdout.Bytes()); bytes.HasPrefix(out, []byte("{")) {
		if r := new(model.Run); json.Unmarshal(out, r) == nil {
			run = r
		}
	}
	if code != c.WantExit {
		return fmt.Errorf("%s: exit %d, want %d%s", c.Name, code, c.WantExit, tail(stderr.String()))
	}
	if run != nil && run.ExitCode != code {
		return fmt.Errorf("%s: run.json says exitCode %d, but the process exited %d", c.Name, run.ExitCode, code)
	}
	if c.Check != nil {
		if err := c.Check(run, stderr.String()); err != nil {
			return fmt.Errorf("%s: %w%s", c.Name, err, tail(stderr.String()))
		}
	}
	return nil
}

func childEnv(env Env) []string {
	var out []string
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "GITHUB_STEP_SUMMARY", "CI", "SHEARBOLT_DB_ARCHIVE", "SHEARBOLT_CACHE_DIR":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CI=true", "SHEARBOLT_DB_ARCHIVE="+env.DBArchive, "SHEARBOLT_CACHE_DIR="+env.CacheDir)
}

// tail formats the end of stderr for an error message.
func tail(stderr string) string {
	if stderr = strings.TrimSpace(stderr); stderr == "" {
		return ""
	}
	if len(stderr) > 2000 {
		stderr = "..." + stderr[len(stderr)-2000:]
	}
	return "\nstderr:\n" + stderr
}
