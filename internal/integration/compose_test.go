//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/model"
)

// cleanImage is gcr.io/distroless/static-debian12 pinned by digest (multi-arch index). Chosen on
// 2026-11-09: the candidates distroless static, distroless base, busybox and alpine were to be
// tried smallest first with `shearbolt scan --image` on the pinned snapshot under snapshotPolicy;
// distroless static was the first and passed with exit 0, so the others were not needed.
const cleanImage = "gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2"

// composeTarget is a config target for a compose file with the given content, written into a temp
// folder and referenced by absolute path.
func composeTarget(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("  - name: %s\n    compose: %s\n", name, path)
}

func TestComposeVulnerableImage(t *testing.T) {
	code, r := snapshotScan(t, composeTarget(t, "stack", fmt.Sprintf("services:\n  node:\n    image: %s\n", nodeImage)))
	// The suite's node image has Node.js runtime findings under the fixture policy, so exit 1 is
	// expected; TestNodeImageReportsRuntimeVulnerabilities only requires != 2.
	if code != 1 {
		t.Fatalf("exit = %d, want 1; errors = %+v", code, r.Errors)
	}
	if !slices.ContainsFunc(r.Findings, func(f model.Finding) bool { return f.Image == nodeImage }) {
		t.Fatalf("no finding with image %s; findings = %+v", nodeImage, r.Findings)
	}
	images := target(r, "stack").Images
	if len(images) != 1 || images[0].Identity.ManifestDigest == "" {
		t.Fatalf("images = %+v, want one with a manifest digest", images)
	}
}

func TestComposeCleanImage(t *testing.T) {
	code, r := snapshotScan(t, composeTarget(t, "stack", fmt.Sprintf("services:\n  app:\n    image: %s\n", cleanImage)))
	if code != 0 {
		t.Fatalf("exit = %d, want 0; errors = %+v, findings = %+v", code, r.Errors, r.Findings)
	}
	if images := target(r, "stack").Images; len(images) != 1 || images[0].Status != "scanned" || images[0].Packages == 0 {
		t.Fatalf("images = %+v, want one scanned image with packages", images)
	}
}

func TestComposeBroken(t *testing.T) {
	t.Run("no images", func(t *testing.T) {
		code, r := snapshotScan(t, composeTarget(t, "stack", "services:\n  app:\n    build: .\n"))
		if code != 2 || !hasCode(r.Errors, "COMPOSE_NO_IMAGES", "stack") {
			t.Fatalf("exit = %d, errors = %+v", code, r.Errors)
		}
	})
	t.Run("unset variable", func(t *testing.T) {
		code, r := snapshotScan(t, composeTarget(t, "stack", "services:\n  app:\n    image: ${UNSET_FOR_TEST}\n"))
		if code != 2 || !hasCode(r.Errors, "COMPOSE_VARIABLE", "stack") {
			t.Fatalf("exit = %d, errors = %+v", code, r.Errors)
		}
	})
}
