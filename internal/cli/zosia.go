package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/report"
	"github.com/klar-web-services/shearbolt/internal/zosia"
)

var zosiaPolicy = zosia.Policy{Total: 30 * time.Second, Attempts: 2, Pause: 2 * time.Second, MaxRetryAfter: 10 * time.Second}

// zosiaFetch: fetching a policy holds up the scan, so it gets half the time a report's send has.
var zosiaFetch = zosia.Policy{Total: 15 * time.Second, Attempts: 2, Pause: 2 * time.Second, MaxRetryAfter: 10 * time.Second}

// policyFromZosia decides which policy a config scan uses. When shearbolt.yaml takes its policy
// from Zosia (zosia.policy) and this is a CI scan that could also send its report, it asks Zosia
// and, with an answer that holds a policy, puts that policy in place of the file's. In every
// other case the file's policy stays, and a warning says why when the fetch was wanted and
// brought nothing usable. It never fails the scan.
func policyFromZosia(ctx context.Context, d deps, cfg *config.Config, configPath string, ciMode bool) (model.PolicySource, []model.Diagnostic) {
	src := model.PolicySource{From: "file"}
	if !cfg.Zosia.Policy {
		return src, nil
	}
	fellBack := func(state, code, message string) (model.PolicySource, []model.Diagnostic) {
		src.Zosia = state
		return src, []model.Diagnostic{{Code: code, Message: message + ", so this scan used the policy in shearbolt.yaml"}}
	}
	if !ciMode {
		src.Zosia = "local"
		return src, nil
	}
	secret := strings.TrimSpace(d.getenv("ZOSIA_SECRET"))
	if secret == "" {
		return fellBack("no-secret", "ZOSIA_POLICY_NO_SECRET", "shearbolt.yaml takes its policy from Zosia, but ZOSIA_SECRET is not set here (as in a pull request from a fork)")
	}
	ci, ok := zosia.DetectCI(d.getenv, os.ReadFile)
	if !ok {
		return fellBack("no-ci", "ZOSIA_POLICY_NO_CI", "shearbolt.yaml takes its policy from Zosia, but this is no CI whose repository Shearbolt can name")
	}
	url := d.getenv("ZOSIA_URL")
	if url == "" {
		url = zosia.DefaultURL
	}
	answer, err := d.fetchPolicy(ctx, zosia.PolicyRequest{
		URL: url, Secret: secret, Client: "shearbolt", Version: d.version,
		Server: ci.Server, Repository: ci.Repository, ConfigPath: ci.RepoPath(configPath),
	})
	var pe *zosia.PolicyError
	switch {
	case errors.As(err, &pe) && pe.Reason == zosia.PolicyRefused:
		return fellBack("refused", "ZOSIA_POLICY_REFUSED", fmt.Sprintf("Zosia refused the secret (%s)", pe.Message))
	case errors.As(err, &pe) && pe.Reason == zosia.PolicyInvalid:
		return fellBack("invalid", "ZOSIA_POLICY_INVALID", fmt.Sprintf("Zosia's answer could not be used (%s)", pe.Message))
	case err != nil:
		return fellBack("unreachable", "ZOSIA_POLICY_UNREACHABLE", fmt.Sprintf("Zosia could not be reached (%v)", err))
	}
	src.Version = &answer.Version
	if answer.Policy == nil {
		src.Zosia = "none"
		return src, nil
	}
	cfg.Policy = *answer.Policy
	src.From, src.Zosia, src.Scope = "zosia", "used", answer.Scope
	if url != zosia.DefaultURL {
		// ZOSIA_URL is for tests. A policy from there must not pass for Zosia's unnoticed.
		return src, []model.Diagnostic{{Code: "ZOSIA_URL_SET", Message: fmt.Sprintf("this policy came from %s, because ZOSIA_URL is set, and not from %s", hostOf(url), zosia.DefaultURL)}}
	}
	return src, nil
}

// hostOf is the host of a URL, without a user name or password that may be in it.
func hostOf(raw string) string {
	if u, err := neturl.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "another address"
}

// reportToZosia sends a CI scan's result when ZOSIA_SECRET is set. It only ever writes one line
// to stderr; the scan's exit code is decided before and never changes here.
func reportToZosia(ctx context.Context, d deps, stderr io.Writer, result *model.Run, configPath string, only []string) {
	secret := strings.TrimSpace(d.getenv("ZOSIA_SECRET"))
	if secret == "" {
		return
	}
	notSent := func(format string, a ...any) { fmt.Fprintf(stderr, "Zosia: report not sent"+format+"\n", a...) }
	ci, ok := zosia.DetectCI(d.getenv, os.ReadFile)
	if !ok {
		notSent(": unknown CI, so the repository cannot be named")
		return
	}
	url, key, err := zosia.Target(d.getenv("ZOSIA_URL"), d.getenv("ZOSIA_KEY"))
	if err != nil {
		notSent(": %v", err)
		return
	}
	data, err := report.RunJSON(result)
	if err != nil {
		notSent(": %v", err)
		return
	}
	targets := slices.Compact(slices.Sorted(slices.Values(only)))
	if targets == nil {
		targets = []string{}
	}
	res, err := d.sendZosia(ctx, zosia.Request{
		URL: url, Key: key, Secret: secret, Client: "shearbolt", Version: d.version, Kind: "shearbolt",
		Source: map[string]any{"ci": ci, "configPath": ci.RepoPath(configPath), "targets": targets},
		Report: data, Now: d.now(),
	})
	var ze *zosia.Error
	switch {
	case errors.As(err, &ze):
		notSent(" (%s): %s", ze.Code, ze.Message)
	case err != nil:
		notSent(": %v", err)
	default:
		fmt.Fprintf(stderr, "Zosia: report sent: %s\n", res.URL)
	}
}
