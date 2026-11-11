package github_test

import (
	"strings"
	"testing"
)

// earlyStop is a run step that can stop before its build, as in design §3.1.
const earlyStop = "      - name: Ship\n" +
	"        run: |\n" +
	"          set -eu\n" +
	"          if [ -n \"$SKIP\" ]; then exit 0; fi\n" +
	"          docker build -t ghcr.io/acme/edge:latest .\n" +
	"          docker push ghcr.io/acme/edge:latest\n"

// earlyStopEdited is earlyStop after setup, up to the continued step's if:.
const earlyStopEdited = "      - name: Ship\n" +
	"        id: shearbolt-build\n" +
	"        run: |\n" +
	"          set -eu\n" +
	"          if [ -n \"$SKIP\" ]; then exit 0; fi\n" +
	"          docker build -t ghcr.io/acme/edge:latest -t ghcr.io/acme/edge:shearbolt-${{ github.sha }} .\n" +
	"          echo shearbolt-image=ghcr.io/acme/edge:shearbolt-${{ github.sha }} >> \"$GITHUB_OUTPUT\"\n" +
	"          echo shearbolt-reached=true >> \"$GITHUB_OUTPUT\"\n" +
	"      - name: shearbolt\n" +
	"        uses: klar-web-services/shearbolt@v1\n" +
	"        env:\n" +
	"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}\n" +
	"      - name: Ship (continued)\n"

const continuedRun = "        run: |\n" +
	"          set -eu\n" +
	"          docker push ghcr.io/acme/edge:latest\n"

// Design §3.2: a split step that can stop before its build sets setup's marker right after the
// build, and its continued step runs only when the marker is set.
func TestGitHubEarlyStopMarker(t *testing.T) {
	want := head + earlyStopEdited + lines("        if: steps.shearbolt-build.outputs.shearbolt-reached != ''") + continuedRun
	_, got := result(t, npmRepo("edge", head+earlyStop, nil))
	wantWorkflow(t, got, want)
	// The root project's gate, which scans only that image, is skipped too: see
	// TestGitHubEarlyStopSkipsGate.
}

// The continued step keeps the step's own if:, unwrapped, in front of the marker.
func TestGitHubEarlyStopKeepsStepIf(t *testing.T) {
	for _, cond := range []string{"github.ref == 'refs/heads/main'", "${{ github.ref == 'refs/heads/main' }}"} {
		step := strings.Replace(earlyStop, "        run: |\n", "        if: "+cond+"\n        run: |\n", 1)
		edited := strings.Replace(earlyStopEdited, "        run: |\n", "        if: "+cond+"\n        run: |\n", 1)
		edited = strings.Replace(edited, "      - name: shearbolt\n", "      - name: shearbolt\n        if: "+cond+"\n", 1)
		_, got := result(t, npmRepo("edge", head+step, nil))
		wantWorkflow(t, got, head+edited+lines(
			"        if: success() && (github.ref == 'refs/heads/main') && steps.shearbolt-build.outputs.shearbolt-reached != ''",
		)+continuedRun)
	}
}

// A copied if: with a status function needs no success() && (...) wrap of its own: the
// continued step's success() && (<copied>) && <marker> is already safe, and a re-run leaves it
// alone (R8) instead of wrapping it as a push step that would run after a failed gate.
func TestGitHubEarlyStopStatusIfSurvivesReRun(t *testing.T) {
	for cond, bare := range map[string]string{"${{ !cancelled() }}": "!cancelled()", "always()": "always()"} {
		step := strings.Replace(earlyStop, "        run: |\n", "        if: "+cond+"\n        run: |\n", 1)
		edited := strings.Replace(earlyStopEdited, "        run: |\n", "        if: "+cond+"\n        run: |\n", 1)
		edited = strings.Replace(edited, "      - name: shearbolt\n", "      - name: shearbolt\n        if: "+cond+"\n", 1)
		p, got := result(t, npmRepo("edge", head+step, nil))
		wantWorkflow(t, got, head+edited+lines(
			"        if: success() && ("+bare+") && steps.shearbolt-build.outputs.shearbolt-reached != ''",
		)+continuedRun)
		if hasNote(p, "PUSH_IF_WRAPPED") {
			t.Errorf("%s: notes = %v", cond, noteCodes(p))
		}
	}
}

// exec in an if before the build replaces the shell, so it stops the step early too.
func TestGitHubEarlyStopExec(t *testing.T) {
	_, got := result(t, npmRepo("edge", head+lines(
		"      - name: Ship",
		"        run: |",
		`          if [ "$MODE" = other ]; then`,
		"            exec ./other.sh",
		"          fi",
		"          docker build -t ghcr.io/acme/edge:latest .",
		"          docker push ghcr.io/acme/edge:latest",
	), map[string]string{"other.sh*": "#!/bin/sh\necho other\n"}))
	wantWorkflow(t, got, head+lines(
		"      - name: Ship",
		"        id: shearbolt-build",
		"        run: |",
		`          if [ "$MODE" = other ]; then`,
		"            exec ./other.sh",
		"          fi",
		"          docker build -t ghcr.io/acme/edge:latest -t ghcr.io/acme/edge:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=ghcr.io/acme/edge:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		`          echo shearbolt-reached=true >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Ship (continued)",
		"        if: steps.shearbolt-build.outputs.shearbolt-reached != ''",
		"        run: |",
		"          docker push ghcr.io/acme/edge:latest",
	))
}

// A stop after the split point does not end part 1, so the split gets no marker.
func TestGitHubStopAfterSplitNoMarker(t *testing.T) {
	_, got := result(t, npmRepo("edge", head+lines(
		"      - name: Ship",
		"        run: |",
		"          docker build -t ghcr.io/acme/edge:latest .",
		"          docker push ghcr.io/acme/edge:latest",
		"          exit 0",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Ship",
		"        id: shearbolt-build",
		"        run: |",
		"          docker build -t ghcr.io/acme/edge:latest -t ghcr.io/acme/edge:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=ghcr.io/acme/edge:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Ship (continued)",
		"        run: |",
		"          docker push ghcr.io/acme/edge:latest",
		"          exit 0",
	))
}

// A source build (no image output) that can stop early gets the marker and an id as well.
func TestGitHubEarlyStopSourceBuild(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: |",
		`          [ "$GITHUB_REF" = refs/heads/main ] || exit 0`,
		"          npm ci && npm run build && npx vercel deploy --prebuilt",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - id: shearbolt-build",
		"        run: |",
		`          [ "$GITHUB_REF" = refs/heads/main ] || exit 0`,
		`          npm ci && npm run build && echo shearbolt-reached=true >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"      - name: Continue after shearbolt",
		"        if: steps.shearbolt-build.outputs.shearbolt-reached != ''",
		"        run: |",
		"          npx vercel deploy --prebuilt",
	))
}

// An if: that mixes ${{ }} with other text is a non-empty string, which is always true, so it
// cannot carry the marker: the job is left unchanged rather than continued on early stops.
func TestGitHubEarlyStopMixedIfRefused(t *testing.T) {
	step := strings.Replace(earlyStop, "        run: |\n", "        if: github.ref == ${{ env.REF }}\n        run: |\n", 1)
	wantUnsupported(t, npmRepo("edge", head+step, nil), "mixed if: on an early-stop step", "cannot be combined with the check that the build ran")
}

// shipWith is a run step that runs pre before its build and post after it.
func shipWith(pre, post string) string {
	return "      - name: Ship\n        run: |\n" + indent(pre, "          ") +
		"          docker build -t ghcr.io/acme/edge:latest .\n" + indent(post, "          ")
}

// shipEdited is shipWith after setup when pre is not carried, up to the continued step's run.
func shipEdited(pre string) string {
	return "      - name: Ship\n        id: shearbolt-build\n        run: |\n" + indent(pre, "          ") + lines(
		"          docker build -t ghcr.io/acme/edge:latest -t ghcr.io/acme/edge:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=ghcr.io/acme/edge:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Ship (continued)",
	)
}

const (
	pushLatest = "docker push ghcr.io/acme/edge:latest"
	pushRun    = "        run: |\n          " + pushLatest + "\n"
	pulled     = "live=$(docker pull -q ghcr.io/acme/edge:latest || true)"
	substMsg   = "uses command substitution, so a split step cannot re-create it"
	condMsg    = "changes the shell state inside a conditional or loop, which a split step cannot re-create"
	pipeMsg    = "runs only under a condition or in a pipeline, so a split step cannot re-create its state"
)

// CS-1 (design §4): a value computed before the build that nothing after it reads does not
// block the split, and the continued step does not compute it again.
func TestGitHubUnreadSubstitutionDropped(t *testing.T) {
	_, got := result(t, npmRepo("edge", head+shipWith(pulled, pushLatest), nil))
	wantWorkflow(t, got, head+shipEdited(pulled)+pushRun)
}

// CS-2: a value that the rest of the step reads fails the split with today's message.
func TestGitHubReadSubstitutionRefused(t *testing.T) {
	wantUnsupported(t, npmRepo("edge", head+shipWith(pulled, pushLatest+"\necho \"$live\""), nil), "direct read", substMsg)
}

// Reads that never name the variable directly, or name it only in arithmetic or a test, count
// as reads too, and so do an export, readonly or allexport anywhere around it.
func TestGitHubIndirectReadsRefused(t *testing.T) {
	for why, c := range map[string]struct{ pre, post string }{
		"arithmetic":           {pulled, "(( live )) || ./deploy.sh"},
		"[[ -v ]]":             {pulled, "[[ -v live ]] && ./deploy.sh"},
		"prefix expansion":     {"live_rev=$(git rev-parse HEAD)", `echo "${!live_@}"`},
		"eval":                 {pulled, `eval "$CMD"`},
		"source":               {pulled, "source ./env.sh"},
		".":                    {pulled, ". ./env.sh"},
		"eval in substitution": {pulled, `echo "$(eval "$CMD")"`},
		"declare -n":           {pulled, "declare -n r=live"},
		"declare -n indirect":  {pulled, `declare -n r="$TARGET"`},
		"typeset -n":           {pulled, `typeset -n r="$TARGET"`},
		"bare set":             {pulled, "set"},
		"declare -p":           {pulled, "declare -p"},
		"export -p":            {pulled, "export -p"},
		"compgen -v":           {pulled, "compgen -v"},
		"compgen -A variable":  {pulled, "compgen -A variable"},
		"export later":         {pulled, "export live"},
		"readonly":             {pulled, "readonly live"},
		"set -a before":        {"set -a\n" + pulled, ""},
		"set -o allexport":     {"set -o allexport\n" + pulled, ""},
	} {
		post := pushLatest
		if c.post != "" {
			post += "\n" + c.post
		}
		wantUnsupported(t, npmRepo("edge", head+shipWith(c.pre, post), map[string]string{"env.sh": "echo env\n", "deploy.sh*": "#!/bin/sh\necho deploy\n"}), why, substMsg)
	}
}

// An assignment inside an if that nothing after the build reads is left out of the continued
// step; one that is read fails with today's message about conditionals.
func TestGitHubUnreadConditionalAssignmentDropped(t *testing.T) {
	cond := `if [ -n "$X" ]; then n=1; fi`
	_, got := result(t, npmRepo("edge", head+shipWith(cond, pushLatest), nil))
	wantWorkflow(t, got, head+shipEdited(cond)+pushRun)
	wantUnsupported(t, npmRepo("edge", head+shipWith(cond, pushLatest+"\necho \"$n\""), nil), "read conditional assignment", condMsg)
	and := `[ -n "$X" ] && n=1`
	_, got = result(t, npmRepo("edge", head+shipWith(and, pushLatest), nil))
	wantWorkflow(t, got, head+shipEdited(and)+pushRun)
	wantUnsupported(t, npmRepo("edge", head+shipWith(and, pushLatest+"\necho \"$n\""), nil), "read assignment after &&", pipeMsg)
}

// A plain assignment is still carried, read or not.
func TestGitHubPlainAssignmentCarried(t *testing.T) {
	_, got := result(t, npmRepo("edge", head+shipWith("FOO=bar", pushLatest+"\necho \"$FOO\""), nil))
	wantWorkflow(t, got, head+shipEdited("FOO=bar")+lines(
		"        run: |",
		"          FOO=bar",
		"          "+pushLatest,
		`          echo "$FOO"`,
	))
}

// An assignment in a pipeline or in the background still fails, read or not, and so does a
// state change inside an if that assigns no name.
func TestGitHubUnsplittableAssignmentsStillRefused(t *testing.T) {
	for why, c := range map[string]struct{ pre, msg string }{
		"pipeline":   {"n=$(date) | cat", pipeMsg},
		"background": {"n=$(date) &", pipeMsg},
		"no names":   {`if [ -n "$X" ]; then exec >log 2>&1; fi`, condMsg},
	} {
		wantUnsupported(t, npmRepo("edge", head+shipWith(c.pre, pushLatest), nil), why, c.msg)
	}
}
