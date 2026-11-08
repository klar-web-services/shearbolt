package scan

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/testutil"
)

const (
	rootDevLodash = `{"devDependencies":{"lodash":"4.17.20"}}`
	appDeps       = `{"dependencies":{"lodash":"4.17.20","is-number":"7.0.0"}}`
)

func lodashFinding(location string) testutil.Match {
	return testutil.Match{ID: "GHSA-1", Package: "lodash", Version: "4.17.20", FixState: "fixed", FixVersions: []string{"4.17.21"}, Location: location}
}

func nestedFiles(appPkg string) map[string]string {
	return map[string]string{"package.json": rootDevLodash, "app/package.json": appPkg, "app/yarn.lock": "# yarn"}
}

func TestNestedYarnProjectFindingsComeFromItsOwnScan(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(appDeps))
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh), testutil.SBOMAt("/app/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"dir:" + filepath.Join(root, "app"): {testutil.GrypeJSON(fresh, lodashFinding("/yarn.lock")), testutil.SBOMAt("/yarn.lock", "pkg:npm/lodash@4.17.20", "pkg:npm/is-number@7.0.0"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 1 || len(r.Errors) != 0 || len(r.Findings) != 1 {
		t.Fatalf("exit = %d, errors = %+v, findings = %+v", r.ExitCode, r.Errors, r.Findings)
	}
	f := r.Findings[0]
	if f.Package != "lodash" || f.Target != "repo" || !slices.Equal(f.Locations, []string{"/app/yarn.lock"}) {
		t.Fatalf("finding = %+v", f)
	}
	if len(sc.calls) != 2 || len(sc.calls[1].Exclude) == 0 {
		t.Fatalf("calls = %+v", sc.calls)
	}
}

func TestNestedYarnProjectFindingsAreDeduplicated(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(appDeps))
	isNumber := testutil.Match{ID: "GHSA-2", Package: "is-number", Version: "7.0.0", FixState: "fixed", FixVersions: []string{"7.0.1"}}
	rootMatch, subMatch := isNumber, isNumber
	rootMatch.Location, subMatch.Location = "/app/yarn.lock", "/yarn.lock"
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh, rootMatch), testutil.SBOMAt("/app/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"dir:" + filepath.Join(root, "app"): {testutil.GrypeJSON(fresh, subMatch), testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if len(r.Findings) != 1 || !slices.Equal(r.Findings[0].Locations, []string{"/app/yarn.lock"}) {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

func TestNestedDevOnlyYarnProjectPassesWithWarning(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(`{"devDependencies":{"a":"1.0.0"}}`))
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh), testutil.SBOMJSON(), nil},
		"dir:" + filepath.Join(root, "app"): {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 0 || codes(r.Warnings) != "DEV_ONLY_LOCKFILE" {
		t.Fatalf("exit = %d, errors = %+v, warnings = %+v", r.ExitCode, r.Errors, r.Warnings)
	}
}

func TestNestedYarnSubScanFailureIsAnError(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(appDeps))
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh), testutil.SBOMAt("/app/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"dir:" + filepath.Join(root, "app"): {err: errors.New("boom")},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "SCAN_FAILED" {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestRootLevelYarnLockfileGetsNoExtraScan(t *testing.T) {
	cfg, root := project(t, sourceTarget, map[string]string{"package.json": appDeps, "yarn.lock": "# yarn"})
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 0 || len(sc.calls) != 1 {
		t.Fatalf("exit = %d, errors = %+v, calls = %d", r.ExitCode, r.Errors, len(sc.calls))
	}
}

func TestNestedSubScanFailureNamesTheLockfile(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(appDeps))
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh), testutil.SBOMAt("/app/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"dir:" + filepath.Join(root, "app"): {err: errors.New("boom")},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Message, "app/yarn.lock") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestNestedSubScanBadOutputIsAnError(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(appDeps))
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh), testutil.SBOMAt("/app/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"dir:" + filepath.Join(root, "app"): {"not json", testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "BAD_OUTPUT" || !strings.Contains(r.Errors[0].Message, "app/yarn.lock") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestNestedSubScanFindingsOutsideTheLockfileAreIgnored(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(appDeps))
	other := lodashFinding("/package.json")
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh), testutil.SBOMAt("/app/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"dir:" + filepath.Join(root, "app"): {testutil.GrypeJSON(fresh, other), testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 0 || len(r.Findings) != 0 {
		t.Fatalf("exit = %d, errors = %+v, findings = %+v", r.ExitCode, r.Errors, r.Findings)
	}
}

func TestNestedRootFindingMissingFromSubScanIsRemoved(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(appDeps))
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh, lodashFinding("/app/yarn.lock")), testutil.SBOMAt("/app/yarn.lock", "pkg:npm/lodash@4.17.20"), nil},
		"dir:" + filepath.Join(root, "app"): {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 0 || len(r.Findings) != 0 {
		t.Fatalf("exit = %d, errors = %+v, findings = %+v", r.ExitCode, r.Errors, r.Findings)
	}
}

func TestNestedDedupeMergesLocations(t *testing.T) {
	files := nestedFiles(appDeps)
	files["package-lock.json"] = "{}"
	cfg, root := project(t, sourceTarget, files)
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh, lodashFinding("/package-lock.json")), testutil.SBOMJSON("pkg:npm/lodash@4.17.20"), nil},
		"dir:" + filepath.Join(root, "app"): {testutil.GrypeJSON(fresh, lodashFinding("/yarn.lock")), testutil.SBOMAt("/yarn.lock", "pkg:npm/lodash@4.17.20"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %+v, errors = %+v", r.Findings, r.Errors)
	}
	locs := r.Findings[0].Locations
	if !slices.Contains(locs, "/package-lock.json") || !slices.Contains(locs, "/app/yarn.lock") {
		t.Fatalf("locations = %v", locs)
	}
}

func TestNestedSubScanCountOverwritesRootCount(t *testing.T) {
	cfg, root := project(t, sourceTarget, nestedFiles(appDeps))
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root:                       {testutil.GrypeJSON(fresh), testutil.SBOMAt("/app/yarn.lock", "pkg:npm/lodash@4.17.20"), nil},
		"dir:" + filepath.Join(root, "app"): {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestUnderFolder(t *testing.T) {
	cases := []struct {
		exclude []string
		dir     string
		want    []string
	}{
		{[]string{"app/legacy/", "application/x", "app", "vendor"}, "app", []string{"legacy"}},
		{[]string{"a/b/c", "./a/b/d/", "/a/e"}, "a/b", []string{"c", "d"}},
		{[]string{"x"}, "app", nil},
		{nil, "app", nil},
	}
	for _, c := range cases {
		if got := underFolder(c.exclude, c.dir); !slices.Equal(got, c.want) {
			t.Errorf("underFolder(%v, %q) = %v, want %v", c.exclude, c.dir, got, c.want)
		}
	}
}

const (
	wsRoot  = `{"private":true,"workspaces":["packages/*"],"devDependencies":{"body-parser":"1.19.0"}}`
	wsNest  = `{"private":true,"workspaces":["packages/*"],"devDependencies":{"lodash":"4.17.20"}}`
	wsQS    = "pkg:npm/qs@6.7.0"
	wsLodsh = "pkg:npm/lodash@4.17.20"
)

func qsFinding(location string) testutil.Match {
	return testutil.Match{ID: "GHSA-qs", Package: "qs", Version: "6.7.0", FixState: "fixed", FixVersions: []string{"6.7.3"}, Location: location}
}

func TestYarnWorkspaceRootIsScannedAsFile(t *testing.T) {
	cfg, root := project(t, sourceTarget, map[string]string{"package.json": wsRoot, "yarn.lock": "# yarn", "packages/a/package.json": "{}"})
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"file:" + filepath.Join(root, "yarn.lock"): {testutil.GrypeJSON(fresh, qsFinding("/yarn.lock")), testutil.SBOMAt("/yarn.lock", wsQS, "pkg:npm/is-number@7.0.0"), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 1 || len(r.Errors) != 0 || len(r.Findings) != 1 {
		t.Fatalf("exit = %d, errors = %+v, findings = %+v", r.ExitCode, r.Errors, r.Findings)
	}
	if r.Targets[0].Packages != 2 { // root scan saw 1 package at yarn.lock, the file scan checked 2
		t.Fatalf("packages = %d, want 2", r.Targets[0].Packages)
	}
	if f := r.Findings[0]; f.Package != "qs" || !slices.Equal(f.Locations, []string{"/yarn.lock"}) {
		t.Fatalf("finding = %+v", f)
	}
	if len(r.Warnings) != 1 || r.Warnings[0].Code != "YARN_WORKSPACE_DEV_INCLUDED" || r.Warnings[0].Target != "repo" || !strings.HasPrefix(r.Warnings[0].Message, "yarn.lock is a yarn workspace root") {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
	for _, f := range []string{"raw/repo+workspace1.grype.json", "raw/repo+workspace1.cdx.json"} {
		if _, err := os.Stat(filepath.Join(r.EvidenceDir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestNestedYarnWorkspaceRootIsScannedAsFileNotDir(t *testing.T) {
	cfg, root := project(t, sourceTarget, map[string]string{"package.json": "{}", "nest/package.json": wsNest, "nest/yarn.lock": "# yarn", "nest/packages/a/package.json": "{}"})
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMAt("/nest/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"file:" + filepath.Join(root, "nest", "yarn.lock"): {testutil.GrypeJSON(fresh, lodashFinding("/yarn.lock")), testutil.SBOMAt("/yarn.lock", wsLodsh), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 1 || len(r.Errors) != 0 || len(r.Findings) != 1 {
		t.Fatalf("exit = %d, errors = %+v, findings = %+v", r.ExitCode, r.Errors, r.Findings)
	}
	if f := r.Findings[0]; f.Package != "lodash" || !slices.Equal(f.Locations, []string{"/nest/yarn.lock"}) {
		t.Fatalf("finding = %+v", f)
	}
	if codes(r.Warnings) != "YARN_WORKSPACE_DEV_INCLUDED" || !strings.HasPrefix(r.Warnings[0].Message, "nest/yarn.lock is a yarn workspace root") {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
	for _, c := range sc.calls {
		if strings.HasPrefix(c.Source, "dir:") && c.Source != "dir:"+root {
			t.Fatalf("unexpected sub-scan %s", c.Source)
		}
	}
}

func TestYarnWorkspaceFileScanFailureNamesTheLockfile(t *testing.T) {
	cfg, root := project(t, sourceTarget, map[string]string{"package.json": wsRoot, "yarn.lock": "# yarn"})
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"file:" + filepath.Join(root, "yarn.lock"): {err: errors.New("boom")},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || len(r.Errors) != 1 || r.Errors[0].Code != "SCAN_FAILED" || !strings.Contains(r.Errors[0].Message, "yarn.lock") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestYarnWorkspaceFileScanBadOutputIsAnError(t *testing.T) {
	cfg, root := project(t, sourceTarget, map[string]string{"package.json": wsRoot, "yarn.lock": "# yarn"})
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"file:" + filepath.Join(root, "yarn.lock"): {"not json", testutil.SBOMAt("/yarn.lock", wsQS), nil},
	}}
	r := run(t, cfg, sc, nil)
	if r.ExitCode != 2 || codes(r.Errors) != "BAD_OUTPUT" || !strings.Contains(r.Errors[0].Message, "yarn.lock") {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}

func TestYarnWorkspaceFileScanCountOverwritesRootCount(t *testing.T) {
	cfg, root := project(t, sourceTarget, map[string]string{"package.json": wsRoot, "yarn.lock": "# yarn"})
	sc := &fakeScanner{outputs: map[string]output{
		"dir:" + root: {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock", "pkg:npm/is-number@7.0.0"), nil},
		"file:" + filepath.Join(root, "yarn.lock"): {testutil.GrypeJSON(fresh), testutil.SBOMAt("/yarn.lock"), nil},
	}}
	if r := run(t, cfg, sc, nil); r.ExitCode != 2 || codes(r.Errors) != "LOCKFILE_UNREAD" {
		t.Fatalf("exit = %d, errors = %+v", r.ExitCode, r.Errors)
	}
}
