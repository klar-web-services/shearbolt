package github_test

import "testing"

// The marker joins the build with the build's own && after the output line, so a failed build
// still fails part 1: on a new line it would follow a failed && list and succeed.
func TestGitHubMarkerJoinsBuild(t *testing.T) {
	_, got := result(t, npmRepo("edge", head+lines(
		"      - name: Ship",
		"        run: |",
		`          if [ -n "$SKIP" ]; then exit 0; fi`,
		"          npm ci && docker build -t ghcr.io/acme/edge:latest . && docker push ghcr.io/acme/edge:latest",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Ship",
		"        id: shearbolt-build",
		"        run: |",
		`          if [ -n "$SKIP" ]; then exit 0; fi`,
		`          npm ci && docker build -t ghcr.io/acme/edge:latest -t ghcr.io/acme/edge:shearbolt-${{ github.sha }} . && echo shearbolt-image=ghcr.io/acme/edge:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT" && echo shearbolt-reached=true >> "$GITHUB_OUTPUT"`,
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

// A --push build at the end of a one-line script: the marker follows the output line on the
// next line, without an empty line between them.
func TestGitHubMarkerAtScriptEnd(t *testing.T) {
	_, got := result(t, npmRepo("edge", head+lines(
		"      - name: Image",
		`        run: '[ -n "$SKIP" ] && exit 0; docker buildx build --push -t ghcr.io/acme/edge:latest .'`,
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Image",
		"        id: shearbolt-build",
		"        run: |",
		`          [ -n "$SKIP" ] && exit 0; docker buildx build --load -t ghcr.io/acme/edge:latest -t ghcr.io/acme/edge:shearbolt-${{ github.sha }} .`,
		`          echo shearbolt-image=ghcr.io/acme/edge:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		`          echo shearbolt-reached=true >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Image (continued)",
		"        if: steps.shearbolt-build.outputs.shearbolt-reached != ''",
		"        run: docker push ghcr.io/acme/edge:latest",
	))
}

// The existing output line stays with the build, and the marker follows it.
func TestGitHubMarkerAfterExistingOutputLine(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - name: Build",
		"        id: shearbolt-build",
		"        run: |",
		`          if [ -n "$SKIP" ]; then exit 0; fi`,
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"          ./deploy.sh",
	), map[string]string{"deploy.sh*": "#!/bin/sh\necho deploy\n"}))
	wantWorkflow(t, got, head+lines(
		"      - name: Build",
		"        id: shearbolt-build",
		"        run: |",
		`          if [ -n "$SKIP" ]; then exit 0; fi`,
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		`          echo shearbolt-reached=true >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Build (continued)",
		"        if: steps.shearbolt-build.outputs.shearbolt-reached != ''",
		"        run: |",
		"          ./deploy.sh",
	))
}
