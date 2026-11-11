package github_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

// reached is setup's marker condition for the step with id shearbolt-build.
const reached = "steps.shearbolt-build.outputs.shearbolt-reached != ''"

// composeImage is a compose file, which gives the root project the target docker-compose.
const composeImage = "services:\n  web:\n    image: nginx:1.27\n"

// rootRepo is a repository with only a Dockerfile, so the root project owns its image builds,
// plus the workflow and extra files.
func rootRepo(workflow string, extra map[string]string) map[string]string {
	files := map[string]string{"Dockerfile": "FROM alpine\n", wf: workflow}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

// withGateIf gives the shearbolt gate step in workflow text s the condition cond.
func withGateIf(s, cond string) string {
	return strings.Replace(s, "      - name: shearbolt\n", "      - name: shearbolt\n        if: "+cond+"\n", 1)
}

// skipping and running are the two forms of EARLY_STOP (design §3.3).
func skipping(stop string, line int) string {
	return fmt.Sprintf("this step can stop before its build (%s at line %d); the rest of the step and the shearbolt gate run only when the build ran. If you add a target to shearbolt.yaml by hand, run setup again, so the gate stops skipping.", stop, line)
}

// running is the running form with the advice for the reasons about targets and later pushes.
func running(stop string, line int, reason string) string {
	return runningAdvice(stop, line, reason, "Remove targets that do not ship from shearbolt.yaml and run setup again, or move the early stop into the job's if:.")
}

func runningAdvice(stop string, line int, reason, advice string) string {
	return fmt.Sprintf("this step can stop before its build (%s at line %d); the rest of the step runs only when the build ran, but the shearbolt gate still runs on early stops and fails when the image was not built, because %s. %s", stop, line, reason, advice)
}

// The running form's advice for the reasons that are about the gate itself.
const (
	adviceNotNext  = "Move the shearbolt gate directly after this step and run setup again."
	adviceRunStep  = "Use the Shearbolt action for the gate (uses: klar-web-services/shearbolt@v1) and run setup again."
	adviceTemplate = "Write the gate's if: without ${{ }} inside it and run setup again."
)

// removed is EARLY_STOP for a gate whose marker term setup took away.
func removed(id string) string {
	return "setup removed the early-stop condition from this shearbolt gate, because it no longer directly follows a step that sets steps." + id + ".outputs.shearbolt-reached; the gate now runs on every run."
}

func earlyStops(p *plan.Plan) []plan.Note {
	var out []plan.Note
	for _, n := range p.Notes {
		if n.Code == "EARLY_STOP" {
			out = append(out, n)
		}
	}
	return out
}

// wantEarlyStop checks that p has exactly one EARLY_STOP, with message msg on the workflow's line.
func wantEarlyStop(t *testing.T, p *plan.Plan, msg string, line int) {
	t.Helper()
	ns := earlyStops(p)
	if len(ns) != 1 || ns[0].Message != msg || ns[0].File != wf || ns[0].Line != line {
		t.Errorf("EARLY_STOP notes = %+v\nwant one at %s:%d: %s", ns, wf, line, msg)
	}
}

// wantEarlyStopSays checks that p has exactly one EARLY_STOP, and that it contains text.
func wantEarlyStopSays(t *testing.T, p *plan.Plan, text string) {
	t.Helper()
	ns := earlyStops(p)
	if len(ns) != 1 || !strings.Contains(ns[0].Message, text) {
		t.Errorf("EARLY_STOP notes = %+v\nwant one that says %q", ns, text)
	}
}

// applied runs setup on files and writes its plan, and returns the tree's root.
func applied(t *testing.T, files map[string]string) string {
	t.Helper()
	root := tree(t, files)
	if err := setup.Write(root, compute(t, root)); err != nil {
		t.Fatal(err)
	}
	return root
}

func compute(t *testing.T, root string) *plan.Plan {
	t.Helper()
	p := setup.Compute(setup.Options{Root: root, Yes: true, Version: "1.2.3"})
	if p.Error != nil {
		t.Fatalf("plan error: %+v", p.Error)
	}
	return p
}

// recompute edits file path under root with edit, runs setup again, writes its plan, and returns
// the plan and the workflow it writes ("" when unchanged). A further run must change nothing and
// say nothing about early stops.
func recompute(t *testing.T, root, path string, edit func(string) string) (*plan.Plan, string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	src, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if next := edit(string(src)); next != string(src) {
		if err := os.WriteFile(full, []byte(next), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatalf("the edit of %s changes nothing:\n%s", path, src)
	}
	p := compute(t, root)
	out := ""
	if c := change(p, wf); c != nil {
		out = c.Content
	}
	if err := setup.Write(root, p); err != nil {
		t.Fatal(err)
	}
	again := compute(t, root)
	for _, c := range again.Changes {
		t.Errorf("a further run changes %s:\n%s", c.Path, c.Diff)
	}
	if ns := earlyStops(again); len(ns) > 0 {
		t.Errorf("a further run notes %+v", ns)
	}
	return p, out
}

// readFile returns file path under root.
func readFile(t *testing.T, root, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ES-1: the gate scans only the image built in the split step and nothing later pushes, so it
// is skipped on early stops like the rest of the step. A re-run changes nothing and says nothing.
func TestGitHubEarlyStopSkipsGate(t *testing.T) {
	p, got := result(t, rootRepo(head+earlyStop, nil))
	wantWorkflow(t, got, head+withGateIf(earlyStopEdited, reached)+lines("        if: "+reached)+continuedRun)
	wantEarlyStop(t, p, skipping("exit", 11), 11)

	root := applied(t, rootRepo(head+earlyStop, nil))
	again := compute(t, root)
	if len(again.Changes) > 0 || len(earlyStops(again)) > 0 {
		t.Errorf("a re-run changes %d files, notes %+v", len(again.Changes), earlyStops(again))
	}
}

// The gate's copied if: is combined with the marker, unwrapped, and the continued step's keeps
// no ${{ }} inside either.
func TestGitHubEarlyStopSkipsGateKeepsIf(t *testing.T) {
	const cond = "${{ github.event_name == 'push' }}"
	step := strings.Replace(earlyStop, "        run: |\n", "        if: "+cond+"\n        run: |\n", 1)
	edited := strings.Replace(earlyStopEdited, "        run: |\n", "        if: "+cond+"\n        run: |\n", 1)
	p, got := result(t, rootRepo(head+step, nil))
	wantWorkflow(t, got, head+withGateIf(edited, "(github.event_name == 'push') && "+reached)+lines(
		"        if: success() && (github.event_name == 'push') && "+reached,
	)+continuedRun)
	wantEarlyStop(t, p, skipping("exit", 12), 12)
}

// ES-2: a compose target keeps the gate running; only the continued step gets the marker.
func TestGitHubEarlyStopComposeKeepsGate(t *testing.T) {
	p, got := result(t, rootRepo(head+earlyStop, map[string]string{"docker-compose.yml": composeImage}))
	wantWorkflow(t, got, head+earlyStopEdited+lines("        if: "+reached)+continuedRun)
	wantEarlyStop(t, p, running("exit", 11, "it also checks docker-compose"), 11)
}

// ES-3: a later step that pushes keeps the gate running.
func TestGitHubEarlyStopLaterPushKeepsGate(t *testing.T) {
	later := lines("      - run: docker push other")
	p, got := result(t, rootRepo(head+earlyStop+later, nil))
	wantWorkflow(t, got, head+earlyStopEdited+lines("        if: "+reached)+continuedRun+later)
	wantEarlyStop(t, p, running("exit", 11, "a later step pushes or deploys"), 11)
}

// A later push step that runs only when the build ran, by setup's marker form, does not count.
func TestGitHubEarlyStopGuardedLaterPush(t *testing.T) {
	later := lines(
		"      - if: "+reached,
		"        run: docker push other",
	)
	p, got := result(t, rootRepo(head+earlyStop+later, nil))
	wantWorkflow(t, got, head+withGateIf(earlyStopEdited, reached)+lines("        if: "+reached)+continuedRun+later)
	wantEarlyStop(t, p, skipping("exit", 11), 11)
}

// A later push step guarded by setup's other marker form, success() && (<X>) && <marker>, does
// not count either.
func TestGitHubEarlyStopGuardedLaterPushSuccessForm(t *testing.T) {
	later := lines(
		"      - if: success() && (github.ref == 'refs/heads/main') && "+reached,
		"        run: docker push other",
	)
	p, got := result(t, rootRepo(head+earlyStop+later, nil))
	wantWorkflow(t, got, head+withGateIf(earlyStopEdited, reached)+lines("        if: "+reached)+continuedRun+later)
	wantEarlyStop(t, p, skipping("exit", 11), 11)
}

// Anything the gate scans besides the split step's own images keeps it running, and the note
// names it.
func TestGitHubEarlyStopOtherTargetsKeepGate(t *testing.T) {
	// An npm project's source target.
	p, got := result(t, npmRepo("edge", head+earlyStop, nil))
	wantWorkflow(t, got, head+earlyStopEdited+lines("        if: "+reached)+continuedRun)
	wantEarlyStop(t, p, running("exit", 11, "it also checks source"), 11)

	// An image target with a fixed reference: no image variable, so not the gate's own image.
	fixed := "version: 1\nproject: edge\ntargets:\n  - name: edge\n    image: ghcr.io/acme/edge:latest\n"
	p, got = result(t, rootRepo(head+earlyStop, map[string]string{"shearbolt.yaml": fixed}))
	if strings.Contains(got, "        if: "+reached+"\n        uses:") {
		t.Errorf("the gate is skipped:\n%s", got)
	}
	wantEarlyStopSays(t, p, "because it also checks edge.")

	// An image built in another step of the job. It fills the first image slot, so its target is
	// named after the project, that is the temporary directory.
	worker := lines(
		"      - name: Worker",
		"        run: docker build -f worker.Dockerfile -t ghcr.io/acme/worker:1 .",
	)
	p, got = result(t, rootRepo(head+worker+earlyStop, map[string]string{"worker.Dockerfile": "FROM alpine\n"}))
	if !strings.Contains(got, "      - name: shearbolt\n        uses:") {
		t.Errorf("the gate is skipped:\n%s", got)
	}
	cfg := change(p, "shearbolt.yaml")
	if cfg == nil {
		t.Fatalf("no shearbolt.yaml: %v", noteCodes(p))
	}
	m := regexp.MustCompile(`- name: (\S+)\n\s+image: \$\{SHEARBOLT_IMAGE:-`).FindStringSubmatch(cfg.Content)
	if m == nil || m[1] == "edge" {
		t.Fatalf("no first image target in\n%s", cfg.Content)
	}
	wantEarlyStopSays(t, p, "because it also checks "+strings.Trim(m[1], `"'`)+".")
}

// ES-4: after the compose target is removed from shearbolt.yaml, a re-run gives the gate the
// marker term and says so; another re-run changes nothing.
func TestGitHubEarlyStopReRunAddsTerm(t *testing.T) {
	root := applied(t, rootRepo(head+earlyStop, map[string]string{"docker-compose.yml": composeImage}))
	before := readFile(t, root, wf)
	p, got := recompute(t, root, "shearbolt.yaml", func(s string) string {
		return strings.Replace(s, "  - name: docker-compose\n    compose: docker-compose.yml\n", "", 1)
	})
	wantWorkflow(t, got, withGateIf(before, reached))
	wantEarlyStop(t, p, skipping("exit", 12), 12)
}

// A push step added after ES-1's result: the re-run removes the gate's marker term and says why.
func TestGitHubEarlyStopReRunRemovesTerm(t *testing.T) {
	root := applied(t, rootRepo(head+earlyStop, nil))
	before := readFile(t, root, wf)
	later := lines("      - run: docker push other")
	p, got := recompute(t, root, wf, func(s string) string { return s + later })
	wantWorkflow(t, got, strings.Replace(before, "        if: "+reached+"\n        uses:", "        uses:", 1)+later)
	wantEarlyStop(t, p, running("exit", 12, "a later step pushes or deploys"), 12)

	// With a condition of its own, the gate keeps it.
	const cond = "github.event_name == 'push'"
	step := strings.Replace(earlyStop, "        run: |\n", "        if: "+cond+"\n        run: |\n", 1)
	root = applied(t, rootRepo(head+step, nil))
	before = readFile(t, root, wf)
	if !strings.Contains(before, "        if: ("+cond+") && "+reached+"\n        uses:") {
		t.Fatalf("the gate has no combined condition:\n%s", before)
	}
	p, got = recompute(t, root, wf, func(s string) string { return s + later })
	wantWorkflow(t, got, strings.Replace(before, "        if: ("+cond+") && "+reached+"\n        uses:", "        if: "+cond+"\n        uses:", 1)+later)
	wantEarlyStop(t, p, running("exit", 13, "a later step pushes or deploys"), 13)
}

// A marker term the user reformatted is setup's term all the same: nothing is rewritten, and
// nothing is said.
func TestGitHubEarlyStopReformattedTerm(t *testing.T) {
	for _, term := range []string{
		"steps.shearbolt-build.outputs.shearbolt-reached  !=  ''",
		"${{ steps.shearbolt-build.outputs.shearbolt-reached != '' }}",
		"(true)  &&   steps.shearbolt-build.outputs.shearbolt-reached != ''",
	} {
		root := applied(t, rootRepo(head+earlyStop, nil))
		src := readFile(t, root, wf)
		edited := strings.Replace(src, "        if: "+reached+"\n        uses:", "        if: "+term+"\n        uses:", 1)
		if edited == src {
			t.Fatalf("no gate condition to reformat:\n%s", src)
		}
		if err := os.WriteFile(filepath.Join(root, wf), []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		p := compute(t, root)
		if len(p.Changes) > 0 || len(earlyStops(p)) > 0 {
			t.Errorf("%s: a re-run changes %d files, notes %+v", term, len(p.Changes), earlyStops(p))
		}
	}
}

// A shearbolt scan in a run step never gets an if: from setup: it would condition the step's
// other commands too.
func TestGitHubEarlyStopRunStepGate(t *testing.T) {
	p, got := result(t, rootRepo(head+lines(
		"      - name: Ship",
		"        run: |",
		`          if [ -n "$SKIP" ]; then exit 0; fi`,
		"          docker buildx build --push -t ghcr.io/acme/edge:latest .",
		"      - run: |",
		"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
		`          "$HOME/.local/bin/shearbolt" scan --ci`,
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Ship",
		"        id: shearbolt-build",
		"        run: |",
		`          if [ -n "$SKIP" ]; then exit 0; fi`,
		"          docker buildx build --load -t ghcr.io/acme/edge:latest -t ghcr.io/acme/edge:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=ghcr.io/acme/edge:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		`          echo shearbolt-reached=true >> "$GITHUB_OUTPUT"`,
		"      - run: |",
		"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
		`          "$HOME/.local/bin/shearbolt" scan --ci`,
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Ship (continued)",
		"        if: "+reached,
		"        run: docker push ghcr.io/acme/edge:latest",
	))
	wantEarlyStop(t, p, runningAdvice("exit", 10, "setup does not add an if: to a gate in a run step", adviceRunStep), 10)
}

// An existing action gate that does not directly follow the split step cannot carry the term:
// a re-run would not find the split step before it, so it could never take the term away.
func TestGitHubEarlyStopExistingGateNotNext(t *testing.T) {
	gate := strings.Replace(existingGate, "      - name: shearbolt\n", "      - run: echo tested\n      - name: shearbolt\n", 1)
	edited := strings.Replace(existingGateEdited, "      - name: shearbolt\n", "      - run: echo tested\n      - name: shearbolt\n", 1)
	p, got := result(t, rootRepo(head+gate, nil))
	wantWorkflow(t, got, head+edited)
	wantEarlyStop(t, p, runningAdvice("exit", 10, "it does not directly follow this step", adviceNotNext), 10)
}

// existingGate is a --push build that can stop early, before a gate the user already has.
const existingGate = "      - name: Ship\n" +
	"        run: |\n" +
	"          if [ -n \"$SKIP\" ]; then exit 0; fi\n" +
	"          docker buildx build --push -t ghcr.io/acme/edge:latest .\n" +
	"      - name: shearbolt\n" +
	"        uses: klar-web-services/shearbolt@v1\n"

const existingGateEdited = "      - name: Ship\n" +
	"        id: shearbolt-build\n" +
	"        run: |\n" +
	"          if [ -n \"$SKIP\" ]; then exit 0; fi\n" +
	"          docker buildx build --load -t ghcr.io/acme/edge:latest -t ghcr.io/acme/edge:shearbolt-${{ github.sha }} .\n" +
	"          echo shearbolt-image=ghcr.io/acme/edge:shearbolt-${{ github.sha }} >> \"$GITHUB_OUTPUT\"\n" +
	"          echo shearbolt-reached=true >> \"$GITHUB_OUTPUT\"\n" +
	"      - name: shearbolt\n" +
	"        uses: klar-web-services/shearbolt@v1\n" +
	"        env:\n" +
	"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}\n" +
	"      - name: Ship (continued)\n" +
	"        if: steps.shearbolt-build.outputs.shearbolt-reached != ''\n" +
	"        run: docker push ghcr.io/acme/edge:latest\n"

// A step split with the marker before an existing action gate: the gate gets the marker term
// when it may be skipped, and EARLY_STOP is added because the split is new.
func TestGitHubEarlyStopExistingGate(t *testing.T) {
	p, got := result(t, rootRepo(head+existingGate, nil))
	wantWorkflow(t, got, head+withGateIf(existingGateEdited, reached))
	wantEarlyStop(t, p, skipping("exit", 10), 10)
	again := compute(t, applied(t, rootRepo(head+existingGate, nil)))
	if len(again.Changes) > 0 || len(earlyStops(again)) > 0 {
		t.Errorf("a re-run changes %d files, notes %+v", len(again.Changes), earlyStops(again))
	}

	// A compose target keeps it running.
	p, got = result(t, rootRepo(head+existingGate, map[string]string{"docker-compose.yml": composeImage}))
	wantWorkflow(t, got, head+existingGateEdited)
	wantEarlyStop(t, p, running("exit", 10, "it also checks docker-compose"), 10)
}

// A gate condition with ${{ }} inside an expression is a string, which is always true, so the
// marker cannot be combined with it: the gate keeps running and the note says why.
func TestGitHubEarlyStopTemplateGateIf(t *testing.T) {
	const cond = "github.ref == ${{ env.REF }}"
	gate := strings.Replace(existingGate, "      - name: shearbolt\n", "      - name: shearbolt\n        if: "+cond+"\n", 1)
	p, got := result(t, rootRepo(head+gate, nil))
	wantWorkflow(t, got, head+withGateIf(existingGateEdited, cond))
	wantEarlyStop(t, p, runningAdvice("exit", 10, "its condition cannot be combined with the marker", adviceTemplate), 10)

	// On a re-run, a marker term next to such a condition is removed.
	root := applied(t, rootRepo(head+existingGate, nil))
	before := readFile(t, root, wf)
	p, got = recompute(t, root, wf, func(s string) string {
		return strings.Replace(s, "        if: "+reached+"\n        uses:", "        if: ("+cond+") && "+reached+"\n        uses:", 1)
	})
	wantWorkflow(t, got, strings.Replace(before, "        if: "+reached+"\n        uses:", "        if: "+cond+"\n        uses:", 1))
	wantEarlyStop(t, p, runningAdvice("exit", 11, "its condition cannot be combined with the marker", adviceTemplate), 11)
}

// No EARLY_STOP without an early stop, and none for a split without the marker.
func TestGitHubNoEarlyStopNote(t *testing.T) {
	for _, files := range []map[string]string{
		rootRepo(head+lines(
			"      - run: |",
			"          docker build -t ghcr.io/acme/edge:latest .",
			"          docker push ghcr.io/acme/edge:latest",
			"          exit 0",
		), nil),
		rootRepo(head+lines(
			"      - run: docker build -t ghcr.io/acme/edge:latest .",
			`      - run: '[ -n "$SKIP" ] && exit 0; docker push ghcr.io/acme/edge:latest'`,
		), nil),
	} {
		p, _ := result(t, files)
		if ns := earlyStops(p); len(ns) > 0 {
			t.Errorf("notes %+v for\n%s", ns, files[wf])
		}
	}
}

// The note names the command that stops: exec too.
func TestGitHubEarlyStopNamesExec(t *testing.T) {
	p, _ := result(t, rootRepo(head+lines(
		"      - name: Ship",
		"        run: |",
		`          if [ "$MODE" = other ]; then`,
		"            exec ./other.sh",
		"          fi",
		"          docker build -t ghcr.io/acme/edge:latest .",
		"          docker push ghcr.io/acme/edge:latest",
	), map[string]string{"other.sh*": "#!/bin/sh\necho other\n"}))
	wantEarlyStop(t, p, skipping("exec", 11), 11)
}

// gateTermLine is ES-1's gate with its marker term, as setup writes it.
const gateTermLine = "      - name: shearbolt\n        if: " + reached + "\n"

// stale applies ES-1, edits its workflow with edit, and runs setup again; a further run must
// change nothing. It returns the edited workflow, the plan and the workflow setup writes.
func stale(t *testing.T, edit func(string) string) (string, *plan.Plan, string) {
	t.Helper()
	root := applied(t, rootRepo(head+earlyStop, nil))
	edited := edit(readFile(t, root, wf))
	p, got := recompute(t, root, wf, func(string) string { return edited })
	return edited, p, got
}

// (A) A step inserted between the split step and its gate: the gate no longer directly follows
// a step that sets the marker, so a re-run takes the term away instead of keeping it while a
// later push goes unchecked.
func TestGitHubEarlyStopStaleTermStepBetween(t *testing.T) {
	later := lines("      - run: docker push other")
	edited, p, got := stale(t, func(s string) string {
		return strings.Replace(s, gateTermLine, "      - run: echo hi\n"+gateTermLine, 1) + later
	})
	wantWorkflow(t, got, strings.Replace(edited, gateTermLine, "      - name: shearbolt\n", 1))
	wantEarlyStop(t, p, removed("shearbolt-build"), 17)
}

// (B) The split step's id renamed, and the continued step updated: the term with the old id is
// replaced, not kept next to the new one, which would never let the gate run.
func TestGitHubEarlyStopStaleTermRenamedID(t *testing.T) {
	edited, p, got := stale(t, func(s string) string {
		s = strings.Replace(s, "        id: shearbolt-build\n", "        id: build\n", 1)
		i := strings.LastIndex(s, "        if: "+reached+"\n")
		return s[:i] + strings.Replace(s[i:], "steps.shearbolt-build.", "steps.build.", 1)
	})
	wantWorkflow(t, got, strings.ReplaceAll(edited, "steps.shearbolt-build.", "steps.build."))
	wantEarlyStop(t, p, skipping("exit", 12), 12)
}

// (G) The id deleted and a later push added: the re-run gives the step its id back and decides
// with it, so the term goes in one run.
func TestGitHubEarlyStopStaleTermDeletedID(t *testing.T) {
	later := lines("      - run: docker push other")
	edited, p, got := stale(t, func(s string) string {
		return strings.Replace(s, "        id: shearbolt-build\n", "", 1) + later
	})
	want := strings.Replace(edited, "      - name: Ship\n", "      - name: Ship\n        id: shearbolt-build\n", 1)
	wantWorkflow(t, got, strings.Replace(want, gateTermLine, "      - name: shearbolt\n", 1))
	wantEarlyStop(t, p, running("exit", 11, "a later step pushes or deploys"), 11)
}

// (H) The marker line deleted and a later push added: nothing sets the marker any more, so the
// term goes; kept, it would never let the gate run.
func TestGitHubEarlyStopStaleTermDeletedMarker(t *testing.T) {
	later := lines("      - run: docker push other")
	edited, p, got := stale(t, func(s string) string {
		return strings.Replace(s, "          echo shearbolt-reached=true >> \"$GITHUB_OUTPUT\"\n", "", 1) + later
	})
	wantWorkflow(t, got, strings.Replace(edited, gateTermLine, "      - name: shearbolt\n", 1))
	wantEarlyStop(t, p, removed("shearbolt-build"), 15)
}

// An if: on the item's dash line cannot be deleted, so a term that is the whole condition
// becomes if: success(), which means the same as no if:, and later runs leave it alone.
func TestGitHubEarlyStopDashLineIf(t *testing.T) {
	later := lines("      - run: docker push other")
	edited, p, got := stale(t, func(s string) string {
		return strings.Replace(s, gateTermLine, "      - if: "+reached+"\n        name: shearbolt\n", 1) + later
	})
	wantWorkflow(t, got, strings.Replace(edited, "      - if: "+reached+"\n", "      - if: success()\n", 1))
	wantEarlyStop(t, p, running("exit", 12, "a later step pushes or deploys"), 12)
}
