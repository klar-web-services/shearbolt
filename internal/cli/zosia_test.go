package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/zosia"
)

const testPub = "test:X9R2M7XkP8p7rRv75CT-rLF12dTUow8Wbb0wY28LRGs"

func zosiaDeps(env map[string]string, send func(context.Context, zosia.Request) (zosia.Result, error)) deps {
	return deps{
		getenv:    func(k string) string { return env[k] },
		now:       func() time.Time { return time.Unix(0, 0) },
		version:   "1.4.0",
		sendZosia: send,
	}
}

func githubEnv(extra map[string]string) map[string]string {
	m := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "o/r",
		"GITHUB_REF": "refs/heads/main", "GITHUB_REF_NAME": "main", "GITHUB_SHA": "abc", "GITHUB_WORKSPACE": "/w", "ZOSIA_KEY": testPub}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestReportToZosiaSendsWithASecret(t *testing.T) {
	var got zosia.Request
	d := zosiaDeps(githubEnv(map[string]string{"ZOSIA_SECRET": "zosia_x"}), func(_ context.Context, r zosia.Request) (zosia.Result, error) {
		got = r
		return zosia.Result{URL: "https://zosiasec.com/app/reports/1"}, nil
	})
	var stderr bytes.Buffer
	reportToZosia(context.Background(), d, &stderr, &model.Run{RunID: "r1"}, "/w/apps/web/shearbolt.yaml", []string{"web", "source", "web"})
	if stderr.String() != "Zosia: report sent: https://zosiasec.com/app/reports/1\n" {
		t.Fatalf("stderr %q", stderr.String())
	}
	src := got.Source.(map[string]any)
	var run struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(got.Report, &run); err != nil {
		t.Fatal(err)
	}
	if got.Secret != "zosia_x" || got.Kind != "shearbolt" || src["configPath"] != "apps/web/shearbolt.yaml" ||
		strings.Join(src["targets"].([]string), " ") != "source web" || run.RunID != "r1" {
		t.Fatalf("request %+v", got)
	}
}

func TestReportToZosiaWithoutASecretSaysNothing(t *testing.T) {
	d := zosiaDeps(githubEnv(nil), func(context.Context, zosia.Request) (zosia.Result, error) {
		t.Fatal("sent without a secret")
		return zosia.Result{}, nil
	})
	var stderr bytes.Buffer
	reportToZosia(context.Background(), d, &stderr, &model.Run{}, "shearbolt.yaml", nil)
	if stderr.Len() != 0 {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestReportToZosiaFailureIsOneLineWithoutTheSecret(t *testing.T) {
	d := zosiaDeps(githubEnv(map[string]string{"ZOSIA_SECRET": "zosia_x"}), func(context.Context, zosia.Request) (zosia.Result, error) {
		return zosia.Result{}, &zosia.Error{Code: "PLAN_LIMIT", Message: "Your plan allows 5 repositories."}
	})
	var stderr bytes.Buffer
	reportToZosia(context.Background(), d, &stderr, &model.Run{}, "shearbolt.yaml", nil)
	if stderr.String() != "Zosia: report not sent (PLAN_LIMIT): Your plan allows 5 repositories.\n" || strings.Contains(stderr.String(), "zosia_x") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestReportToZosiaUnknownCI(t *testing.T) {
	d := zosiaDeps(map[string]string{"CI": "true", "ZOSIA_SECRET": "zosia_x"}, nil)
	var stderr bytes.Buffer
	reportToZosia(context.Background(), d, &stderr, &model.Run{}, "shearbolt.yaml", nil)
	if stderr.String() != "Zosia: report not sent: unknown CI, so the repository cannot be named\n" {
		t.Fatalf("stderr %q", stderr.String())
	}
}
