package zosia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
)

// PolicySchema is the document Zosia answers a project that takes its policy from there.
const PolicySchema = "zosia.policy/1"

const (
	maxPolicyAnswer  = 64 << 10
	maxPolicyMessage = 200
)

// NoRedirect is the CheckRedirect for the client that fetches a policy: an answer that sends
// the request elsewhere is not followed, since a policy from another address is not Zosia's.
func NoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// PolicyRequest names the project as its reports do: the CI's server, the repository's path
// there, and the config's path in the repository.
type PolicyRequest struct {
	URL        string // base URL, such as https://zosiasec.com
	Secret     string
	Client     string
	Version    string
	Server     string
	Repository string
	ConfigPath string
}

// PolicyAnswer is what Zosia has for the project. Policy is nil when it has none; Version
// counts the changes to the repository's policies, and Scope says whose policy it is.
type PolicyAnswer struct {
	Version int
	Scope   string // repository | project
	Policy  *config.Policy
}

// Why a fetch brought no answer that can be used.
const (
	PolicyUnreachable = "unreachable"
	PolicyRefused     = "refused"
	PolicyInvalid     = "invalid"
)

// PolicyError says why, and in words for the report. It never holds the secret.
type PolicyError struct {
	Reason  string
	Message string
	retry   bool
	after   time.Duration
}

func (e *PolicyError) Error() string { return e.Message }

// FetchPolicy asks Zosia for the project's policy, with the retry rules a report's send has.
func FetchPolicy(ctx context.Context, hc *http.Client, p Policy, r PolicyRequest) (PolicyAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Total)
	defer cancel()
	body, err := json.Marshal(map[string]string{"server": r.Server, "repository": r.Repository, "configPath": r.ConfigPath})
	if err != nil {
		return PolicyAnswer{}, &PolicyError{Reason: PolicyInvalid, Message: err.Error()}
	}
	for attempt := 1; ; attempt++ {
		answer, err := fetchPolicy(ctx, hc, r, body)
		if err == nil {
			return answer, nil
		}
		if !err.retry || attempt >= p.Attempts {
			return PolicyAnswer{}, err
		}
		wait := p.Pause
		if err.after > 0 {
			if err.after > p.MaxRetryAfter {
				return PolicyAnswer{}, err
			}
			wait = err.after
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			return PolicyAnswer{}, err
		}
		select {
		case <-ctx.Done():
			return PolicyAnswer{}, err
		case <-time.After(wait):
		}
	}
}

func fetchPolicy(ctx context.Context, hc *http.Client, r PolicyRequest, body []byte) (PolicyAnswer, *PolicyError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.URL, "/")+"/api/v1/repo/policy", bytes.NewReader(body))
	if err != nil {
		return PolicyAnswer{}, &PolicyError{Reason: PolicyUnreachable, Message: "invalid Zosia URL: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+r.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", r.Client+"/"+r.Version)
	resp, err := hc.Do(req)
	if err != nil {
		msg := unwrapped(err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = "timeout"
		}
		return PolicyAnswer{}, &PolicyError{Reason: PolicyUnreachable, Message: msg, retry: ctx.Err() == nil}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPolicyAnswer+1))
	switch {
	case err != nil:
		return PolicyAnswer{}, &PolicyError{Reason: PolicyUnreachable, Message: "the answer broke off: " + err.Error(), retry: ctx.Err() == nil}
	case len(data) > maxPolicyAnswer:
		return PolicyAnswer{}, &PolicyError{Reason: PolicyUnreachable, Message: "the answer is larger than 64 KiB"}
	case resp.StatusCode == http.StatusOK:
		answer, err := ParsePolicyAnswer(data)
		if err != nil {
			return PolicyAnswer{}, &PolicyError{Reason: PolicyInvalid, Message: err.Error()}
		}
		return answer, nil
	}
	var said struct{ Message string }
	_ = json.Unmarshal(data, &said)
	// The server's words go into the report, so only a line of them.
	if r := []rune(said.Message); len(r) > maxPolicyMessage {
		said.Message = string(r[:maxPolicyMessage]) + "…"
	}
	if said.Message == "" {
		said.Message = "Zosia answered " + resp.Status
	}
	out := &PolicyError{Reason: PolicyInvalid, Message: said.Message}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		out.Reason = PolicyRefused
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		out.Reason, out.retry = PolicyUnreachable, true
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			out.after = time.Duration(s) * time.Second
		}
	}
	return PolicyAnswer{}, out
}

// ParsePolicyAnswer reads Zosia's document. It is data with a fixed list of fields: a policy
// needs all five values, each one shearbolt.yaml would accept, or nothing of it is used.
// Fields it does not know are ignored. Names count exactly as written: encoding/json would
// also take "POLICY" for "policy", which Zosia does not, so the fields are read from a map.
func ParsePolicyAnswer(data []byte) (PolicyAnswer, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		return PolicyAnswer{}, fmt.Errorf("the answer is not a JSON object")
	}
	if string(doc["schema"]) != strconv.Quote(PolicySchema) {
		return PolicyAnswer{}, fmt.Errorf("the answer is not a %s document", PolicySchema)
	}
	var out PolicyAnswer
	var err error
	if out.Version, err = wholeNumber(doc["version"]); err != nil || out.Version < 0 {
		return PolicyAnswer{}, fmt.Errorf("the answer's version is not a whole number from 0")
	}
	switch string(doc["policy"]) {
	case "":
		return PolicyAnswer{}, fmt.Errorf("the answer names no policy")
	case "null":
		return out, nil
	}
	// The scope is shown, not acted on, so one that a later Zosia adds is taken as it is.
	if json.Unmarshal(doc["scope"], &out.Scope) != nil || !scopeName.MatchString(out.Scope) {
		return PolicyAnswer{}, fmt.Errorf("the answer does not say whose policy it is")
	}
	pol, err := ParsePolicy(doc["policy"])
	if err != nil {
		return PolicyAnswer{}, err
	}
	out.Policy = &pol
	return out, nil
}

var scopeName = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)

// The most Zosia lets anyone save. A larger number is not Zosia's, and an age beyond what a
// time.Duration holds would turn into a negative limit.
const (
	maxPolicyHours = 87_600
	maxPolicyDays  = 3_650
)

// wholeNumber reads a JSON number written without fraction or exponent, as shearbolt.yaml takes
// whole numbers: 47.5, 48.0, 1e2 and "48" are refused.
func wholeNumber(raw json.RawMessage) (int, error) {
	return strconv.Atoi(string(raw))
}

// ParsePolicy reads the five values of a policy from JSON, by shearbolt.yaml's rules. Each
// value is read under its exact name and as its own JSON type: encoding/json alone would take
// "0.1" for a number and "BLOCKFIXABLE" for "blockFixable".
func ParsePolicy(data []byte) (config.Policy, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return config.Policy{}, fmt.Errorf("the policy is not a JSON object")
	}
	for _, name := range []string{"blockKnownExploited", "blockEpssAtLeast", "blockFixable", "maxDatabaseAgeHours", "maxExceptionDays"} {
		if len(raw[name]) == 0 {
			return config.Policy{}, fmt.Errorf("the policy does not have all five values")
		}
	}
	var pol config.Policy
	var err error
	switch string(raw["blockKnownExploited"]) {
	case "true":
		pol.BlockKnownExploited = true
	case "false":
	default:
		return config.Policy{}, fmt.Errorf("policy.blockKnownExploited must be true or false")
	}
	if fixable := raw["blockFixable"]; fixable[0] != '"' || json.Unmarshal(fixable, &pol.BlockFixable) != nil {
		return config.Policy{}, fmt.Errorf("policy.blockFixable must be text")
	}
	if c := raw["blockEpssAtLeast"][0]; c == '"' || c == 'n' {
		return config.Policy{}, fmt.Errorf("policy.blockEpssAtLeast must be a number")
	}
	if pol.BlockEpssAtLeast, err = strconv.ParseFloat(string(raw["blockEpssAtLeast"]), 64); err != nil {
		return config.Policy{}, fmt.Errorf("policy.blockEpssAtLeast must be a number")
	}
	if pol.MaxDatabaseAgeHours, err = wholeNumber(raw["maxDatabaseAgeHours"]); err != nil || pol.MaxDatabaseAgeHours > maxPolicyHours {
		return config.Policy{}, fmt.Errorf("policy.maxDatabaseAgeHours must be a whole number of at most %d", maxPolicyHours)
	}
	if pol.MaxExceptionDays, err = wholeNumber(raw["maxExceptionDays"]); err != nil || pol.MaxExceptionDays > maxPolicyDays {
		return config.Policy{}, fmt.Errorf("policy.maxExceptionDays must be a whole number of at most %d", maxPolicyDays)
	}
	if problems := pol.Problems(); len(problems) > 0 {
		return config.Policy{}, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return pol, nil
}
