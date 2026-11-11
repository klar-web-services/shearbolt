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

	// The root project owns the image build when there is only a Dockerfile.
	_, got = result(t, map[string]string{"Dockerfile": "FROM alpine\n", wf: head + earlyStop})
	wantWorkflow(t, got, want)
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
