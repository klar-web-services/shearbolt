package github_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const dailyWf = ".github/workflows/shearbolt-daily-ci.yml"

const dailyHead = "# Shearbolt's daily run of ci.yml: its build and its scan, without the steps after the scan.\n" +
	"# `shearbolt setup` writes this file. Run it again after you change ci.yml.\n" +
	"name: Shearbolt daily (ci)\non:\n  schedule:\n    - cron: '23 4 * * *'\n  workflow_dispatch:\n"

func daily(p *plan.Plan) string {
	for _, c := range p.Changes {
		if c.Path == dailyWf {
			return c.Content
		}
	}
	return ""
}

func dailyNotes(p *plan.Plan) []plan.Note {
	var out []plan.Note
	for _, n := range p.Notes {
		if strings.HasPrefix(n.Code, "DAILY_") {
			out = append(out, n)
		}
	}
	return out
}

// The daily workflow keeps what the build needs and leaves out what ties the job to a push: the
// jobs it waits for, its condition, its environment, its concurrency group and its outputs, the
// step that only pushes, and everything after the scan.
func TestGitHubDailyCopiesTheJobUpToItsScan(t *testing.T) {
	p, _ := result(t, npmRepo("web", lines(
		"name: ci",
		"on:",
		"  push:",
		"    branches: [main]",
		"permissions:",
		"  contents: read",
		"env:",
		"  NODE_ENV: production",
		"defaults:",
		"  run:",
		"    shell: bash",
		"jobs:",
		"  test:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: npm test",
		"  build:",
		"    needs: test",
		"    if: github.ref == 'refs/heads/main'",
		"    runs-on: ubuntu-latest",
		"    environment: production",
		"    concurrency: deploy",
		"    timeout-minutes: 20",
		"    outputs:",
		"      url: ${{ steps.deploy.outputs.url }}",
		"    steps:",
		"      - uses: actions/checkout@v5",
		"      # the site",
		"      - run: npm run build",
		"      - id: deploy",
		"        run: npm publish",
	), nil))
	want(t, daily(p), dailyHead+lines(
		"permissions:",
		"  contents: read",
		"env:",
		"  NODE_ENV: production",
		"defaults:",
		"  run:",
		"    shell: bash",
		"jobs:",
		"  build:",
		"    runs-on: ubuntu-latest",
		"    timeout-minutes: 20",
		"    steps:",
		"      - uses: actions/checkout@v5",
		"      # the site",
		"      - run: npm run build",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
	))
	ns := dailyNotes(p)
	if len(ns) != 1 || ns[0].Code != "DAILY_RUN" || ns[0].File != dailyWf || !strings.Contains(ns[0].Message, "60 days") {
		t.Errorf("notes = %+v", ns)
	}
	if i := slices.IndexFunc(p.Questions, func(q plan.Question) bool { return q.ID == "daily" }); i != len(p.Questions)-1 || p.Questions[i].Default != "yes" {
		t.Errorf("questions = %+v", p.Questions)
	}
}

func want(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("daily workflow:\n%s\nwant:\n%s", got, want)
	}
}

// A push between two scans is left out; the scans and their builds stay.
func TestGitHubDailyLeavesOutAPushBetweenScans(t *testing.T) {
	gate := "      - name: shearbolt\n        uses: klar-web-services/shearbolt@v1\n"
	p, _ := result(t, npmRepo("web", head+"      - run: npm run build\n"+gate+lines(
		"      - uses: softprops/action-gh-release@v2",
		"      - run: docker push ghcr.io/acme/web:1",
		"      - run: npm run build:docs",
	)+gate+"      - run: npm publish\n", nil))
	want(t, daily(p), dailyHead+"jobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v5\n"+
		"      - run: npm run build\n"+gate+"      - run: npm run build:docs\n"+gate)
}

func TestGitHubDailySkipsAJobThatCannotRunAlone(t *testing.T) {
	for name, tc := range map[string]struct{ job, why string }{
		"reads the outputs of a needed job": {lines(
			"  build:",
			"    needs: version",
			"    runs-on: ubuntu-latest",
			"    steps:",
			"      - run: npm run build -- --version ${{ needs.version.outputs.v }}",
			"      - run: npm publish",
		), "it reads the outputs of the jobs it needs"},
		"downloads what a needed job built": {lines(
			"  build:",
			"    needs: version",
			"    runs-on: ubuntu-latest",
			"    steps:",
			"      - uses: actions/download-artifact@v8",
			"      - run: npm run build",
			"      - run: npm publish",
		), "the step at line 11 downloads what another job built"},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := result(t, npmRepo("web", "on: push\njobs:\n  version:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo v=1 >> \"$GITHUB_OUTPUT\"\n"+tc.job, nil))
			ns := dailyNotes(p)
			if daily(p) != "" || len(ns) != 1 || ns[0].Code != "DAILY_SKIPPED" || ns[0].File != wf || !strings.Contains(ns[0].Message, "job build is not in the daily run, because "+tc.why) {
				t.Errorf("daily workflow %q, notes %+v", daily(p), ns)
			}
		})
	}
}

func TestGitHubDailyNo(t *testing.T) {
	root := tree(t, npmRepo("web", publish+"      - run: npm publish\n", nil))
	p := setup.Compute(setup.Options{Root: root, Answers: map[string]string{"daily": "no"}, Yes: true, Version: "1.2.3"})
	if p.Error != nil || daily(p) != "" || len(dailyNotes(p)) != 0 {
		t.Errorf("error %+v, daily workflow %q, notes %+v", p.Error, daily(p), dailyNotes(p))
	}
}

// Once a daily workflow exists, setup no longer asks, reads no jobs from it, and rewrites it when
// its workflow changes.
func TestGitHubDailyFollowsItsWorkflow(t *testing.T) {
	root := applied(t, npmRepo("web", publish+"      - run: npm publish\n", nil))
	full := filepath.Join(root, filepath.FromSlash(wf))
	src, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(strings.Replace(string(src), "npm run build", "npm run build -- --prod", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	p := compute(t, root)
	if len(p.Changes) != 1 || p.Changes[0].Path != dailyWf || p.Changes[0].Action != "modify" || !strings.Contains(p.Changes[0].Content, "npm run build -- --prod") {
		t.Fatalf("changes = %+v", p.Changes)
	}
	if slices.ContainsFunc(p.Questions, func(q plan.Question) bool { return q.ID == "daily" }) {
		t.Errorf("setup asks about a daily run that is there: %+v", p.Questions)
	}
	if len(p.CI) != 1 || p.CI[0].File != wf {
		t.Errorf("ci = %+v", p.CI)
	}
}
