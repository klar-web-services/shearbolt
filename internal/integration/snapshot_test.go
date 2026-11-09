//go:build integration

package integration

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/model"
)

// Deterministic tests on the pinned database snapshot. Fixtures are grouped into one config per
// test, because every scan re-imports the snapshot. Expectations are spec section 13's
// "supported" matrix and, for advisories, Grype 0.121.2 on the pinned snapshot (2026-11-09).

func TestVulnerableLockfilesBlock(t *testing.T) {
	lockfiles := map[string]string{
		"npm-vulnerable":  "package-lock.json",
		"pnpm-vulnerable": "pnpm-lock.yaml",
		"yarn-vulnerable": "yarn.lock",
		"bun-vulnerable":  "bun.lock",
	}
	var targets strings.Builder
	for _, name := range slices.Sorted(maps.Keys(lockfiles)) {
		targets.WriteString(sourceTarget(name, "projects", name))
	}
	code, r := snapshotScan(t, targets.String())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; errors = %+v", code, r.Errors)
	}
	for name, lockfile := range lockfiles {
		if !blockedIn(r, name, "lodash", "4.17.20", lockfile) {
			t.Errorf("%s: lodash 4.17.20 not blocked from its %s", name, lockfile)
		}
	}
}

func TestCleanLockfilesPass(t *testing.T) {
	names := []string{"npm-clean", "pnpm-clean", "yarn-clean", "bun-clean", "cargo-clean"}
	var targets strings.Builder
	for _, name := range names {
		targets.WriteString(sourceTarget(name, "projects", name))
	}
	code, r := snapshotScan(t, targets.String())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; errors = %+v, findings = %+v", code, r.Errors, r.Findings)
	}
	for _, name := range names {
		if status, packages := targetStatus(r, name), target(r, name).Packages; status != "scanned" || packages == 0 {
			t.Errorf("%s: status %q with %d packages, want scanned with packages", name, status, packages)
		}
	}
}

func TestBrokenLockfilesAreErrors(t *testing.T) {
	names := []string{"broken-lockfile", "pnpm-broken", "yarn-broken", "bun-broken", "cargo-broken"}
	var targets strings.Builder
	for _, name := range names {
		targets.WriteString(sourceTarget(name, "projects", name))
	}
	code, r := snapshotScan(t, targets.String())
	if code != 2 {
		t.Fatalf("exit = %d, want 2; errors = %+v", code, r.Errors)
	}
	for _, name := range names {
		if !hasCode(r.Errors, "LOCKFILE_UNREAD", name) || targetStatus(r, name) != "error" {
			t.Errorf("%s: status %q, want error with LOCKFILE_UNREAD; errors = %+v", name, targetStatus(r, name), r.Errors)
		}
	}
}

func TestSourceWithoutLockfileIsAnError(t *testing.T) {
	code, r := snapshotScan(t, sourceTarget("no-lockfile", "projects", "no-lockfile"))
	if code != 2 || !hasCode(r.Errors, "NO_LOCKFILE", "no-lockfile") {
		t.Fatalf("exit = %d, errors = %+v", code, r.Errors)
	}
}

// Grype skips devDependencies for yarn and bun, so these lockfiles yield no packages. The scan
// passes with a warning instead of failing as an unread lockfile.
func TestDevOnlyLockfilesPassWithWarning(t *testing.T) {
	names := []string{"yarn-dev-only", "bun-dev-only"}
	code, r := snapshotScan(t, sourceTarget(names[0], "projects", names[0])+sourceTarget(names[1], "projects", names[1]))
	if code != 0 {
		t.Fatalf("exit = %d, want 0; errors = %+v", code, r.Errors)
	}
	for _, name := range names {
		if !hasCode(r.Warnings, "DEV_ONLY_LOCKFILE", name) {
			t.Errorf("%s: no DEV_ONLY_LOCKFILE warning; warnings = %+v", name, r.Warnings)
		}
	}
}

// nested-yarn: Syft judges a yarn.lock's devDependencies from the scan root's package.json, so a
// nested project that ships lodash (which the root lists as a devDependency) lost it; the nested
// project is scanned on its own with its own package.json.
// yarn-workspace: Syft drops every package reachable from a workspace root's devDependencies, even
// when a member ships it (qs and body-parser, through express); the lockfile is scanned on its own.
// nested-yarn-workspace: the same for a workspace root in a subfolder.
func TestNestedAndWorkspaceYarn(t *testing.T) {
	code, r := snapshotScan(t, sourceTarget("nested-yarn", "projects", "nested-yarn")+
		sourceTarget("yarn-workspace", "projects", "yarn-workspace")+
		sourceTarget("nested-yarn-workspace", "projects", "nested-yarn-workspace"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1; errors = %+v", code, r.Errors)
	}
	if !blockedIn(r, "nested-yarn", "lodash", "4.17.20", "app/yarn.lock") {
		t.Errorf("nested-yarn: lodash not blocked from app/yarn.lock")
	}
	if !blockedIn(r, "yarn-workspace", "qs", "6.7.0", "yarn.lock") || !blockedIn(r, "yarn-workspace", "body-parser", "1.19.0", "yarn.lock") ||
		!hasCode(r.Warnings, "YARN_WORKSPACE_DEV_INCLUDED", "yarn-workspace") {
		t.Errorf("yarn-workspace: qs and body-parser must block, with YARN_WORKSPACE_DEV_INCLUDED; warnings = %+v", r.Warnings)
	}
	if !blockedIn(r, "nested-yarn-workspace", "lodash", "4.17.20", "nest/yarn.lock") || !hasCode(r.Warnings, "YARN_WORKSPACE_DEV_INCLUDED", "nested-yarn-workspace") {
		t.Errorf("nested-yarn-workspace: lodash must block from nest/yarn.lock, with YARN_WORKSPACE_DEV_INCLUDED; warnings = %+v", r.Warnings)
	}
}

// Grype reads .grype.yaml from its working directory, so the scan runs from inside the fixture:
// without Shearbolt's isolation (private cwd + its own config) lodash would be suppressed here.
func TestProjectGrypeConfigCannotSuppressFindings(t *testing.T) {
	t.Chdir(fixture("projects", "grype-ignore"))
	if code, r := snapshotScan(t, sourceTarget("repo", "projects", "grype-ignore")); code != 1 || !blocked(r, "repo", "lodash", "4.17.20") {
		t.Fatalf("the project's .grype.yaml must be ignored: exit = %d, findings = %d, errors = %+v", code, len(r.Findings), r.Errors)
	}
}

func TestNodeImageReportsRuntimeVulnerabilities(t *testing.T) {
	code, r := snapshotScan(t, fmt.Sprintf("  - name: node\n    image: %s\n", nodeImage))
	if code == 2 {
		t.Fatalf("scan failed: %+v", r.Errors)
	}
	if !slices.ContainsFunc(r.Findings, func(f model.Finding) bool { return f.Package == "node" && f.Type == "binary" }) {
		t.Fatal("no Node.js runtime findings")
	}
	if target(r, "node").Identity.ManifestDigest == "" {
		t.Fatal("image identity was not recorded")
	}
}

func TestRustBinaries(t *testing.T) {
	t.Run("auditable binary reports crates", func(t *testing.T) {
		code, r := snapshotScan(t, fmt.Sprintf("  - name: cli\n    file: %s\n", quoted("rust", "bin", "demo-auditable-linux-amd64")))
		if code != 1 || !blocked(r, "cli", "smallvec", "1.6.0") {
			t.Fatalf("exit = %d, errors = %+v, findings = %+v", code, r.Errors, r.Findings)
		}
	})
	t.Run("plain binary without fallback is an error", func(t *testing.T) {
		code, r := snapshotScan(t, fmt.Sprintf("  - name: cli\n    file: %s\n", quoted("rust", "bin", "demo-plain-linux-amd64")))
		if code != 2 || !hasCode(r.Errors, "NO_PACKAGES", "cli") {
			t.Fatalf("exit = %d, errors = %+v, findings = %+v", code, r.Errors, r.Findings)
		}
	})
	t.Run("plain binary with fallback uses the source target", func(t *testing.T) {
		code, r := snapshotScan(t, fmt.Sprintf("  - name: cli\n    file: %s\n    lockfileFallback: %s\n", quoted("rust", "bin", "demo-plain-linux-amd64"), quoted("rust", "demo", "Cargo.lock"))+
			sourceTarget("repo", "rust", "demo"))
		if code != 1 || !hasCode(r.Warnings, "NOT_AUDITABLE", "cli") || !blocked(r, "repo", "smallvec", "1.6.0") || blocked(r, "cli", "smallvec", "1.6.0") {
			t.Fatalf("exit = %d, warnings = %+v, errors = %+v, findings = %+v", code, r.Warnings, r.Errors, r.Findings)
		}
	})
}

// With no source target covering the lockfile, the fallback scans the lockfile itself.
func TestRustFallbackScansTheLockfileItself(t *testing.T) {
	code, r := snapshotScan(t, fmt.Sprintf("  - name: cli\n    file: %s\n    lockfileFallback: %s\n", quoted("rust", "bin", "demo-plain-linux-amd64"), quoted("rust", "demo", "Cargo.lock")))
	if code != 1 || !hasCode(r.Warnings, "NOT_AUDITABLE", "cli") || !blocked(r, "cli", "smallvec", "1.6.0") {
		t.Fatalf("exit = %d, warnings = %+v, errors = %+v, findings = %+v", code, r.Warnings, r.Errors, r.Findings)
	}
	raw := "raw/cli+lockfile.grype.json" // Plan 1: fallback raw files are named <target>+lockfile
	if !slices.Contains(target(r, "cli").Raw, raw) {
		t.Errorf("target cli lists raw files %v, want %s", target(r, "cli").Raw, raw)
	}
	if _, err := os.Stat(filepath.Join(r.EvidenceDir, filepath.FromSlash(raw))); err != nil {
		t.Errorf("fallback raw report missing: %v", err)
	}
}

// jquery 3.4.1: GHSA-jpcq-cgw6-v4j6 is known exploited through its related CVE-2020-11023 (EPSS
// 0.849); GHSA-gxr4-xjj5-5px2 is not (EPSS 0.992). The KEV-only policy isolates the KEV reason.
func TestKnownExploitedBlocksWithoutFixPolicy(t *testing.T) {
	code, r := snapshotScanWithPolicy(t, sourceTarget("repo", "projects", "npm-kev"),
		"policy:\n  blockFixable: never\n  blockEpssAtLeast: 1\n  maxDatabaseAgeHours: 1000000\n")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; errors = %+v", code, r.Errors)
	}
	var blocking []model.Finding
	warned := false
	for _, f := range r.Findings {
		switch {
		case f.Decision.Action == model.Block:
			blocking = append(blocking, f)
		case f.ID == "GHSA-gxr4-xjj5-5px2" && f.Decision.Action == model.Warn:
			warned = true
		}
	}
	if len(blocking) != 1 || blocking[0].ID != "GHSA-jpcq-cgw6-v4j6" || !slices.Contains(blocking[0].Decision.Reasons, "known exploited (CISA KEV)") {
		t.Fatalf("blocking findings = %+v, want only GHSA-jpcq-cgw6-v4j6 with reason \"known exploited (CISA KEV)\"", blocking)
	}
	if !warned {
		t.Fatalf("GHSA-gxr4-xjj5-5px2 is not a warning; findings = %+v", r.Findings)
	}
}

func TestUpgradeLineOnSnapshot(t *testing.T) {
	code, r := snapshotScan(t, sourceTarget("repo", "projects", "npm-vulnerable"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1; errors = %+v", code, r.Errors)
	}
	summary, err := os.ReadFile(filepath.Join(r.EvidenceDir, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "- Upgrade lodash to 4.18.0 to clear all 4 of its blocking vulnerabilities.\n"; !strings.Contains(string(summary), want) {
		t.Fatalf("summary.md lacks %q:\n%s", want, summary)
	}
}
