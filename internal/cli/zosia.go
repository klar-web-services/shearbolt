package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/report"
	"github.com/klar-web-services/shearbolt/internal/zosia"
)

var zosiaPolicy = zosia.Policy{Total: 30 * time.Second, Attempts: 2, Pause: 2 * time.Second, MaxRetryAfter: 10 * time.Second}

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
