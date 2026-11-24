package zosia

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
)

const highPolicy = `{"blockKnownExploited":true,"blockEpssAtLeast":0.1,"blockFixable":"high","maxDatabaseAgeHours":48,"maxExceptionDays":90}`

func policyRequest(url string) PolicyRequest {
	return PolicyRequest{URL: url, Secret: "zosia_s3cret", Client: "shearbolt", Version: "1.11.0",
		Server: "https://github.com", Repository: "acme/app", ConfigPath: "apps/web/shearbolt.yaml"}
}

// answering serves each request with the next of the answers (the last one again after that)
// and counts the requests.
func answering(t *testing.T, answers ...func(http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1)) - 1
		answers[min(i, len(answers)-1)](w)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func status(code int, body string, header ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is no PolicyError", err)
	}
	if strings.Contains(pe.Message, "zosia_s3cret") {
		t.Fatalf("the error names the secret: %s", pe.Message)
	}
	return pe.Reason
}

func TestFetchPolicyAsksForTheProjectAndReadsThePolicy(t *testing.T) {
	var auth, agent, method, path string
	var asked map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth, agent, method, path = req.Header.Get("Authorization"), req.Header.Get("User-Agent"), req.Method, req.URL.RequestURI()
		json.NewDecoder(req.Body).Decode(&asked)
		io.WriteString(w, `{"schema":"zosia.policy/1","version":7,"scope":"repository","policy":`+highPolicy+`,"newer":true}`)
	}))
	defer srv.Close()
	got, err := FetchPolicy(context.Background(), srv.Client(), fast, policyRequest(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	want := config.DefaultPolicy()
	want.BlockFixable = "high"
	if got.Version != 7 || got.Scope != "repository" || got.Policy == nil || *got.Policy != want {
		t.Fatalf("answer %+v, policy %+v", got, got.Policy)
	}
	if method != "POST" || path != "/api/v1/repo/policy" || auth != "Bearer zosia_s3cret" || agent != "shearbolt/1.11.0" {
		t.Errorf("request: %s %s, Authorization %q, User-Agent %q", method, path, auth, agent)
	}
	// What names the project travels in the body, and nothing else does.
	if len(asked) != 3 || asked["server"] != "https://github.com" || asked["repository"] != "acme/app" || asked["configPath"] != "apps/web/shearbolt.yaml" {
		t.Errorf("body %v", asked)
	}
}

func TestFetchPolicyWithoutAPolicyInZosia(t *testing.T) {
	srv, _ := answering(t, status(200, `{"schema":"zosia.policy/1","version":3,"policy":null}`))
	got, err := FetchPolicy(context.Background(), srv.Client(), fast, policyRequest(srv.URL))
	if err != nil || got.Policy != nil || got.Version != 3 || got.Scope != "" {
		t.Fatalf("answer %+v, error %v", got, err)
	}
}

func TestFetchPolicySaysWhyThereIsNoAnswer(t *testing.T) {
	ok := status(200, `{"schema":"zosia.policy/1","version":1,"scope":"project","policy":`+highPolicy+`}`)
	for name, c := range map[string]struct {
		answers  []func(http.ResponseWriter)
		reason   string
		requests int32
	}{
		"a refused secret":                  {[]func(http.ResponseWriter){status(401, `{"code":"BAD_SECRET","message":"This secret is unknown or was revoked."}`)}, PolicyRefused, 1},
		"forbidden":                         {[]func(http.ResponseWriter){status(403, "")}, PolicyRefused, 1},
		"a server error, twice":             {[]func(http.ResponseWriter){status(500, "")}, PolicyUnreachable, 2},
		"a server error, then the policy":   {[]func(http.ResponseWriter){status(502, ""), ok}, "", 2},
		"busy, then the policy":             {[]func(http.ResponseWriter){status(429, "", "Retry-After", "1"), ok}, "", 2},
		"the hourly limit":                  {[]func(http.ResponseWriter){status(429, `{"code":"RATE_LIMITED","message":"too often"}`, "Retry-After", "600"), ok}, PolicyUnreachable, 1},
		"a Zosia without this API":          {[]func(http.ResponseWriter){status(404, "<html>")}, PolicyInvalid, 1},
		"no JSON":                           {[]func(http.ResponseWriter){status(200, "<html>")}, PolicyInvalid, 1},
		"another document":                  {[]func(http.ResponseWriter){status(200, `{"schema":"zosia.settings/1","version":1,"policy":null}`)}, PolicyInvalid, 1},
		"no version":                        {[]func(http.ResponseWriter){status(200, `{"schema":"zosia.policy/1","policy":null}`)}, PolicyInvalid, 1},
		"a version that is no whole number": {[]func(http.ResponseWriter){status(200, `{"schema":"zosia.policy/1","version":1.5,"policy":null}`)}, PolicyInvalid, 1},
		"no policy field":                   {[]func(http.ResponseWriter){status(200, `{"schema":"zosia.policy/1","version":1}`)}, PolicyInvalid, 1},
		"a policy without a scope":          {[]func(http.ResponseWriter){status(200, `{"schema":"zosia.policy/1","version":1,"policy":`+highPolicy+`}`)}, PolicyInvalid, 1},
		"one bad value in the policy":       {[]func(http.ResponseWriter){status(200, `{"schema":"zosia.policy/1","version":1,"scope":"repository","policy":`+strings.Replace(highPolicy, `"high"`, `"severe"`, 1)+`}`)}, PolicyInvalid, 1},
		"an answer of more than 64 KiB":     {[]func(http.ResponseWriter){status(200, `{"schema":"zosia.policy/1","version":1,"policy":null,"pad":"`+strings.Repeat("x", 64<<10)+`"}`)}, PolicyUnreachable, 1},
	} {
		srv, n := answering(t, c.answers...)
		got, err := FetchPolicy(context.Background(), srv.Client(), fast, policyRequest(srv.URL))
		if c.reason == "" {
			if err != nil || got.Policy == nil {
				t.Errorf("%s: answer %+v, error %v", name, got, err)
			}
		} else if err == nil || reason(t, err) != c.reason || got.Policy != nil {
			t.Errorf("%s: answer %+v, error %v, want %s", name, got, err, c.reason)
		}
		if n.Load() != c.requests {
			t.Errorf("%s: %d requests, want %d", name, n.Load(), c.requests)
		}
	}
}

func TestPolicyAnswerCountsNamesExactlyAsWritten(t *testing.T) {
	// encoding/json alone would take these for "policy" and "blockFixable"; Zosia reads exact names.
	got, err := ParsePolicyAnswer([]byte(`{"schema":"zosia.policy/1","version":1,"policy":null,"POLICY":` + strings.Replace(highPolicy, `"high"`, `"never"`, 1) + `,"scope":"repository"}`))
	if err != nil || got.Policy != nil {
		t.Errorf("a second POLICY: answer %+v, policy %+v, error %v", got, got.Policy, err)
	}
	pol, err := ParsePolicy([]byte(strings.Replace(highPolicy, `}`, `,"BLOCKFIXABLE":"never","BlockKnownExploited":false}`, 1)))
	if err != nil || pol.BlockFixable != "high" || !pol.BlockKnownExploited {
		t.Errorf("names in another case next to the real ones: %+v, %v", pol, err)
	}
	for _, doc := range []string{
		`{"Schema":"zosia.policy/1","version":1,"policy":null}`,
		`{"schema":"zosia.policy/1","Version":1,"policy":null}`,
		`{"schema":["zosia.policy/1"],"version":1,"policy":null}`,
		`[]`, `null`, `"zosia.policy/1"`,
	} {
		if got, err := ParsePolicyAnswer([]byte(doc)); err == nil {
			t.Errorf("accepted %s: %+v", doc, got)
		}
	}
}

func TestPolicyAnswerHasUpperLimitsAndTakesALaterScope(t *testing.T) {
	doc := func(scope, policy string) []byte {
		return []byte(`{"schema":"zosia.policy/1","version":1,"scope":` + scope + `,"policy":` + policy + `}`)
	}
	// Zosia saves at most these, and a larger age would overflow the limit it is compared with.
	for name, policy := range map[string]string{
		"hours beyond Zosia's limit":     strings.Replace(highPolicy, `"maxDatabaseAgeHours":48`, `"maxDatabaseAgeHours":87601`, 1),
		"hours that overflow a duration": strings.Replace(highPolicy, `"maxDatabaseAgeHours":48`, `"maxDatabaseAgeHours":2562048`, 1),
		"days beyond Zosia's limit":      strings.Replace(highPolicy, `"maxExceptionDays":90`, `"maxExceptionDays":3651`, 1),
		"a number no int holds":          strings.Replace(highPolicy, `"maxExceptionDays":90`, `"maxExceptionDays":99999999999999999999`, 1),
	} {
		if got, err := ParsePolicyAnswer(doc(`"repository"`, policy)); err == nil {
			t.Errorf("%s: accepted %+v", name, got.Policy)
		}
	}
	atLimit := strings.Replace(strings.Replace(highPolicy, `"maxDatabaseAgeHours":48`, `"maxDatabaseAgeHours":87600`, 1), `"maxExceptionDays":90`, `"maxExceptionDays":3650`, 1)
	if _, err := ParsePolicyAnswer(doc(`"repository"`, atLimit)); err != nil {
		t.Errorf("at the limits: %v", err)
	}
	// The scope is only shown, so one a later Zosia adds does not make the policy unusable.
	if got, err := ParsePolicyAnswer(doc(`"account"`, highPolicy)); err != nil || got.Scope != "account" {
		t.Errorf("a later scope: %+v, %v", got, err)
	}
	for _, scope := range []string{`""`, `"Two Words"`, `7`, `null`, `"` + strings.Repeat("x", 40) + `"`} {
		if got, err := ParsePolicyAnswer(doc(scope, highPolicy)); err == nil {
			t.Errorf("scope %s: accepted %+v", scope, got)
		}
	}
}

func TestFetchPolicyDoesNotFollowARedirect(t *testing.T) {
	elsewhere, asked := answering(t, status(200, `{"schema":"zosia.policy/1","version":9,"scope":"repository","policy":`+strings.Replace(highPolicy, `"high"`, `"never"`, 1)+`}`))
	for _, code := range []int{301, 302, 307, 308} {
		srv, _ := answering(t, status(code, "", "Location", elsewhere.URL+"/api/v1/repo/policy"))
		client := srv.Client()
		client.CheckRedirect = NoRedirect
		got, err := FetchPolicy(context.Background(), client, fast, policyRequest(srv.URL))
		if err == nil || reason(t, err) != PolicyInvalid || got.Policy != nil {
			t.Errorf("%d: answer %+v, policy %+v, error %v", code, got, got.Policy, err)
		}
	}
	if asked.Load() != 0 {
		t.Errorf("the other address was asked %d times", asked.Load())
	}
}

func TestFetchPolicyGivesUpInTime(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	brief := Policy{Total: 150 * time.Millisecond, Attempts: 2, Pause: 10 * time.Millisecond, MaxRetryAfter: time.Second}
	start := time.Now()
	_, err := FetchPolicy(context.Background(), srv.Client(), brief, policyRequest(srv.URL))
	var pe *PolicyError
	if !errors.As(err, &pe) || pe.Reason != PolicyUnreachable || pe.Message != "timeout" || time.Since(start) > 2*time.Second {
		t.Fatalf("a server that does not answer: %v after %s", err, time.Since(start))
	}
	// Ctrl-C: no second attempt, and no long wait.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start = time.Now()
	if _, err := FetchPolicy(ctx, srv.Client(), fast, policyRequest(srv.URL)); err == nil || reason(t, err) != PolicyUnreachable || time.Since(start) > 2*time.Second {
		t.Fatalf("cancelled: %v after %s", err, time.Since(start))
	}
}

func TestFetchPolicyKeepsTheServersWordsShort(t *testing.T) {
	srv, _ := answering(t, status(401, `{"code":"BAD_SECRET","message":"`+strings.Repeat("ä", 20000)+`"}`))
	_, err := FetchPolicy(context.Background(), srv.Client(), fast, policyRequest(srv.URL))
	var pe *PolicyError
	if !errors.As(err, &pe) || pe.Reason != PolicyRefused || len([]rune(pe.Message)) > 201 {
		t.Fatalf("reason %v, %d characters", err, len([]rune(pe.Message)))
	}
}

func TestFetchPolicyWhenZosiaIsDown(t *testing.T) {
	srv, _ := answering(t, status(200, ""))
	url := srv.URL
	srv.Close()
	if _, err := FetchPolicy(context.Background(), &http.Client{}, fast, policyRequest(url)); err == nil || reason(t, err) != PolicyUnreachable {
		t.Fatalf("error %v", err)
	}
	if _, err := FetchPolicy(context.Background(), &http.Client{}, fast, policyRequest("http://exa mple")); err == nil || reason(t, err) != PolicyUnreachable {
		t.Fatalf("a URL that is none: error %v", err)
	}
}

// The fixture is also in Zosia's repository (tests/fixtures/shearbolt-policy.json), where a test
// reads it the same way: the defaults and the rules for the five values cannot drift apart unnoticed.
func TestPolicyRulesMatchTheFixtureSharedWithZosia(t *testing.T) {
	data, err := os.ReadFile("testdata/shearbolt-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Defaults json.RawMessage   `json:"defaults"`
		Valid    []json.RawMessage `json:"valid"`
		Invalid  []struct {
			Why    string          `json:"why"`
			Policy json.RawMessage `json:"policy"`
		} `json:"invalid"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if got, err := ParsePolicy(fixture.Defaults); err != nil || got != config.DefaultPolicy() {
		t.Errorf("the fixture's defaults are %+v (error %v), Shearbolt's are %+v", got, err, config.DefaultPolicy())
	}
	if len(fixture.Valid) == 0 || len(fixture.Invalid) == 0 {
		t.Fatal("the fixture has no cases")
	}
	for _, v := range fixture.Valid {
		if _, err := ParsePolicy(v); err != nil {
			t.Errorf("refused %s: %v", v, err)
		}
	}
	for _, c := range fixture.Invalid {
		if got, err := ParsePolicy(c.Policy); err == nil {
			t.Errorf("accepted a policy where %s: %+v", c.Why, got)
		}
	}
}
