package github_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const (
	zosiaWith = "          zosia-secret: ${{ secrets.ZOSIA_SECRET }}"
	zosiaEnv  = "          ZOSIA_SECRET: ${{ secrets.ZOSIA_SECRET }}"
)

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

// The zosia answer is about the gates setup adds. A gate that is there already keeps what it has:
// setup only corrects its targets, and a yes points to `shearbolt zosia enable` for it.
func TestGitHubZosiaLeavesExistingGates(t *testing.T) {
	for _, tc := range []struct {
		name         string
		before, want []string // want nil = the workflow does not change
	}{
		{"no inputs", []string{"      - uses: klar-web-services/shearbolt@v1"}, nil},
		{"other inputs",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence  # keep", "        if: always()"}, nil},
		{"only stale targets",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          targets: web"},
			[]string{"      - uses: klar-web-services/shearbolt@v1"}},
		{"stale targets last",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence", "          targets: web"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, got := zosiaResult(t, npmRepo("web", publish+lines(tc.before...)+"      - run: npm publish\n", nil), "yes")
			want := ""
			if tc.want != nil {
				want = publish + lines(tc.want...) + "      - run: npm publish\n"
			}
			wantWorkflow(t, got, want)
			if got := zosiaNotes(p); !slices.Equal(got, []string{"ZOSIA_NOT_ADDED"}) {
				t.Errorf("notes = %v", got)
			}
			if i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "ZOSIA_NOT_ADDED" }); i >= 0 && !strings.Contains(p.Notes[i].Message, "shearbolt zosia enable") {
				t.Errorf("the note does not name the command that adds the secret: %+v", p.Notes[i])
			}
		})
	}
}

// A new job beside one that is gated already: setup gates only the new one, and the zosia answer
// decides only about the new gate.
func TestGitHubZosiaNewGateBesideExisting(t *testing.T) {
	workflow := func(input ...string) string {
		gate := append([]string{"      - uses: klar-web-services/shearbolt@v1"}, input...)
		return publish + lines(gate...) + lines(
			"      - run: npm publish",
			"  nightly:",
			"    runs-on: ubuntu-latest",
			"    steps:",
			"      - run: npm run build",
			"      - run: npm publish",
		)
	}
	added := func(input ...string) string {
		gate := append([]string{"      - name: shearbolt", "        uses: klar-web-services/shearbolt@v1"}, input...)
		return "      - run: npm run build\n" + lines(gate...) + "      - run: npm publish\n"
	}
	sends := []string{"        with:", zosiaWith}
	for _, tc := range []struct {
		name            string
		existing        []string
		answer          string
		newGate         []string
		notes           []string
		prompt, secrets string
	}{
		{"zosia is set up, defaults", sends, "", sends, []string{"ZOSIA_SECRET"}, "already send", "already"},
		{"zosia is set up, yes", sends, "yes", sends, []string{"ZOSIA_SECRET"}, "already send", "already"},
		{"zosia is set up, no", sends, "no", nil, nil, "already send", ""},
		{"zosia is not set up, defaults", nil, "", nil, nil, "Send reports to Zosia", ""},
		{"zosia is not set up, yes", nil, "yes", sends, []string{"ZOSIA_NOT_ADDED", "ZOSIA_SECRET"}, "Send reports to Zosia", "Add the repository secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := workflow(tc.existing...)
			p, got := zosiaResult(t, npmRepo("web", before, nil), tc.answer)
			old, _, _ := strings.Cut(before, "  nightly:\n")
			if !strings.HasPrefix(got, old+"  nightly:\n") {
				t.Errorf("the job that was gated already changed:\n%s", got)
			}
			if !strings.HasSuffix(got, added(tc.newGate...)) {
				t.Errorf("the new job's gate:\n%s\nwant it to end with:\n%s", got, added(tc.newGate...))
			}
			notes := zosiaNotes(p)
			slices.Sort(notes)
			if !slices.Equal(notes, tc.notes) {
				t.Errorf("notes = %v, want %v", notes, tc.notes)
			}
			q := p.Questions[len(p.Questions)-1]
			wantDefault := "no"
			if tc.existing != nil {
				wantDefault = "yes"
			}
			if q.ID != "zosia" || q.Default != wantDefault || !strings.Contains(q.Prompt, tc.prompt) {
				t.Errorf("question = %+v", q)
			}
			if i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "ZOSIA_SECRET" }); i >= 0 && !strings.Contains(p.Notes[i].Message, tc.secrets) {
				t.Errorf("the secret note = %q, want it to say %q", p.Notes[i].Message, tc.secrets)
			}
		})
	}
}

// The publish job's gate gets targets (the image job's gate scans the image), with or without
// a with: block of its own, and nothing else.
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
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          targets: source"}},
		{"other targets",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          targets: web"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          targets: source"}},
		{"other inputs",
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence"},
			[]string{"      - uses: klar-web-services/shearbolt@v1", "        with:", "          artifact-name: evidence", "          targets: source"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No answer: the image job's gate already sends, so the default is yes. It is about new
			// gates only, and this one is there already.
			p, got := zosiaResult(t, npmRepo("web", image+lines(tc.before...)+"      - run: npm publish\n", nil), "")
			wantWorkflow(t, got, image+lines(tc.want...)+"      - run: npm publish\n")
			if got := zosiaNotes(p); len(got) != 0 {
				t.Errorf("notes = %v", got)
			}
		})
	}
}

func TestGitHubZosiaNoNeverRemoves(t *testing.T) {
	src := publish + lines("      - uses: klar-web-services/shearbolt@v1", "        with:", zosiaWith, "      - run: npm publish")
	p, got := zosiaResult(t, npmRepo("web", src, nil), "no")
	if got != "" || len(zosiaNotes(p)) != 0 {
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
	if got != "" || !slices.Equal(zosiaNotes(p), []string{"ZOSIA_NOT_ADDED"}) {
		t.Errorf("notes = %v, workflow:\n%s", zosiaNotes(p), got)
	}
	if i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "ZOSIA_NOT_ADDED" }); i < 0 || !strings.Contains(p.Notes[i].Message, "shearbolt zosia enable") {
		t.Errorf("the note does not name the command that adds the secret: %+v", p.Notes)
	}
	env := append(slices.Clone(scan), "        env:", "          ZOSIA_SECRET: ${{ secrets.ZOSIA_SECRET }}")
	p, got = zosiaResult(t, npmRepo("web", publish+lines(env...)+"      - run: npm publish\n", nil), "yes")
	if got != "" || len(zosiaNotes(p)) != 0 {
		t.Errorf("a step that already sends: notes = %v, workflow:\n%s", zosiaNotes(p), got)
	}
}

// zosiaCmd runs what `shearbolt zosia enable` (on) or `disable` computes and returns the plan and
// the workflow afterwards ("" when it is unchanged).
func zosiaCmd(t *testing.T, files map[string]string, on bool) (*plan.Plan, string) {
	t.Helper()
	p := setup.Zosia(tree(t, files), on)
	if p.Error != nil {
		t.Fatalf("error: %+v", p.Error)
	}
	if c := change(p, wf); c != nil {
		if c.Diff == "" || c.Mode == 0 {
			t.Errorf("the change has no diff or mode: %+v", c)
		}
		return p, c.Content
	}
	return p, ""
}

const gateStep = "      - uses: klar-web-services/shearbolt@v1"

func TestZosiaEnable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		before, want []string
		notes        []string
	}{
		{"no inputs", []string{gateStep}, []string{gateStep, "        with:", zosiaWith}, []string{"ZOSIA_ADDED", "ZOSIA_SECRET"}},
		{"targets are kept",
			[]string{gateStep, "        with:", "          targets: web  # keep"},
			[]string{gateStep, "        with:", "          targets: web  # keep", zosiaWith},
			[]string{"ZOSIA_ADDED", "ZOSIA_SECRET"}},
		{"already enabled", []string{gateStep, "        with:", zosiaWith}, nil, []string{"ZOSIA_ALREADY", "ZOSIA_SECRET"}},
		{"run step",
			[]string{"      - run: |", "          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh", `          "$HOME/.local/bin/shearbolt" scan --ci`},
			[]string{"      - run: |", "          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh", `          "$HOME/.local/bin/shearbolt" scan --ci`, "        env:", zosiaEnv},
			[]string{"ZOSIA_ADDED", "ZOSIA_SECRET"}},
		{"run step with an env",
			[]string{"      - name: gate", "        env:", "          CACHE: /tmp/c  # keep", "        run: shearbolt scan --ci"},
			[]string{"      - name: gate", "        env:", "          CACHE: /tmp/c  # keep", zosiaEnv, "        run: shearbolt scan --ci"},
			[]string{"ZOSIA_ADDED", "ZOSIA_SECRET"}},
		{"run step with two scans",
			[]string{"      - run: |", "          shearbolt scan --ci", "          shearbolt scan --ci --targets web"},
			[]string{"      - run: |", "          shearbolt scan --ci", "          shearbolt scan --ci --targets web", "        env:", zosiaEnv},
			[]string{"ZOSIA_ADDED", "ZOSIA_ADDED", "ZOSIA_SECRET"}},
		{"run step that sends already",
			[]string{"      - run: shearbolt scan --ci", "        env:", zosiaEnv},
			nil, []string{"ZOSIA_ALREADY", "ZOSIA_SECRET"}},
		{"flow style", []string{"      - {uses: klar-web-services/shearbolt@v1}"}, nil, []string{"UNSUPPORTED_EDIT", "ZOSIA_SECRET"}},
		{"run step in flow style", []string{"      - {run: shearbolt scan --ci}"}, nil, []string{"UNSUPPORTED_EDIT", "ZOSIA_SECRET"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, got := zosiaCmd(t, npmRepo("web", publish+lines(tc.before...)+"      - run: npm publish\n", nil), true)
			want := ""
			if tc.want != nil {
				want = publish + lines(tc.want...) + "      - run: npm publish\n"
			}
			wantWorkflow(t, got, want)
			if !slices.Equal(noteCodes(p), tc.notes) {
				t.Errorf("notes = %v, want %v", noteCodes(p), tc.notes)
			}
			if len(p.Changes) > 1 {
				t.Errorf("changes = %d: only the workflow may change", len(p.Changes))
			}
		})
	}
}

func TestZosiaEnableNamesTheGitHubCLI(t *testing.T) {
	p, _ := zosiaCmd(t, npmRepo("web", publish+gateStep+"\n", nil), true)
	n := p.Notes[len(p.Notes)-1]
	if n.Code != "ZOSIA_SECRET" || n.File != wf || !strings.Contains(n.Message, "https://zosiasec.com/app/secrets") || !strings.Contains(n.Message, "gh secret set ZOSIA_SECRET") {
		t.Errorf("note = %+v", n)
	}
}

func TestZosiaDisable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		before, want []string
		notes        []string
	}{
		{"only input", []string{gateStep, "        with:", zosiaWith}, []string{gateStep}, []string{"ZOSIA_REMOVED", "ZOSIA_SECRET_STAYS"}},
		{"other inputs stay",
			[]string{gateStep, "        with:", "          targets: web", zosiaWith},
			[]string{gateStep, "        with:", "          targets: web"},
			[]string{"ZOSIA_REMOVED", "ZOSIA_SECRET_STAYS"}},
		{"not enabled", []string{gateStep}, nil, []string{"ZOSIA_OFF", "ZOSIA_SECRET_STAYS"}},
		{"run step with the secret",
			[]string{"      - run: shearbolt scan --ci", "        env:", zosiaEnv},
			[]string{"      - run: shearbolt scan --ci"},
			[]string{"ZOSIA_REMOVED", "ZOSIA_SECRET_STAYS"}},
		{"run step, other env stays",
			[]string{"      - env:", "          CACHE: /tmp/c", zosiaEnv, "        run: shearbolt scan --ci"},
			[]string{"      - env:", "          CACHE: /tmp/c", "        run: shearbolt scan --ci"},
			[]string{"ZOSIA_REMOVED", "ZOSIA_SECRET_STAYS"}},
		{"run step, the secret is not in the step's env",
			[]string{"      - run: ZOSIA_SECRET=${{ secrets.ZOSIA_SECRET }} shearbolt scan --ci"},
			nil, []string{"ZOSIA_KEPT", "ZOSIA_SECRET_STAYS"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, got := zosiaCmd(t, npmRepo("web", publish+lines(tc.before...)+"      - run: npm publish\n", nil), false)
			want := ""
			if tc.want != nil {
				want = publish + lines(tc.want...) + "      - run: npm publish\n"
			}
			wantWorkflow(t, got, want)
			if !slices.Equal(noteCodes(p), tc.notes) {
				t.Errorf("notes = %v, want %v", noteCodes(p), tc.notes)
			}
		})
	}
}

// What enable writes is what setup would have written, so setup has nothing to add afterwards,
// and what disable leaves is what setup leaves with the answer no.
func TestZosiaEnableAgreesWithSetup(t *testing.T) {
	root := tree(t, npmRepo("web", publish+"      - run: npm publish\n", nil))
	first := setup.Compute(setup.Options{Root: root, Yes: true, Version: "1.2.3"})
	if err := setup.Write(root, first); err != nil {
		t.Fatal(err)
	}
	on := setup.Zosia(root, true)
	if on.Error != nil || len(on.Changes) != 1 {
		t.Fatalf("enable: %+v, %d changes", on.Error, len(on.Changes))
	}
	if err := setup.Write(root, on); err != nil {
		t.Fatal(err)
	}
	again := setup.Compute(setup.Options{Root: root, Yes: true, Version: "1.2.3"})
	if again.Error != nil || len(again.Changes) != 0 || len(zosiaNotes(again)) != 0 {
		t.Errorf("setup after enable: %+v, %d changes, notes %v", again.Error, len(again.Changes), zosiaNotes(again))
	}
	off := setup.Zosia(root, false)
	if off.Error != nil || len(off.Changes) != 1 || off.Changes[0].Content != first.Changes[slices.IndexFunc(first.Changes, func(c plan.FileChange) bool { return c.Path == wf })].Content {
		t.Errorf("disable does not restore the workflow setup wrote: %+v", off.Changes)
	}
}

// A gate that runs a Shearbolt release from before 1.4.0 cannot send, whatever its step is given.
func TestZosiaEnableOldGate(t *testing.T) {
	download := func(version string) []string {
		return []string{
			"      - run: |",
			`          curl -fsSL "https://tools.klar.ws/shearbolt/v` + version + `/shearbolt.tar.gz" | tar -xz`,
			"          ./shearbolt scan --ci",
		}
	}
	workflowEnv := strings.Replace(publish, "jobs:\n", "env:\n  GATE_VERSION: '1.3.0'\njobs:\n", 1)
	jobEnv := strings.Replace(publish, "    steps:\n", "    env:\n      GATE_VERSION: 1.3.0\n    steps:\n", 1)
	for _, tc := range []struct {
		name, head string
		step       []string
		old        string
	}{
		{"version in the URL", publish, download("1.3.0"), "1.3.0"},
		{"version in the workflow env", workflowEnv, download("$GATE_VERSION"), "1.3.0"},
		{"version in the job env", jobEnv, download("${GATE_VERSION}"), "1.3.0"},
		{"first release that sends", publish, download("1.4.0"), ""},
		{"later release", publish, download("2.0.1"), ""},
		{"version not known", publish, download("$GATE_VERSION"), ""},
		{"version computed in the workflow", strings.Replace(workflowEnv, "'1.3.0'", "${{ vars.GATE }}", 1), download("$GATE_VERSION"), ""},
		{"old action", publish, []string{"      - uses: klar-web-services/shearbolt@v1.3.0"}, "1.3.0"},
		{"action by its major tag", publish, []string{gateStep}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, got := zosiaCmd(t, npmRepo("web", tc.head+lines(tc.step...)+"      - run: npm publish\n", nil), true)
			if !strings.Contains(got, "ZOSIA_SECRET") {
				t.Errorf("the step did not get the secret:\n%s", got)
			}
			var notes []plan.Note
			for _, n := range p.Notes {
				if n.Code == "ZOSIA_GATE_TOO_OLD" {
					notes = append(notes, n)
				}
			}
			switch {
			case tc.old == "" && len(notes) != 0:
				t.Errorf("notes = %+v, want no ZOSIA_GATE_TOO_OLD", notes)
			case tc.old != "" && (len(notes) != 1 || notes[0].Line == 0 || !strings.Contains(notes[0].Message, "Shearbolt "+tc.old) || !strings.Contains(notes[0].Message, "1.4.0")):
				t.Errorf("notes = %+v, want one ZOSIA_GATE_TOO_OLD naming %s and 1.4.0", p.Notes, tc.old)
			}
		})
	}
	// Disable has nothing to say about the version.
	p, _ := zosiaCmd(t, npmRepo("web", publish+lines(download("1.3.0")...), nil), false)
	if hasNote(p, "ZOSIA_GATE_TOO_OLD") {
		t.Errorf("disable: notes = %v", noteCodes(p))
	}
}

func TestZosiaDisableKeepsAJobLevelSecret(t *testing.T) {
	head := strings.Replace(publish, "    steps:\n", "    env:\n      ZOSIA_SECRET: ${{ secrets.ZOSIA_SECRET }}\n    steps:\n", 1)
	p, got := zosiaCmd(t, npmRepo("web", head+"      - run: shearbolt scan --ci\n      - run: npm publish\n", nil), false)
	if got != "" || !slices.Equal(noteCodes(p), []string{"ZOSIA_KEPT", "ZOSIA_SECRET_STAYS"}) {
		t.Errorf("notes = %v, workflow:\n%s", noteCodes(p), got)
	}
}

// What enable adds to a run step, disable takes out again.
func TestZosiaRunStepRoundTrip(t *testing.T) {
	for _, step := range [][]string{
		{"      - run: shearbolt scan --ci"},
		{"      - name: gate", "        env:", "          CACHE: /tmp/c", "        run: |", "          shearbolt scan --ci", ""},
	} {
		before := publish + lines(step...) + "      - run: npm publish\n"
		root := tree(t, npmRepo("web", before, nil))
		on := setup.Zosia(root, true)
		if on.Error != nil || len(on.Changes) != 1 {
			t.Fatalf("enable: %+v, %d changes", on.Error, len(on.Changes))
		}
		if err := setup.Write(root, on); err != nil {
			t.Fatal(err)
		}
		if again := setup.Zosia(root, true); again.Error != nil || len(again.Changes) != 0 || !hasNote(again, "ZOSIA_ALREADY") {
			t.Errorf("second enable: %+v, %d changes, notes %v", again.Error, len(again.Changes), noteCodes(again))
		}
		off := setup.Zosia(root, false)
		if off.Error != nil || len(off.Changes) != 1 || off.Changes[0].Content != before {
			t.Errorf("disable does not restore the workflow: %+v", off.Changes)
		}
	}
}
