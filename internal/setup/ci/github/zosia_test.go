package github_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const zosiaWith = "          zosia-secret: ${{ secrets.ZOSIA_SECRET }}"

// zosiaResult runs setup with the zosia answer ("" = the default) and returns the plan and the
// workflow afterwards ("" when it is unchanged).
func zosiaResult(t *testing.T, files map[string]string, answer string) (*plan.Plan, string) {
	t.Helper()
	o := setup.Options{Root: tree(t, files), Yes: true, Version: "1.2.3"}
	if answer != "" {
		o.Answers = map[string]string{"zosia": answer}
	}
	p := setup.Compute(o)
	if p.Error != nil {
		t.Fatalf("plan error: %+v", p.Error)
	}
	if c := change(p, wf); c != nil {
		return p, c.Content
	}
	return p, ""
}

func zosiaNotes(p *plan.Plan) []string {
	return slices.DeleteFunc(noteCodes(p), func(c string) bool { return !strings.HasPrefix(c, "ZOSIA_") })
}

const publish = "on: push\njobs:\n  publish:\n    runs-on: ubuntu-latest\n    steps:\n      - run: npm run build\n"

func TestGitHubZosiaNewGate(t *testing.T) {
	p, got := zosiaResult(t, npmRepo("web", publish+"      - run: npm publish\n", nil), "yes")
	wantWorkflow(t, got, publish+lines(
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        with:",
		zosiaWith,
		"      - run: npm publish",
	))
	if got := zosiaNotes(p); !slices.Equal(got, []string{"ZOSIA_SECRET"}) {
		t.Errorf("notes = %v", got)
	}
}

func TestGitHubZosiaExistingGates(t *testing.T) {
	for _, tc := range []struct {
		name         string
		before, want []string
	}{
		{"no inputs",
			[]string{"      - uses: klar-web-services/shearbolt@v1"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", zosiaWith}},
		{"other inputs",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence  # keep", "        if: always()"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence  # keep", zosiaWith, "        if: always()"}},
		{"only stale targets",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          targets: web"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", zosiaWith}},
		{"stale targets last",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence", "          targets: web"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence", zosiaWith}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, got := zosiaResult(t, npmRepo("web", publish+lines(tc.before...)+"      - run: npm publish\n", nil), "yes")
			wantWorkflow(t, got, publish+lines(tc.want...)+"      - run: npm publish\n")
			if got := zosiaNotes(p); !slices.Equal(got, []string{"ZOSIA_SECRET"}) {
				t.Errorf("notes = %v", got)
			}
		})
	}
}

// The publish job's gate gets targets (the image job's gate scans the image), with or without
// a with: block of its own.
func TestGitHubZosiaWithNewTargets(t *testing.T) {
	image := lines(
		"on: push",
		"jobs:",
		"  image:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - id: shearbolt-build",
		"        run: |",
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"        with:",
		zosiaWith,
		"      - run: docker push web:1",
		"  publish:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: npm run build",
	)
	for _, tc := range []struct {
		name         string
		before, want []string
	}{
		{"no inputs",
			[]string{"      - uses: klar-web-services/shearbolt@v1"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          targets: source", zosiaWith}},
		{"other targets",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          targets: web"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          targets: source", zosiaWith}},
		{"other inputs",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence", "          targets: source", zosiaWith}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No answer: the image job's gate already sends, so the default is yes.
			p, got := zosiaResult(t, npmRepo("web", image+lines(tc.before...)+"      - run: npm publish\n", nil), "")
			wantWorkflow(t, got, image+lines(tc.want...)+"      - run: npm publish\n")
			if got := zosiaNotes(p); !slices.Equal(got, []string{"ZOSIA_SECRET"}) {
				t.Errorf("notes = %v", got)
			}
		})
	}
}

func TestGitHubZosiaNoNeverRemoves(t *testing.T) {
	src := publish + lines("      - uses: klar-web-services/shearbolt@v1", "        with:", zosiaWith, "      - run: npm publish")
	p, got := zosiaResult(t, npmRepo("web", src, nil), "no")
	if got != "" || !slices.Equal(zosiaNotes(p), []string{"ZOSIA_KEPT"}) {
		t.Errorf("notes = %v, workflow:\n%s", zosiaNotes(p), got)
	}
	p, got = zosiaResult(t, npmRepo("web", src, nil), "")
	if got != "" || len(zosiaNotes(p)) != 0 {
		t.Errorf("a run with the defaults: notes = %v, workflow:\n%s", zosiaNotes(p), got)
	}
}

func TestGitHubZosiaRunStepGate(t *testing.T) {
	scan := []string{
		"      - run: |",
		"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
		`          "$HOME/.local/bin/shearbolt" scan --ci`,
	}
	p, got := zosiaResult(t, npmRepo("web", publish+lines(scan...)+"      - run: npm publish\n", nil), "yes")
	if got != "" || !slices.Equal(zosiaNotes(p), []string{"ZOSIA_RUN_GATE"}) {
		t.Errorf("notes = %v, workflow:\n%s", zosiaNotes(p), got)
	}
	env := append(slices.Clone(scan), "        env:", "          ZOSIA_SECRET: ${{ secrets.ZOSIA_SECRET }}")
	p, got = zosiaResult(t, npmRepo("web", publish+lines(env...)+"      - run: npm publish\n", nil), "yes")
	if got != "" || len(zosiaNotes(p)) != 0 {
		t.Errorf("a step that already sends: notes = %v, workflow:\n%s", zosiaNotes(p), got)
	}
}

func TestGitHubZosiaFlowStyleStepIsRefused(t *testing.T) {
	src := publish + "      - {uses: klar-web-services/shearbolt@v1}\n      - run: npm publish\n"
	p, got := zosiaResult(t, npmRepo("web", src, nil), "yes")
	if got != "" || !hasNote(p, "UNSUPPORTED_EDIT") || len(zosiaNotes(p)) != 0 {
		t.Errorf("notes = %v, workflow:\n%s", noteCodes(p), got)
	}
}
