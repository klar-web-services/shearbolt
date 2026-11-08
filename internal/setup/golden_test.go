package setup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

var update = flag.Bool("update", false, "rewrite testdata/setup/*/after and expect.json from the results")

const goldenDir = "../../testdata/setup"

// expectation is a fixture's expect.json.
type expectation struct {
	Questions []string `json:"questions"`
	Notes     []string `json:"notes"`
}

// treeFile is a file of a fixture tree: content and whether it is executable.
type treeFile struct {
	content string
	exec    bool
}

// TestGolden runs setup on each testdata/setup/<fixture>/before and compares the result with
// after/ (the files setup creates or modifies) and expect.json (question IDs in order, note codes
// as a sorted set). It also checks that a second run changes nothing and that every
// shearbolt.yaml loads with an empty environment. With -update it rewrites after/ and expect.json.
func TestGolden(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(goldenDir, "*", "before"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures under %s (%v)", goldenDir, err)
	}
	for _, before := range dirs {
		fixture := filepath.Dir(before)
		t.Run(filepath.Base(fixture), func(t *testing.T) { golden(t, fixture) })
	}
}

func golden(t *testing.T, fixture string) {
	before := filepath.Join(fixture, "before")
	afterDir := filepath.Join(fixture, "after")
	root := t.TempDir()
	copyTree(t, before, root)
	answers := readAnswers(t, filepath.Join(fixture, "answers.txt"))

	p := Compute(Options{Root: root, Answers: answers, Yes: true, Version: "1.2.3"})
	if p.Error != nil {
		t.Fatalf("plan error: %+v", p.Error)
	}
	if err := Write(root, p); err != nil {
		t.Fatal(err)
	}
	got := readTree(t, root)
	orig := readTree(t, before)
	exp := expectation{Questions: []string{}, Notes: []string{}}
	for _, q := range p.Questions {
		exp.Questions = append(exp.Questions, q.ID)
	}
	for _, n := range p.Notes {
		exp.Notes = append(exp.Notes, n.Code)
	}
	slices.Sort(exp.Notes)
	exp.Notes = slices.Compact(exp.Notes)

	if *update {
		writeAfter(t, afterDir, got, orig)
		b, _ := json.MarshalIndent(exp, "", "  ")
		if err := os.WriteFile(filepath.Join(fixture, "expect.json"), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	after := readTree(t, afterDir)
	for path, want := range after {
		g, ok := got[path]
		switch {
		case !ok:
			t.Errorf("%s: missing from the result", path)
		case g.content != want.content:
			t.Errorf("%s differs from after/:\n%s", path, plan.UnifiedDiff(path, []byte(want.content), []byte(g.content)))
		case g.exec != want.exec:
			t.Errorf("%s: executable %v, want %v", path, g.exec, want.exec)
		}
	}
	for path, g := range got {
		if _, ok := after[path]; ok {
			continue
		}
		o, ok := orig[path]
		switch {
		case !ok:
			t.Errorf("%s: unexpected file in the result:\n%s", path, g.content)
		case o != g:
			t.Errorf("%s changed but is not in after/:\n%s", path, plan.UnifiedDiff(path, []byte(o.content), []byte(g.content)))
		}
	}

	var want expectation
	b, err := os.ReadFile(filepath.Join(fixture, "expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatalf("expect.json: %v", err)
	}
	if !slices.Equal(exp.Questions, orEmpty(want.Questions)) {
		t.Errorf("questions = %q, want %q", exp.Questions, want.Questions)
	}
	if !slices.Equal(exp.Notes, orEmpty(want.Notes)) {
		t.Errorf("note codes = %q, want %q\n%s", exp.Notes, want.Notes, noteDump(p.Notes))
	}

	again := Compute(Options{Root: root, Answers: answers, Yes: true, Version: "1.2.3"})
	if again.Error != nil {
		t.Fatalf("second plan error: %+v", again.Error)
	}
	for _, c := range again.Changes {
		t.Errorf("a second run changes %s:\n%s", c.Path, c.Diff)
	}

	for path, f := range got {
		if filepath.Base(path) != "shearbolt.yaml" {
			continue
		}
		file := filepath.Join(root, filepath.FromSlash(path))
		if _, err := config.Load(file, func(string) (string, bool) { return "", false }, time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Errorf("%s does not load: %v\n%s", path, err, f.content)
		}
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func noteDump(notes []plan.Note) string {
	var b strings.Builder
	for _, n := range notes {
		b.WriteString("  " + n.Code + ": " + n.Message + "\n")
	}
	return b.String()
}

// readAnswers reads answers.txt: one id=value per line; blank lines and # comments are ignored.
func readAnswers(t *testing.T, file string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			args = append(args, l)
		}
	}
	answers, err := plan.ParseAnswers(args)
	if err != nil {
		t.Fatal(err)
	}
	return answers
}

// readTree returns every regular file under dir by slash-separated relative path; a missing
// dir is an empty tree.
func readTree(t *testing.T, dir string) map[string]treeFile {
	t.Helper()
	out := map[string]treeFile{}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return out
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = treeFile{content: string(b), exec: info.Mode().Perm()&0o111 != 0}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	for rel, f := range readTree(t, from) {
		writeTreeFile(t, filepath.Join(to, filepath.FromSlash(rel)), f)
	}
}

func writeTreeFile(t *testing.T, p string, f treeFile) {
	t.Helper()
	mode := os.FileMode(0o644)
	if f.exec {
		mode = 0o755
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(f.content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// writeAfter makes afterDir hold exactly the result files that differ from before (-update),
// rewriting only the files whose content or mode changed.
func writeAfter(t *testing.T, afterDir string, got, orig map[string]treeFile) {
	t.Helper()
	old := readTree(t, afterDir)
	for path, g := range got {
		if o, ok := orig[path]; ok && o == g {
			continue
		}
		if a, ok := old[path]; ok && a == g {
			continue
		}
		writeTreeFile(t, filepath.Join(afterDir, filepath.FromSlash(path)), g)
	}
	for path := range old {
		if g, ok := got[path]; ok && orig[path] != g {
			continue
		}
		if err := os.Remove(filepath.Join(afterDir, filepath.FromSlash(path))); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMonorepoAnswers is the monorepo with a second answer set (Task 13): deselecting apps/api
// gives it no config and no gate, its gate-job question is still listed (with its project), and
// an answer to it is ignored.
func TestMonorepoAnswers(t *testing.T) {
	root := t.TempDir()
	copyTree(t, filepath.Join(goldenDir, "monorepo", "before"), root)
	answers := map[string]string{"projects": "apps/web,crates/cli", "gate-job:apps/api": ".github/workflows/ci.yml#web"}
	p := Compute(Options{Root: root, Answers: answers, Version: "1.2.3"})
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if !p.Complete {
		t.Error("every applicable question is answered")
	}
	i := slices.IndexFunc(p.Questions, func(q plan.Question) bool { return q.ID == "gate-job:apps/api" })
	if i < 0 || p.Questions[i].Project != "apps/api" || p.Questions[i].Answer != ".github/workflows/ci.yml#web" {
		t.Fatalf("questions: %+v", p.Questions)
	}
	var paths []string
	for _, c := range p.Changes {
		paths = append(paths, c.Path)
	}
	want := []string{".github/workflows/ci.yml", ".gitignore", "apps/web/.dockerignore", "apps/web/shearbolt.yaml", "crates/cli/shearbolt.yaml"}
	if !slices.Equal(paths, want) {
		t.Fatalf("changes = %q, want %q", paths, want)
	}
	after, err := os.ReadFile(filepath.Join(goldenDir, "monorepo", "after", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	apiGate := "      - name: shearbolt\n        uses: klar-web-services/shearbolt@v1\n        with:\n          working-directory: apps/api\n"
	wantCI := strings.Replace(string(after), apiGate, "", 1)
	if wantCI == string(after) {
		t.Fatal("the golden has no apps/api gate to remove")
	}
	if got := p.Changes[0].Content; got != wantCI {
		t.Errorf("ci.yml:\n%s", plan.UnifiedDiff("ci.yml", []byte(wantCI), []byte(got)))
	}
	if hasCode(p.Notes, "NO_BUILD_FOUND") {
		t.Errorf("an unselected project gets no gate-job note: %v", codes(p.Notes))
	}
}

// A re-run with the defaults keeps an earlier build-project answer: the build's existing gate
// says which project it was given to (Batch 4, skill review).
func TestRerunKeepsBuildProjectAnswer(t *testing.T) {
	root := writeTree(t, map[string]string{
		"apps/a/package.json": `{"name":"a"}`, "apps/a/package-lock.json": "{}",
		"apps/b/package.json": `{"name":"b"}`, "apps/b/package-lock.json": "{}",
		"docker/Dockerfile": "FROM node:22\n",
		".github/workflows/ci.yml": strings.Join([]string{
			"on: push",
			"jobs:",
			"  test:",
			"    runs-on: ubuntu-latest",
			"    steps:",
			"      - uses: actions/checkout@v5",
			"      - run: npm test",
			"        working-directory: apps/a",
			"  image:",
			"    runs-on: ubuntu-latest",
			"    steps:",
			"      - uses: actions/checkout@v5",
			"      - run: docker build -f docker/Dockerfile -t x:${{ github.sha }} docker",
			"      - run: docker push x:${{ github.sha }}",
			"",
		}, "\n"),
	})
	p := Compute(Options{Root: root, Version: "1.2.3"})
	i := slices.IndexFunc(p.Questions, func(q plan.Question) bool { return strings.HasPrefix(q.ID, "build-project:") })
	if p.Error != nil || i < 0 {
		t.Fatalf("plan: %+v, questions %v", p.Error, questionIDs(p.Questions))
	}
	bp := p.Questions[i].ID
	p = Compute(Options{Root: root, Answers: map[string]string{"projects": "apps/a,apps/b", bp: "apps/b"}, Yes: true, Version: "1.2.3"})
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if err := Write(root, p); err != nil {
		t.Fatal(err)
	}
	again := Compute(Options{Root: root, Yes: true, Version: "1.2.3"})
	if again.Error != nil {
		t.Fatal(again.Error)
	}
	for _, c := range again.Changes {
		t.Errorf("a re-run with the defaults changes %s:\n%s", c.Path, c.Diff)
	}
	// The ID names the build's line, which setup's own edits moved.
	if j := slices.IndexFunc(again.Questions, func(q plan.Question) bool { return strings.HasPrefix(q.ID, "build-project:") }); j < 0 || again.Questions[j].Default != "apps/b" {
		t.Errorf("questions: %+v", again.Questions)
	}
}

// A re-run on the Bitbucket and GitLab goldens' results has nothing to say: setup's own install
// item, trailing sh included, is part of the gate, so no COMMANDS_BEFORE_GATE (Batch 5 review).
func TestRerunGivesNoNotes(t *testing.T) {
	for _, fx := range []string{"bitbucket", "gitlab"} {
		root := t.TempDir()
		copyTree(t, filepath.Join(goldenDir, fx, "before"), root)
		copyTree(t, filepath.Join(goldenDir, fx, "after"), root)
		p := Compute(Options{Root: root, Yes: true, Version: "1.2.3"})
		if p.Error != nil {
			t.Fatalf("%s: %+v", fx, p.Error)
		}
		if len(p.Changes) > 0 || len(p.Notes) > 0 {
			t.Errorf("%s: %d changes, notes:\n%s", fx, len(p.Changes), noteDump(p.Notes))
		}
	}
}
