package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/testutil"
	"github.com/klar-web-services/shearbolt/internal/zosia"
)

// A project with one fixable finding: the default policy blocks it, "blockFixable: never" lets it pass.
const (
	optedIn = "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\nzosia:\n  policy: true\n"
	lenient = "policy:\n  blockFixable: never\n"
)

func fixable() string {
	return testutil.GrypeJSON(time.Now(), testutil.Match{ID: "GHSA-1", Package: "lodash", Version: "4.17.20", FixState: "fixed", FixVersions: []string{"4.17.21"}})
}

func policyProject(t *testing.T, yaml string) string {
	t.Helper()
	cfg := filepath.Join(projectDir(t), "shearbolt.yaml")
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

type fetched struct {
	answer zosia.PolicyAnswer
	err    error
}

// policyScan runs a scan whose policy fetch is answered by f; a nil f fails the test when a fetch
// is made. It returns the exit code, run.json, the text report of a second, identical scan, and
// the requests.
func policyScan(t *testing.T, vars map[string]string, f *fetched, args ...string) (int, model.Run, string, []zosia.PolicyRequest) {
	t.Helper()
	var asked []zosia.PolicyRequest
	deps := func() deps {
		all := map[string]string{"FAKE_GRYPE_JSON": fixable()}
		for k, v := range vars {
			all[k] = v
		}
		d := testDeps(t, all)
		d.version = "1.11.0"
		d.sendZosia = func(context.Context, zosia.Request) (zosia.Result, error) { return zosia.Result{}, nil }
		d.fetchPolicy = func(_ context.Context, r zosia.PolicyRequest) (zosia.PolicyAnswer, error) {
			if f == nil {
				t.Error("asked Zosia for a policy")
				return zosia.PolicyAnswer{}, nil
			}
			asked = append(asked, r)
			return f.answer, f.err
		}
		return d
	}
	kept := evidence(t)
	code, out, errb := scanCmd(t, deps(), append(args, "--output-dir", kept, "--json")...)
	filepath.WalkDir(kept, func(path string, e os.DirEntry, err error) error {
		if data, readErr := os.ReadFile(path); err == nil && !e.IsDir() && readErr == nil && strings.Contains(string(data), "zosia_x") {
			t.Errorf("the evidence file %s names the secret", path)
		}
		return nil
	})
	var r model.Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not run.json: %v\n%s\nstderr: %s", err, out, errb)
	}
	textCode, text, textErr := scanCmd(t, deps(), append(args, "--output-dir", evidence(t))...)
	if textCode != code {
		t.Fatalf("the same scan exits %d and then %d", code, textCode)
	}
	for _, s := range []string{out, errb, text, textErr} {
		if strings.Contains(s, "zosia_x") {
			t.Fatalf("the output names the secret:\n%s", s)
		}
	}
	return code, r, text, asked[:len(asked)/2]
}

func version(n int) *int { return &n }

func sourceOf(r model.Run) string {
	data, _ := json.Marshal(r.PolicySource)
	return string(data)
}

func warningCodes(r model.Run) string {
	var codes []string
	for _, w := range r.Warnings {
		codes = append(codes, w.Code)
	}
	return strings.Join(codes, ",")
}

func TestScanUsesZosiasPolicyInPlaceOfTheFiles(t *testing.T) {
	ci := githubEnv(map[string]string{"ZOSIA_SECRET": "zosia_x"})
	loose := config.DefaultPolicy()
	loose.BlockFixable = "never"

	// The file blocks, Zosia's policy lets the build pass.
	cfg := policyProject(t, optedIn)
	code, r, text, asked := policyScan(t, ci, &fetched{answer: zosia.PolicyAnswer{Version: 7, Scope: "repository", Policy: &loose}}, "--ci", "--config", cfg)
	if code != 0 || r.Policy != loose || sourceOf(r) != `{"from":"zosia","zosia":"used","version":7,"scope":"repository"}` || len(r.Warnings) != 0 {
		t.Fatalf("looser: exit %d, policy %+v, source %s, warnings %+v", code, r.Policy, sourceOf(r), r.Warnings)
	}
	if !strings.Contains(text, "Policy: from Zosia (repository policy, version 7)\n") {
		t.Errorf("the report does not name the policy's source:\n%s", text)
	}
	want := zosia.PolicyRequest{URL: zosia.DefaultURL, Secret: "zosia_x", Client: "shearbolt", Version: "1.11.0",
		Server: "https://github.com", Repository: "o/r", ConfigPath: "shearbolt.yaml"}
	if len(asked) != 1 || asked[0] != want {
		t.Errorf("asked %+v, want once %+v", asked, want)
	}

	// ZOSIA_URL is for tests: a policy from there is used, and the report says where it came from.
	ci["ZOSIA_URL"] = "https://user:hunter2@zosia.test:8443/base"
	code, r, text, asked = policyScan(t, ci, &fetched{answer: zosia.PolicyAnswer{Version: 7, Scope: "repository", Policy: &loose}}, "--ci", "--config", cfg)
	if code != 0 || warningCodes(r) != "ZOSIA_URL_SET" || !strings.Contains(r.Warnings[0].Message, "zosia.test:8443") || strings.Contains(text+r.Warnings[0].Message, "hunter2") ||
		len(asked) != 1 || asked[0].URL != ci["ZOSIA_URL"] {
		t.Fatalf("another Zosia: exit %d, warnings %+v, asked %+v", code, r.Warnings, asked)
	}
	delete(ci, "ZOSIA_URL")

	// The file lets it pass, Zosia's policy blocks: Zosia wins both ways, and the file's block is not mixed in.
	strict := config.DefaultPolicy()
	code, r, text, _ = policyScan(t, ci, &fetched{answer: zosia.PolicyAnswer{Version: 8, Scope: "project", Policy: &strict}}, "--ci", "--config", policyProject(t, optedIn+lenient))
	if code != 1 || r.Policy != strict || sourceOf(r) != `{"from":"zosia","zosia":"used","version":8,"scope":"project"}` {
		t.Fatalf("stricter: exit %d, policy %+v, source %s", code, r.Policy, sourceOf(r))
	}
	if !strings.Contains(text, "Policy: from Zosia (project policy, version 8)\n") {
		t.Errorf("the report does not name the policy's source:\n%s", text)
	}
}

func TestScanUsesTheFilesPolicyWhenZosiaGivesNone(t *testing.T) {
	ci := githubEnv(map[string]string{"ZOSIA_SECRET": "zosia_x"})
	const fellBack = "Policy: shearbolt.yaml (Zosia's policy could not be fetched, see the warnings)\n"
	for name, c := range map[string]struct {
		vars    map[string]string
		fetch   *fetched
		args    []string
		source  string
		warning string
		line    string
	}{
		"Zosia has no policy": {ci, &fetched{answer: zosia.PolicyAnswer{Version: 3}}, []string{"--ci"},
			`{"from":"file","zosia":"none","version":3}`, "", "Policy: shearbolt.yaml (Zosia has no policy for this project)\n"},
		"Zosia has none and knows nothing of the repository": {ci, &fetched{}, []string{"--ci"},
			`{"from":"file","zosia":"none","version":0}`, "", "Policy: shearbolt.yaml (Zosia has no policy for this project)\n"},
		"Zosia cannot be reached": {ci, &fetched{err: &zosia.PolicyError{Reason: zosia.PolicyUnreachable, Message: "connection refused"}}, []string{"--ci"},
			`{"from":"file","zosia":"unreachable"}`, "ZOSIA_POLICY_UNREACHABLE", fellBack},
		"Zosia refuses the secret": {ci, &fetched{err: &zosia.PolicyError{Reason: zosia.PolicyRefused, Message: "This secret is unknown or was revoked."}}, []string{"--ci"},
			`{"from":"file","zosia":"refused"}`, "ZOSIA_POLICY_REFUSED", fellBack},
		"Zosia's answer cannot be used": {ci, &fetched{err: &zosia.PolicyError{Reason: zosia.PolicyInvalid, Message: "the policy does not have all five values"}}, []string{"--ci"},
			`{"from":"file","zosia":"invalid"}`, "ZOSIA_POLICY_INVALID", fellBack},
		// Without the secret the scan is not connected to Zosia: the file's policy, and nothing said.
		"CI without a secret, as a fork's pull request": {githubEnv(nil), nil, []string{"--ci"},
			`{"from":"file"}`, "", ""},
		"a CI Shearbolt cannot name the repository of": {map[string]string{"ZOSIA_SECRET": "zosia_x"}, nil, []string{"--ci"},
			`{"from":"file","zosia":"no-ci"}`, "ZOSIA_POLICY_NO_CI", fellBack},
		"a local scan": {ci, nil, nil,
			`{"from":"file"}`, "", ""},
	} {
		// The file's policy lets the build pass, the default would block: so the exit code shows whose policy it was.
		code, r, text, _ := policyScan(t, c.vars, c.fetch, append(c.args, "--config", policyProject(t, optedIn+lenient))...)
		if code != 0 || r.Policy.BlockFixable != "never" || sourceOf(r) != c.source || warningCodes(r) != c.warning {
			t.Errorf("%s: exit %d, blockFixable %s, source %s, warnings %q; want 0, never, %s, %q", name, code, r.Policy.BlockFixable, sourceOf(r), warningCodes(r), c.source, c.warning)
		}
		if !strings.Contains(text, c.line) || c.line == "" && strings.Contains(text, "Policy:") {
			t.Errorf("%s: the report lacks %q, or names a policy's source where Zosia was not asked:\n%s", name, c.line, text)
		}
		if c.warning != "" && (!strings.Contains(r.Warnings[0].Message, "so this scan used the policy in shearbolt.yaml") || !strings.Contains(text, r.Warnings[0].Message)) {
			t.Errorf("%s: warning %q, report:\n%s", name, r.Warnings[0].Message, text)
		}
	}
}

// The real fetch, as the command wires it: over HTTP to ZOSIA_URL, and never on to another address.
func TestScanFetchesThePolicyOverHTTPAndFollowsNoRedirect(t *testing.T) {
	const never = `{"schema":"zosia.policy/1","version":4,"scope":"repository","policy":{"blockKnownExploited":true,"blockEpssAtLeast":0.1,"blockFixable":"never","maxDatabaseAgeHours":48,"maxExceptionDays":90}}`
	var auth, path string
	zosiaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth, path = req.Header.Get("Authorization"), req.URL.Path
		io.WriteString(w, never)
	}))
	defer zosiaSrv.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, zosiaSrv.URL+req.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	scan := func(url string) (int, model.Run) {
		d := testDeps(t, githubEnv(map[string]string{"ZOSIA_SECRET": "zosia_x", "ZOSIA_URL": url, "FAKE_GRYPE_JSON": fixable()}))
		d.sendZosia = func(context.Context, zosia.Request) (zosia.Result, error) { return zosia.Result{}, nil }
		code, out, errb := scanCmd(t, d, "--ci", "--config", policyProject(t, optedIn), "--output-dir", evidence(t), "--json")
		var r model.Run
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("stdout is not run.json: %v\n%s\nstderr: %s", err, out, errb)
		}
		return code, r
	}
	code, r := scan(zosiaSrv.URL)
	if code != 0 || sourceOf(r) != `{"from":"zosia","zosia":"used","version":4,"scope":"repository"}` || auth != "Bearer zosia_x" || path != "/api/v1/repo/policy" {
		t.Fatalf("exit %d, source %s, Authorization %q, path %q", code, sourceOf(r), auth, path)
	}
	auth = ""
	code, r = scan(redirecting.URL)
	if code != 1 || sourceOf(r) != `{"from":"file","zosia":"invalid"}` || warningCodes(r) != "ZOSIA_POLICY_INVALID" || auth != "" {
		t.Fatalf("redirected: exit %d, source %s, warnings %q, the other server saw %q", code, sourceOf(r), warningCodes(r), auth)
	}
}

func TestStaleDataUnderZosiasPolicyNamesZosia(t *testing.T) {
	ci := githubEnv(map[string]string{"ZOSIA_SECRET": "zosia_x", "FAKE_GRYPE_JSON": testutil.GrypeJSON(time.Now().Add(-72 * time.Hour))})
	fresh := config.DefaultPolicy() // 48 hours, where the file allows a week
	code, r, _, _ := policyScan(t, ci, &fetched{answer: zosia.PolicyAnswer{Version: 1, Scope: "repository", Policy: &fresh}}, "--ci", "--config", policyProject(t, optedIn+"policy:\n  maxDatabaseAgeHours: 168\n"))
	if code != 2 || len(r.Errors) != 1 || r.Errors[0].Code != "STALE_DATABASE" || !strings.Contains(r.Errors[0].Message, "maxDatabaseAgeHours in the policy from Zosia (48)") {
		t.Fatalf("exit %d, errors %+v", code, r.Errors)
	}
	code, r, _, _ = policyScan(t, ci, &fetched{}, "--ci", "--config", policyProject(t, optedIn+"policy:\n  maxDatabaseAgeHours: 24\n"))
	if code != 2 || len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Message, "older than policy.maxDatabaseAgeHours (24)") {
		t.Fatalf("the file's own limit: exit %d, errors %+v", code, r.Errors)
	}
}

// Since 1.13.0 a CI scan that has the secret asks Zosia, whatever shearbolt.yaml says about it: the
// `policy` line that 1.11.0 and 1.12.0 needed is read and means nothing.
func TestScanAsksZosiaWheneverItHasTheSecret(t *testing.T) {
	ci := githubEnv(map[string]string{"ZOSIA_SECRET": "zosia_x"})
	plain := strings.Replace(optedIn, "zosia:\n  policy: true\n", "", 1)
	never := config.DefaultPolicy()
	never.BlockFixable = "never"
	for name, yaml := range map[string]string{"no zosia block": plain, "policy: false": plain + "zosia:\n  policy: false\n", "policy: true": optedIn} {
		// The file's policy (the default) would block; Zosia's lets the build pass.
		code, r, text, asked := policyScan(t, ci, &fetched{answer: zosia.PolicyAnswer{Version: 2, Scope: "repository", Policy: &never}}, "--ci", "--config", policyProject(t, yaml))
		if code != 0 || len(asked) != 1 || sourceOf(r) != `{"from":"zosia","zosia":"used","version":2,"scope":"repository"}` || !strings.Contains(text, "Policy: from Zosia (repository policy, version 2)") {
			t.Errorf("%s: exit %d, asked %d times, source %s, report:\n%s", name, code, len(asked), sourceOf(r), text)
		}
		// Without the secret nothing is asked, and the file's policy blocks.
		code, r, text, _ = policyScan(t, githubEnv(nil), nil, "--ci", "--config", policyProject(t, yaml))
		if code != 1 || sourceOf(r) != `{"from":"file"}` || len(r.Warnings) != 0 || strings.Contains(text, "Policy:") {
			t.Errorf("%s, no secret: exit %d, source %s, warnings %+v, report:\n%s", name, code, sourceOf(r), r.Warnings, text)
		}
	}
	// Ad-hoc flags read no shearbolt.yaml, so they never ask either.
	code, r, text, _ := policyScan(t, ci, nil, "--ci", "--source", filepath.Dir(policyProject(t, optedIn)))
	if code != 1 || sourceOf(r) != `{"from":"default"}` || strings.Contains(text, "Policy:") {
		t.Errorf("ad hoc: exit %d, source %s, report:\n%s", code, sourceOf(r), text)
	}
}

func TestZosiasShorterExceptionLimitSetsAnExceptionAside(t *testing.T) {
	ci := githubEnv(map[string]string{"ZOSIA_SECRET": "zosia_x"})
	expires := time.Now().UTC().AddDate(0, 0, 60).Format("2006-01-02")
	yaml := optedIn + "exceptions:\n  - id: GHSA-1\n    package: lodash@4.17.20\n    reason: r\n    approvedBy: a\n    expires: " + expires + "\n"
	// The file alone accepts the exception (its limit is 90 days), and so does a run that fell back.
	code, r, _, _ := policyScan(t, ci, &fetched{err: &zosia.PolicyError{Reason: zosia.PolicyUnreachable, Message: "timeout"}}, "--ci", "--config", policyProject(t, yaml))
	if code != 0 || r.Exceptions[0].Status != "applied" {
		t.Fatalf("fallback: exit %d, exception %+v", code, r.Exceptions)
	}
	// Zosia's policy allows 30 days: the exception is set aside, the finding blocks, and the run still completes.
	short := config.DefaultPolicy()
	short.MaxExceptionDays = 30
	code, r, text, _ := policyScan(t, ci, &fetched{answer: zosia.PolicyAnswer{Version: 2, Scope: "repository", Policy: &short}}, "--ci", "--config", policyProject(t, yaml))
	if code != 1 || r.Verdict != "block" || r.Exceptions[0].Status != "over-limit" || warningCodes(r) != "EXCEPTION_BEYOND_LIMIT" {
		t.Fatalf("shorter limit: exit %d, verdict %s, exception %+v, warnings %q", code, r.Verdict, r.Exceptions, warningCodes(r))
	}
	if !strings.Contains(text, expires) || !strings.Contains(text, "maxExceptionDays (30)") {
		t.Errorf("the report does not name the exception's expiry and the limit:\n%s", text)
	}
	// A longer limit in Zosia does not lengthen the file's: such an exception is refused when the file is read.
	far := time.Now().UTC().AddDate(0, 0, 120).Format("2006-01-02")
	long := config.DefaultPolicy()
	long.MaxExceptionDays = 365
	d := testDeps(t, ci)
	d.fetchPolicy = func(context.Context, zosia.PolicyRequest) (zosia.PolicyAnswer, error) {
		t.Error("asked Zosia although the file is invalid")
		return zosia.PolicyAnswer{Version: 2, Scope: "repository", Policy: &long}, nil
	}
	exit, _, errb := scanCmd(t, d, "--ci", "--config", policyProject(t, strings.Replace(yaml, expires, far, 1)), "--output-dir", evidence(t))
	if exit != 2 || !strings.Contains(errb, "more than policy.maxExceptionDays (90) days away") {
		t.Fatalf("exit %d, stderr: %s", exit, errb)
	}
}
