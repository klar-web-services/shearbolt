package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/testutil"
)

const (
	reviewLine  = "Review the changes with git status and git diff (new files show only in git status), then commit them. Setup never commits.\n"
	terminalMsg = "setup needs a terminal; use --yes, or --plan --json and --apply --answer"
)

// setupFixtures is absolute, because the tests change into temp directories.
var setupFixtures = func() string {
	dir, err := filepath.Abs("../../testdata/setup")
	if err != nil {
		panic(err)
	}
	return dir
}()

// setupDeps are deps for setup tests: stdin is the given text and the terminal check is fixed.
// Setup never needs Grype, so getting it fails.
func setupDeps(tty bool, stdin string) deps {
	d := defaultDeps()
	d.stdin = strings.NewReader(stdin)
	d.stdinTTY = func() bool { return tty }
	d.version = "1.2.3"
	d.ensureGrype = func(context.Context, string) (string, error) {
		return "", errors.New("setup tests must not install Grype")
	}
	return d
}

func setupCmd(t *testing.T, d deps, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"setup"}, args...), &out, &errb, d)
	return code, out.String(), errb.String()
}

// tree returns every regular file under dir by slash-separated path, with its mode.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = fmt.Sprintf("%v\n%s", info.Mode().Perm(), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := tree(t, dir)
	for p, w := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("%s was removed", p)
		} else if g != w {
			t.Errorf("%s changed:\n%s", p, g)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s was created", p)
		}
	}
}

// writeFiles writes files (slash paths) under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fixtureRepo copies testdata/setup/<name>/before into a temp dir and changes into it.
func fixtureRepo(t *testing.T, name string) string {
	t.Helper()
	dir := copyFixture(t, name)
	t.Chdir(dir)
	return dir
}

// copyFixture copies testdata/setup/<name>/before into a new temp dir, keeping executable bits.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	src := filepath.Join(setupFixtures, name, "before")
	n := 0
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		n++
		if err := os.WriteFile(dst, b, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chmod(dst, info.Mode().Perm()) // WriteFile's mode is subject to the umask
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("fixture %s is empty", src)
	}
	return dir
}

// tempRepo writes files into a temp dir and changes into it.
func tempRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, files)
	t.Chdir(dir)
	return dir
}

// twoProjects is a monorepo with two npm projects and a workflow that builds neither, so it
// asks projects, gate-job:apps/a and gate-job:apps/b.
func twoProjects() map[string]string {
	return map[string]string{
		"apps/a/package.json":      `{"name":"a"}`,
		"apps/a/package-lock.json": "{}",
		"apps/b/package.json":      `{"name":"b"}`,
		"apps/b/package-lock.json": "{}",
		".github/workflows/ci.yml": "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v5\n      - run: npm test\n",
	}
}

func decodePlan(t *testing.T, out string) plan.Plan {
	t.Helper()
	var p plan.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("stdout is not a plan: %v\n%s", err, out)
	}
	return p
}

func TestSetupPlanJSONWritesNothing(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	before := tree(t, dir)
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--plan", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	p := decodePlan(t, out)
	if !p.Complete || p.Error != nil || p.SchemaVersion != 1 || p.ShearboltVersion != "1.2.3" {
		t.Errorf("plan = complete %v, error %+v, schema %d, version %q", p.Complete, p.Error, p.SchemaVersion, p.ShearboltVersion)
	}
	var paths []string
	for _, c := range p.Changes {
		paths = append(paths, c.Path)
	}
	if want := []string{".dockerignore", ".github/workflows/release.yml", ".gitignore", "shearbolt.yaml"}; !slices.Equal(paths, want) {
		t.Errorf("changes = %q, want %q", paths, want)
	}
	if !strings.Contains(out, `>> \"$GITHUB_OUTPUT\"`) || strings.Contains(out, `\`+"u003e") {
		t.Errorf("the JSON escapes HTML characters:\n%s", out)
	}
	sameTree(t, dir, before)
}

func TestSetupNeedsTerminal(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	before := tree(t, dir)
	for _, args := range [][]string{nil, {"--answer", "projects=."}} {
		code, out, errb := setupCmd(t, setupDeps(false, "y\n"), args...)
		if code != 2 || !strings.Contains(errb, terminalMsg) || out != "" {
			t.Errorf("%q: exit = %d\nstdout: %s\nstderr: %s", args, code, out, errb)
		}
	}
	sameTree(t, dir, before)
}

func TestSetupInteractiveApplies(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	code, out, errb := setupCmd(t, setupDeps(true, "y\n"))
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	for _, want := range []string{
		"Found: shop (npm, Next.js) in .\n",
		"CI: GitHub Actions (.github/workflows/release.yml)\n",
		"--- /dev/null\n+++ b/shearbolt.yaml\n",
		"--- a/.github/workflows/release.yml\n+++ b/.github/workflows/release.yml\n",
		"Notes:\n- ",
		"Apply these changes? [y/N] ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, reviewLine) {
		t.Errorf("stdout does not end with the review line:\n%s", out)
	}
	got := tree(t, dir)
	for rel, want := range tree(t, filepath.Join(setupFixtures, "nextjs-docker", "after")) {
		if got[rel] != want {
			t.Errorf("%s differs from the fixture's after/:\n%s", rel, got[rel])
		}
	}
}

func TestSetupInteractiveDeclines(t *testing.T) {
	for _, stdin := range []string{"n\n", "\n", "maybe\n"} {
		t.Run(strings.TrimSpace(stdin), func(t *testing.T) {
			dir := fixtureRepo(t, "nextjs-docker")
			before := tree(t, dir)
			code, out, errb := setupCmd(t, setupDeps(true, stdin))
			if code != 0 || !strings.HasSuffix(out, "No changes written.\n") || strings.Contains(out, reviewLine) {
				t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
			sameTree(t, dir, before)
		})
	}
}

func TestSetupInteractiveEOFAtConfirmWritesNothing(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	before := tree(t, dir)
	code, out, errb := setupCmd(t, setupDeps(true, ""))
	if code != 2 || !strings.Contains(out, "No changes written.") || !strings.Contains(errb, "no answer") {
		t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	sameTree(t, dir, before)
}

func TestSetupInteractiveAsksOnlyApplicableQuestions(t *testing.T) {
	dir := tempRepo(t, twoProjects())
	// projects: option 1 (apps/a); gate-job:apps/a: Enter (default); gate-job:apps/b no longer applies.
	code, out, errb := setupCmd(t, setupDeps(true, "1\n\ny\n"))
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	for _, want := range []string{
		"Found: a (npm) in apps/a\n", "Found: b (npm) in apps/b\n",
		"Which projects should Shearbolt protect?", "  1) apps/a (npm)\n", "  2) apps/b (npm)\n",
		"apps/a. Which job",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "apps/b. Which job") {
		t.Errorf("asked gate-job:apps/b although apps/b is not selected:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps/a/shearbolt.yaml")); err != nil {
		t.Errorf("apps/a/shearbolt.yaml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps/b/shearbolt.yaml")); err == nil {
		t.Errorf("apps/b/shearbolt.yaml was written for an unselected project")
	}
}

func TestSetupInteractiveSkipsPreAnswered(t *testing.T) {
	dir := tempRepo(t, twoProjects())
	code, out, errb := setupCmd(t, setupDeps(true, "1\ny\n"), "--answer", "projects=apps/b")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if strings.Contains(out, "Which projects") || strings.Contains(out, "apps/a. Which job") || !strings.Contains(out, "apps/b. Which job") {
		t.Errorf("wrong questions asked:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps/b/shearbolt.yaml")); err != nil {
		t.Errorf("apps/b/shearbolt.yaml: %v", err)
	}
}

func TestSetupInteractiveAnswerByValue(t *testing.T) {
	dir := tempRepo(t, twoProjects())
	// projects by value and number; gate-job:apps/a by value; gate-job:apps/b by number.
	code, out, errb := setupCmd(t, setupDeps(true, "apps/b, 1\n.github/workflows/ci.yml#test\n1\ny\n"))
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	for _, f := range []string{"apps/a/shearbolt.yaml", "apps/b/shearbolt.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestSetupInteractiveInvalidAnswers(t *testing.T) {
	cases := map[string]string{
		"three invalid tries": "9\nnope\n0\n1\n\ny\n", // valid input after the third try is not read
		"EOF":                 "",
		"EOF after a try":     "9\n",
	}
	for name, stdin := range cases {
		t.Run(name, func(t *testing.T) {
			dir := tempRepo(t, twoProjects())
			before := tree(t, dir)
			code, out, errb := setupCmd(t, setupDeps(true, stdin))
			if code != 2 || !strings.Contains(errb, "projects") || strings.Contains(out, "apps/a. Which job") {
				t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
			if strings.Contains(out, "Apply these changes") {
				t.Errorf("asked to apply after an invalid answer:\n%s", out)
			}
			sameTree(t, dir, before)
		})
	}
}

func TestSetupInteractiveRetriesInvalidAnswer(t *testing.T) {
	tempRepo(t, twoProjects())
	code, out, errb := setupCmd(t, setupDeps(true, "9\napps/c\n1,2\n\n\nn\n"))
	if code != 0 || strings.Count(out, "Which projects should Shearbolt protect?") != 1 || !strings.Contains(out, "No changes written.") {
		t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if !strings.Contains(out, "apps/c") {
		t.Errorf("the invalid value is not explained:\n%s", out)
	}
}

func TestSetupApplyWithoutAnswers(t *testing.T) {
	dir := tempRepo(t, twoProjects())
	before := tree(t, dir)
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--apply")
	if code != 2 || !strings.Contains(errb, "projects") || !strings.Contains(errb, "gate-job:apps/a") || out != "" {
		t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	code, out, errb = setupCmd(t, setupDeps(false, ""), "--apply", "--json")
	p := decodePlan(t, out)
	if code != 2 || p.Error == nil || p.Error.Code != "UNANSWERED" || !strings.Contains(p.Error.Message, "projects") || len(p.Changes) != 0 || errb != "" {
		t.Errorf("--json: exit = %d, error = %+v, changes = %d, stderr = %s", code, p.Error, len(p.Changes), errb)
	}
	sameTree(t, dir, before)
}

func TestSetupApplyWithAnswers(t *testing.T) {
	dir := tempRepo(t, twoProjects())
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--apply", "--json",
		"--answer", "projects=apps/a", "--answer", "gate-job:apps/a=.github/workflows/ci.yml#test")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	p := decodePlan(t, out)
	if !p.Complete || p.Error != nil || len(p.Changes) == 0 {
		t.Errorf("plan = complete %v, error %+v, %d changes", p.Complete, p.Error, len(p.Changes))
	}
	for _, c := range p.Changes {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(c.Path)))
		if err != nil || string(b) != c.Content {
			t.Errorf("%s does not hold the planned content (%v)", c.Path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "apps/b/shearbolt.yaml")); err == nil {
		t.Errorf("apps/b/shearbolt.yaml was written for an unselected project")
	}
	if !strings.Contains(errb, strings.TrimSuffix(reviewLine, "\n")) {
		t.Errorf("stderr lacks the review line: %s", errb)
	}
}

func TestSetupApplyYesUsesDefaults(t *testing.T) {
	dir := tempRepo(t, twoProjects())
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--apply", "--yes")
	if code != 0 || !strings.HasSuffix(out, reviewLine) {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	for _, f := range []string{"apps/a/shearbolt.yaml", "apps/b/shearbolt.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestSetupUsageErrors(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	before := tree(t, dir)
	cases := [][]string{
		{"--plan", "--apply"},
		{"--plan", "--apply", "--json"},
		{"--json"},
		{"--yes", "--json"},
		{"extra"},
		{"--frobnicate"},
	}
	for _, args := range cases {
		code, out, errb := setupCmd(t, setupDeps(true, "y\n"), args...)
		if code != 2 || errb == "" || out != "" {
			t.Errorf("%q: exit = %d\nstdout: %s\nstderr: %s", args, code, out, errb)
		}
	}
	sameTree(t, dir, before)
}

func TestSetupHelp(t *testing.T) {
	code, _, errb := setupCmd(t, setupDeps(false, ""), "-h")
	if code != 0 || !strings.Contains(errb, "Usage: shearbolt setup") || !strings.Contains(errb, "-answer") {
		t.Errorf("exit = %d, stderr = %s", code, errb)
	}
}

func TestSetupBadAnswer(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	before := tree(t, dir)
	for _, a := range []string{"nope=1", "projects", "=x"} {
		code, out, _ := setupCmd(t, setupDeps(false, ""), "--plan", "--json", "--answer", a)
		p := decodePlan(t, out)
		if code != 2 || p.Error == nil || p.Error.Code != "BAD_ANSWER" {
			t.Errorf("%q: exit = %d, error = %+v", a, code, p.Error)
		}
		code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes", "--answer", a)
		if code != 2 || out != "" || !strings.Contains(errb, "shearbolt setup: ") {
			t.Errorf("%q human: exit = %d\nstdout: %s\nstderr: %s", a, code, out, errb)
		}
	}
	code, out, _ := setupCmd(t, setupDeps(false, ""), "--plan", "--json", "--answer", "a=1", "--answer", "a=2")
	if p := decodePlan(t, out); code != 2 || p.Error == nil || p.Error.Code != "BAD_ANSWER" {
		t.Errorf("duplicate: exit = %d, error = %+v", code, p.Error)
	}
	sameTree(t, dir, before)
}

func TestSetupNoCI(t *testing.T) {
	tempRepo(t, map[string]string{"package.json": `{"name":"x"}`, "package-lock.json": "{}"})
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--plan", "--json")
	p := decodePlan(t, out)
	if code != 2 || p.Error == nil || p.Error.Code != "NO_CI" || len(p.Projects) != 1 || errb != "" {
		t.Errorf("exit = %d, error = %+v, stderr = %s", code, p.Error, errb)
	}
	code, out, errb = setupCmd(t, setupDeps(false, ""), "--yes")
	if code != 2 || out != "" || !strings.Contains(errb, "Shearbolt is a CI gate") || !strings.Contains(errb, "shearbolt scan --source .") {
		t.Errorf("human: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

func TestSetupSecondRunNothingToChange(t *testing.T) {
	fixtureRepo(t, "nextjs-docker")
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes")
	if code != 0 || !strings.Contains(out, "+++ b/shearbolt.yaml") || !strings.HasSuffix(out, reviewLine) || strings.Contains(out, "nothing to change") {
		t.Fatalf("first run: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	code, out, errb = setupCmd(t, setupDeps(false, ""), "--yes")
	if code != 0 || !strings.HasSuffix(out, "Shearbolt is already set up here; nothing to change.\n"+reviewLine) || strings.Contains(out, "+++ ") {
		t.Errorf("second run: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	code, out, errb = setupCmd(t, setupDeps(true, ""))
	if code != 0 || !strings.HasSuffix(out, "Shearbolt is already set up here; nothing to change.\n"+reviewLine) || strings.Contains(out, "Apply these changes") {
		t.Errorf("interactive second run: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

func TestSetupPlanHumanWritesNothing(t *testing.T) {
	dir := tempRepo(t, twoProjects())
	before := tree(t, dir)
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--plan", "--answer", "projects=apps/a")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	for _, want := range []string{
		"Found: a (npm) in apps/a\n", "CI: GitHub Actions (.github/workflows/ci.yml)\n",
		"Questions:\n", "projects (answered): apps/a\n", "gate-job:apps/a (default): .github/workflows/ci.yml#test\n",
		"+++ b/apps/a/shearbolt.yaml", "No changes written (--plan).\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gate-job:apps/b") || strings.Contains(out, reviewLine) {
		t.Errorf("stdout lists an inapplicable question or the review line:\n%s", out)
	}
	sameTree(t, dir, before)
}

// TestSetupPrintsUnsupportedEditInFull checks that a multi-line UNSUPPORTED_EDIT, which carries
// the job, the reason and the snippet to add by hand, is printed verbatim.
func TestSetupPrintsUnsupportedEditInFull(t *testing.T) {
	dir := tempRepo(t, map[string]string{
		"package.json":      `{"name":"web"}`,
		"package-lock.json": "{}",
		"Dockerfile":        "FROM node:22\n",
		".github/workflows/ci.yml": "name: ci\non: push\njobs:\n  docker:\n    runs-on: ubuntu-latest\n    steps:\n" +
			"      - uses: actions/checkout@v5\n      - run: docker build -t web:1 . | tee build.log\n",
	})
	p := setup.Compute(setup.Options{Root: dir, Yes: true, Version: "1.2.3"})
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
	if i < 0 || !strings.Contains(p.Notes[i].Message, "\n") {
		t.Fatalf("fixture gives no multi-line UNSUPPORTED_EDIT: %+v", p.Notes)
	}
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if !strings.Contains(out, p.Notes[i].Message+"\n") {
		t.Errorf("stdout lacks the full note:\n%s\nstdout:\n%s", p.Notes[i].Message, out)
	}
	// A re-run has nothing to change, but must not claim that Shearbolt is set up.
	code, out, errb = setupCmd(t, setupDeps(false, ""), "--yes")
	want := "Setup has nothing more to change automatically; add what the notes above describe by hand.\n" + reviewLine
	if code != 0 || !strings.HasSuffix(out, want) || strings.Contains(out, "already set up") || !strings.Contains(out, p.Notes[i].Message) {
		t.Errorf("second run: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

func TestNoteLine(t *testing.T) {
	cases := map[plan.Note]string{
		{Code: "X", Message: "plain"}:                                                   "- plain",
		{Code: "X", Message: "msg", File: "a.yml"}:                                      "- a.yml: msg",
		{Code: "X", Message: "msg", File: "a.yml", Line: 3}:                             "- a.yml:3: msg",
		{Code: "X", Message: "a.yml: job x was left unchanged", File: "a.yml", Line: 3}: "- a.yml: job x was left unchanged",
	}
	for n, want := range cases {
		if got := noteLine(n); got != want {
			t.Errorf("noteLine(%+v) = %q, want %q", n, got, want)
		}
	}
}

func TestSetupInteractiveLastLineWithoutNewline(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	code, out, errb := setupCmd(t, setupDeps(true, "y"))
	if code != 0 || !strings.HasSuffix(out, reviewLine) {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, "shearbolt.yaml")); err != nil {
		t.Errorf("shearbolt.yaml: %v", err)
	}
}

// TestSetupNumbersWinOverValues: the options are numbered, so a typed option number selects
// that option even when another option's value is the same text.
func TestSetupNumbersWinOverValues(t *testing.T) {
	dir := tempRepo(t, map[string]string{
		"2/package.json": `{"name":"two"}`, "2/package-lock.json": "{}",
		"a/package.json": `{"name":"a"}`, "a/package-lock.json": "{}",
		".github/workflows/ci.yml": "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v5\n      - run: npm test\n",
	})
	code, out, errb := setupCmd(t, setupDeps(true, "2\n\ny\n"))
	if code != 0 || !strings.Contains(out, "  1) 2 (npm)\n  2) a (npm)\n") {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, "a/shearbolt.yaml")); err != nil {
		t.Errorf("option 2 (a) was not selected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2/shearbolt.yaml")); err == nil {
		t.Errorf("the value 2 was selected instead of option 2")
	}
}

// zeros is an endless stdin without newlines, like /dev/zero.
type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func TestSetupLongAnswers(t *testing.T) {
	t.Run("long line is an invalid try", func(t *testing.T) {
		dir := tempRepo(t, twoProjects())
		code, out, errb := setupCmd(t, setupDeps(true, strings.Repeat("1", 5000)+"\n1\n\nn\n"))
		if code != 0 || !strings.Contains(out, "longer than 4 KiB") || !strings.Contains(out, "No changes written.") {
			t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if _, err := os.Stat(filepath.Join(dir, "apps/a/shearbolt.yaml")); err == nil {
			t.Errorf("a config was written after declining")
		}
	})
	t.Run("endless question input", func(t *testing.T) {
		dir := tempRepo(t, twoProjects())
		before := tree(t, dir)
		d := setupDeps(true, "")
		d.stdin = zeros{}
		code, out, errb := setupCmd(t, d)
		if code != 2 || !strings.Contains(errb, "projects") || len(out) > 64<<10 {
			t.Errorf("exit = %d, %d bytes of stdout\nstderr: %s", code, len(out), errb)
		}
		sameTree(t, dir, before)
	})
	t.Run("endless confirmation input", func(t *testing.T) {
		dir := fixtureRepo(t, "nextjs-docker")
		before := tree(t, dir)
		d := setupDeps(true, "")
		d.stdin = zeros{}
		code, out, errb := setupCmd(t, d)
		if code != 0 || !strings.HasSuffix(out, "No changes written.\n") {
			t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		sameTree(t, dir, before)
	})
}

func TestSetupWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for _, mode := range []string{"human", "json"} {
		t.Run(mode, func(t *testing.T) {
			dir := fixtureRepo(t, "nextjs-docker")
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o755) })
			if f, err := os.CreateTemp(dir, "probe"); err == nil {
				f.Close()
				t.Skip("this file system ignores directory permissions")
			}
			before := tree(t, dir)
			args := []string{"--yes"}
			if mode == "json" {
				args = []string{"--apply", "--yes", "--json"}
			}
			code, out, errb := setupCmd(t, setupDeps(false, ""), args...)
			if code != 2 {
				t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
			if mode == "json" {
				p := decodePlan(t, out)
				if p.Error == nil || p.Error.Code != "WRITE" || !strings.Contains(p.Error.Message, "check git status") || len(p.Changes) != 0 {
					t.Errorf("error = %+v, %d changes", p.Error, len(p.Changes))
				}
			} else if out != "" || !strings.Contains(errb, "check git status") {
				t.Errorf("stdout: %s\nstderr: %s", out, errb)
			}
			sameTree(t, dir, before)
		})
	}
}

// failWriter is a stdout that cannot be written, such as a closed pipe.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestSetupJSONOutputFailureExitsTwo(t *testing.T) {
	fixtureRepo(t, "nextjs-docker")
	for _, args := range [][]string{{"--plan", "--json"}, {"--plan", "--json", "--answer", "nope=1"}} {
		var errb bytes.Buffer
		if code := run(append([]string{"setup"}, args...), failWriter{}, &errb, setupDeps(false, "")); code != 2 || !strings.Contains(errb.String(), "broken pipe") {
			t.Errorf("%q: exit = %d, stderr = %s", args, code, errb.String())
		}
	}
}

// escRepo has an escape sequence in its package name, its workflow file name and its job key.
func escRepo(t *testing.T) string {
	t.Helper()
	return tempRepo(t, map[string]string{
		"package.json":      "{\"name\":\"\\u001b[31mevil\"}",
		"package-lock.json": "{}",
		".github/workflows/e\x1b[2Jvil.yml": "name: ci\non: push\njobs:\n  \"bad\\e[31mjob\":\n    runs-on: ubuntu-latest\n    steps:\n" +
			"      - uses: actions/checkout@v5\n      - run: npm test\n",
	})
}

func TestSetupEscapesRepositoryText(t *testing.T) {
	check := func(t *testing.T, code int, out, errb string, wantCode int) {
		t.Helper()
		if code != wantCode {
			t.Errorf("exit = %d, want %d\nstdout: %q\nstderr: %q", code, wantCode, out, errb)
		}
		if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(errb, 0x1b) {
			t.Errorf("raw ESC reached the terminal\nstdout: %q\nstderr: %q", out, errb)
		}
	}
	visible := []string{`\x1b[31mevil`, `e\x1b[2Jvil.yml`, `bad\x1b[31mjob`}
	t.Run("plan", func(t *testing.T) {
		escRepo(t)
		code, out, errb := setupCmd(t, setupDeps(false, ""), "--plan")
		check(t, code, out, errb, 0)
		for _, v := range visible {
			if !strings.Contains(out, v) {
				t.Errorf("stdout lacks %s:\n%s", v, out)
			}
		}
	})
	t.Run("interactive", func(t *testing.T) {
		escRepo(t)
		code, out, errb := setupCmd(t, setupDeps(true, "\ny\n"))
		check(t, code, out, errb, 0)
		if !strings.Contains(out, `e\x1b[2Jvil.yml: bad\x1b[31mjob`) {
			t.Errorf("the job option is not shown escaped:\n%s", out)
		}
	})
	t.Run("apply", func(t *testing.T) {
		escRepo(t)
		code, out, errb := setupCmd(t, setupDeps(false, ""), "--apply", "--yes")
		check(t, code, out, errb, 0)
	})
	t.Run("error", func(t *testing.T) {
		escRepo(t)
		code, out, errb := setupCmd(t, setupDeps(false, ""), "--plan", "--answer", "gate-job:.=x")
		check(t, code, out, errb, 2)
		if !strings.Contains(errb, `bad\x1b[31mjob`) {
			t.Errorf("stderr does not name the job:\n%s", errb)
		}
	})
	t.Run("invalid interactive answer", func(t *testing.T) {
		escRepo(t)
		code, out, errb := setupCmd(t, setupDeps(true, "x\x1b[1m\n\nn\n"))
		check(t, code, out, errb, 0)
	})
}

func TestTermSafe(t *testing.T) {
	cases := []struct {
		in        string
		multiline bool
		want      string
	}{
		{"plain text", false, "plain text"},
		{"a\x1b[31mb", false, `a\x1b[31mb`},
		{"a\nb\tc\r\n", false, `a\nb\tc\r\n`},
		{"a\nb\tc\r\nd\re", true, "a\nb\tc\r\nd\\re"},
		{"del\x7f c1\u0085 \u009b", true, `del\x7f c1\u0085 \u009b`},
		{"bad\x9bbyte", false, `bad\x9bbyte`},
		{"ünïcode ✓", false, "ünïcode ✓"},
	}
	for _, c := range cases {
		if got := termSafe(c.in, c.multiline); got != c.want {
			t.Errorf("termSafe(%q, %v) = %q, want %q", c.in, c.multiline, got, c.want)
		}
	}
}

var updatePlanJSON = flag.Bool("update", false, "rewrite testdata/setup/monorepo/plan.json")

// TestSetupPlanJSONGolden pins the JSON contract the agent skill reads (Task 13): the exact
// `shearbolt setup --plan --json` output on the monorepo fixture, with defaults for the open
// questions. Changing it requires bumping schemaVersion.
func TestSetupPlanJSONGolden(t *testing.T) {
	golden := filepath.Join(setupFixtures, "monorepo", "plan.json")
	fixtureRepo(t, "monorepo")
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--plan", "--json")
	if code != 0 || errb != "" {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	if *updatePlanJSON {
		if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("--plan --json differs from %s:\n%s", golden, plan.UnifiedDiff("plan.json", want, []byte(out)))
	}
	p := decodePlan(t, out)
	if p.Complete || len(p.Questions) != 2 || p.Questions[1].ID != "gate-job:apps/api" || p.Questions[1].Project != "apps/api" {
		t.Errorf("plan: complete %v, questions %+v", p.Complete, p.Questions)
	}
	// The plan's contents are the golden after/ files.
	for _, c := range p.Changes {
		want, err := os.ReadFile(filepath.Join(setupFixtures, "monorepo", "after", filepath.FromSlash(c.Path)))
		if err != nil || c.Content != string(want) {
			t.Errorf("%s: plan content differs from after/ (%v)", c.Path, err)
		}
	}
}

// A re-run without changes does not claim Shearbolt is set up while a note says something runs
// unchecked: PUSH_BEFORE_GATE, BUILD_AFTER_GATE or GATE_AFTER_PUSH (Batch 5 review).
func TestSetupNoAlreadySetUpWithOrderNotes(t *testing.T) {
	tempRepo(t, map[string]string{
		"package.json":      `{"name":"web"}`,
		"package-lock.json": "{}",
		"Dockerfile":        "FROM node:22\n",
		".github/workflows/ci.yml": "name: ci\non: push\njobs:\n  docker:\n    runs-on: ubuntu-latest\n    steps:\n" +
			"      - uses: actions/checkout@v5\n      - run: docker push web:previous\n      - run: docker build -t web:1 .\n",
	})
	if code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes"); code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes")
	want := "Setup has nothing more to change automatically; add what the notes above describe by hand.\n" + reviewLine
	if code != 0 || !strings.HasSuffix(out, want) || !strings.Contains(out, "web:previous") {
		t.Errorf("second run: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

// nothingToChange claims Shearbolt is set up only when no note says something is left by hand or
// runs unchecked (Batch 5 review and re-review).
func TestNothingToChangeCodes(t *testing.T) {
	const notSetUp = "Setup has nothing more to change automatically; add what the notes above describe by hand."
	for _, code := range []string{"UNSUPPORTED_EDIT", "NO_GATE_PLACED", "BUILD_AFTER_GATE", "PUSH_BEFORE_GATE", "GATE_AFTER_PUSH"} {
		if got := nothingToChange(&plan.Plan{Notes: []plan.Note{{Code: "TAG_ADDED"}, {Code: code}}}); got != notSetUp {
			t.Errorf("%s: %q", code, got)
		}
	}
	if got := nothingToChange(&plan.Plan{Notes: []plan.Note{{Code: "TAG_ADDED"}, {Code: "NO_BUILD_FOUND"}}}); got != "Shearbolt is already set up here; nothing to change." {
		t.Errorf("informational notes: %q", got)
	}
	for _, code := range []string{"COMPOSE_ADDED", "COMPOSE_NOT_GATED", "COMPOSE_NO_IMAGES", "COMPOSE_NOT_READ", "COMPOSE_VARIABLE", "BUILD_NOT_GATED", "NO_DEPLOY_SEEN"} {
		if got := nothingToChange(&plan.Plan{Notes: []plan.Note{{Code: "TAG_ADDED"}, {Code: code}}}); got != "Shearbolt is already set up here; nothing to change." {
			t.Errorf("%s: %q", code, got)
		}
	}
}

// typedNumbers is value v of q typed as option numbers, the way a user answers on the terminal.
func typedNumbers(t *testing.T, q plan.Question, v string) string {
	t.Helper()
	parts := []string{v}
	if q.Kind == "multi" {
		parts = strings.Split(v, ",")
	}
	nums := make([]string, len(parts))
	for i, part := range parts {
		n := slices.IndexFunc(q.Options, func(o plan.Option) bool { return o.Value == part })
		if n < 0 {
			t.Fatalf("%s: %q is not an option", q.ID, part)
		}
		nums[i] = fmt.Sprint(n + 1)
	}
	return strings.Join(nums, ",")
}

// interactiveAnswers follows interactive mode's loop: it asks the first applicable unanswered
// question, re-resolving after each answer. Each answer is the fixture's (given), else the
// default. It returns the answers in the order asked and the final answer set.
func interactiveAnswers(t *testing.T, qs []plan.Question, given map[string]string) ([]plan.Question, map[string]string) {
	t.Helper()
	var asked []plan.Question
	answers := map[string]string{}
	for {
		_, missing, err := plan.Resolve(qs, answers, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) == 0 {
			return asked, answers
		}
		i := slices.IndexFunc(qs, func(q plan.Question) bool { return q.ID == missing[0] })
		v, ok := given[qs[i].ID]
		if !ok {
			v = qs[i].Default
		}
		asked = append(asked, qs[i])
		answers[qs[i].ID] = v
	}
}

// sameTrees compares two tree() results.
func sameTrees(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for p, w := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("%s: %s is missing", what, p)
		} else if g != w {
			t.Errorf("%s: %s differs:\n%s", what, p, plan.UnifiedDiff(p, []byte(w), []byte(g)))
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s: %s should not exist", what, p)
		}
	}
}

// setupIn copies the fixture, runs setup in the copy and returns the resulting tree. The run
// must succeed and end with the review line.
func setupIn(t *testing.T, fixture string, d deps, args ...string) (map[string]string, string) {
	t.Helper()
	dir := copyFixture(t, fixture)
	t.Chdir(dir)
	code, out, errb := setupCmd(t, d, args...)
	if code != 0 || !strings.HasSuffix(out, reviewLine) {
		t.Fatalf("setup %q: exit = %d\nstdout: %s\nstderr: %s", args, code, out, errb)
	}
	return tree(t, dir), out
}

// TestSetupModesAgree is AC-5 (Task 18). On every testdata/setup fixture, with the fixture's
// answers: --plan --json writes nothing; --apply with every applicable question answered (the
// way an agent answers after --plan --json), interactive mode (Enter for defaults, or every
// answer typed as option numbers) and, for a defaults-only fixture, --apply --yes write
// byte-identical trees; and each changed file holds the plan's content.
func TestSetupModesAgree(t *testing.T) {
	befores, err := filepath.Glob(filepath.Join(setupFixtures, "*", "before"))
	if err != nil || len(befores) == 0 {
		t.Fatalf("no fixtures under %s (%v)", setupFixtures, err)
	}
	for _, before := range befores {
		name := filepath.Base(filepath.Dir(before))
		t.Run(name, func(t *testing.T) {
			given := fixtureAnswers(t, name)
			var answerArgs []string
			for _, id := range slices.Sorted(maps.Keys(given)) {
				answerArgs = append(answerArgs, "--answer", id+"="+given[id])
			}

			// A: --plan --json changes no file.
			dirA := copyFixture(t, name)
			orig := tree(t, dirA)
			t.Chdir(dirA)
			code, out, errb := setupCmd(t, setupDeps(false, ""), append([]string{"--plan", "--json"}, answerArgs...)...)
			if code != 0 {
				t.Fatalf("--plan --json: exit = %d, stderr = %s", code, errb)
			}
			p := decodePlan(t, out)
			if p.Error != nil || len(p.Changes) == 0 {
				t.Fatalf("plan: error %+v, %d changes", p.Error, len(p.Changes))
			}
			sameTree(t, dirA, orig)
			asked, all := interactiveAnswers(t, p.Questions, given)

			// B: --apply, every applicable question answered.
			applyArgs := []string{"--apply"}
			for _, id := range slices.Sorted(maps.Keys(all)) {
				applyArgs = append(applyArgs, "--answer", id+"="+all[id])
			}
			for id, v := range given {
				if _, ok := all[id]; !ok {
					applyArgs = append(applyArgs, "--answer", id+"="+v)
				}
			}
			want, _ := setupIn(t, name, setupDeps(false, ""), applyArgs...)
			changed := map[string]bool{}
			for _, c := range p.Changes {
				changed[c.Path] = true
				_, content, _ := strings.Cut(want[c.Path], "\n")
				if content != c.Content {
					t.Errorf("--apply: %s differs from the plan's content:\n%s", c.Path, plan.UnifiedDiff(c.Path, []byte(c.Content), []byte(content)))
				}
			}
			for path, v := range orig {
				if !changed[path] && want[path] != v {
					t.Errorf("--apply changed %s, which the plan does not list", path)
				}
			}
			for path := range want {
				if _, ok := orig[path]; !ok && !changed[path] {
					t.Errorf("--apply created %s, which the plan does not list", path)
				}
			}

			// C: interactive, each asked question answered with its option numbers, or with
			// Enter for the default; then y.
			var enter, numbers strings.Builder
			for _, q := range asked {
				v := all[q.ID]
				if _, ok := given[q.ID]; ok {
					enter.WriteString(typedNumbers(t, q, v))
				}
				enter.WriteString("\n")
				numbers.WriteString(typedNumbers(t, q, v) + "\n")
			}
			for variant, stdin := range map[string]string{"Enter for defaults": enter.String(), "option numbers": numbers.String()} {
				got, out := setupIn(t, name, setupDeps(true, stdin+"y\n"))
				if n := strings.Count(out, "Choose one"); n != len(asked) {
					t.Errorf("interactive (%s): %d questions asked, want %d:\n%s", variant, n, len(asked), out)
				}
				if n := strings.Count(out, "Apply these changes? [y/N]"); n != 1 {
					t.Errorf("interactive (%s): asked to apply %d times", variant, n)
				}
				sameTrees(t, "interactive ("+variant+") vs --apply", got, want)
			}

			// D: --apply --yes takes the same defaults.
			if len(given) == 0 {
				got, _ := setupIn(t, name, setupDeps(false, ""), "--apply", "--yes")
				sameTrees(t, "--apply --yes vs --apply", got, want)
			}
		})
	}
}

// The monorepo has open questions, so --apply without answers is an error naming them, and
// writes nothing (Task 18).
func TestSetupApplyWithoutAnswersOnMonorepo(t *testing.T) {
	dir := fixtureRepo(t, "monorepo")
	before := tree(t, dir)
	code, out, errb := setupCmd(t, setupDeps(false, ""), "--apply")
	if code != 2 || out != "" || !strings.Contains(errb, "no answer: projects, gate-job:apps/api.") {
		t.Errorf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	sameTree(t, dir, before)
}

// The config setup generates scans (Task 19): right after setup --yes, a scan with a fake Grype
// loads it, resolves its target names and finds the lockfile, without errors. The fake Grype
// "pulls" any image, so the Next.js image target is scanned; the Rust binary is not built, so
// its target is skipped (local mode).
func TestSetupConfigScans(t *testing.T) {
	cases := []struct {
		fixture, sbom, artifact, artifactStatus string
	}{
		{"nextjs-build-push", testutil.SBOMAt("/package-lock.json", "pkg:npm/next@15.0.0"), "storefront", "scanned"},
		{"rust-make", testutil.SBOMAt("/Cargo.lock", "pkg:cargo/smallvec@1.6.0"), "demo", "skipped"},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			fixtureRepo(t, c.fixture)
			d := testDeps(t, map[string]string{"FAKE_GRYPE_SBOM": c.sbom})
			if code, out, errb := setupCmd(t, d, "--yes"); code != 0 {
				t.Fatalf("setup: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
			code, out, errb := scanCmd(t, d, "--json", "--output-dir", evidence(t))
			var r model.Run
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				t.Fatalf("stdout is not run.json (exit %d): %v\nstderr: %s", code, err, errb)
			}
			if code != 0 || len(r.Errors) > 0 {
				t.Fatalf("scan: exit = %d, errors = %+v\nstderr: %s", code, r.Errors, errb)
			}
			status := map[string]string{}
			packages := map[string]int{}
			for _, tr := range r.Targets {
				status[tr.Name], packages[tr.Name] = tr.Status, tr.Packages
			}
			if len(r.Targets) != 2 || status[c.artifact] != c.artifactStatus || status["source"] != "scanned" || packages["source"] == 0 {
				t.Errorf("targets = %+v, want %s %s and source scanned with packages", r.Targets, c.artifact, c.artifactStatus)
			}
		})
	}
}
