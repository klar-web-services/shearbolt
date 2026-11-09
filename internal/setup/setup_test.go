package setup

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// fakeCI is a plan.CI whose files hold one job per line, "<job>: <shell script>" (";" separates
// commands as in shell). Edit records what it was given, and fails the jobs in fail.
type fakeCI struct {
	files     []string
	jobNotes  []plan.Note
	jobsErr   error
	noJobs    bool // Jobs returns no jobs (every job unsupported)
	fail      []string
	editErr   error // Edit fails for the whole file
	editNotes []plan.Note
	gates     []plan.Gate
	preps     map[string][]plan.Prep
}

func (f *fakeCI) Name() string { return "github" }

func (f *fakeCI) Files(r plan.Repo) []string {
	var out []string
	for _, p := range f.files {
		if r.Exists(p) {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeCI) Jobs(r plan.Repo, file string, src []byte, classify plan.Classifier) ([]plan.Job, []plan.Note, error) {
	if f.jobsErr != nil {
		return nil, nil, f.jobsErr
	}
	if f.noJobs {
		return nil, f.jobNotes, nil
	}
	var jobs []plan.Job
	for n, line := range strings.Split(strings.TrimSpace(string(src)), "\n") {
		key, script, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		jobs = append(jobs, scriptJob(file, key, n+1, classify, script))
	}
	return jobs, f.jobNotes, nil
}

// scriptJob is a job with one run step holding script, classified like a CI plugin does.
func scriptJob(file, key string, line int, classify plan.Classifier, script string) plan.Job {
	j := plan.Job{ID: file + "#" + key, Label: path.Base(file) + ": " + key}
	st := plan.Step{Dir: "."}
	cmds, _ := shell.Parse(script, shell.State{Dir: "."})
	for c, cmd := range cmds {
		pos := plan.Pos{File: file, Job: j.ID, Step: 0, Command: c, Line: line}
		env := map[string]bool{}
		for _, n := range append(slices.Clone(cmd.Before.Env), cmd.Prefix()...) {
			env[n] = true
		}
		cl := classify(cmd, plan.Context{Dir: cmd.Before.Dir, Env: env})
		for _, b := range cl.Builds {
			b.Pos = pos
			st.Builds = append(st.Builds, b)
		}
		if cl.Push {
			st.Push = append(st.Push, c)
		}
		if cl.Gate != nil {
			g := *cl.Gate
			g.Pos = pos
			st.Gates = append(st.Gates, g)
		}
	}
	st.Commands = cmds
	j.Steps = []plan.Step{st}
	return j
}

func (f *fakeCI) Edit(r plan.Repo, file string, src []byte, jobs []plan.Job, gates []plan.Gate, preps map[string][]plan.Prep) (plan.Edited, error) {
	f.gates = append(f.gates, gates...)
	if f.preps == nil {
		f.preps = map[string][]plan.Prep{}
	}
	for k, v := range preps {
		f.preps[k] = append(f.preps[k], v...)
	}
	if f.editErr != nil {
		return plan.Edited{}, f.editErr
	}
	out := plan.Edited{Src: src, Notes: f.editNotes}
	for _, j := range jobs {
		if slices.Contains(f.fail, j.ID) {
			out.Failed = append(out.Failed, j.ID)
			continue
		}
		for _, g := range gates {
			if g.Job == j.ID {
				out.Src = append(slices.Clone(out.Src), []byte("# gate "+g.Project.Dir+" in "+j.ID+"\n")...)
			}
		}
	}
	return out, nil
}

func (f *fakeCI) Snippet(r plan.Repo, file string, g plan.Gate) string {
	return "SNIPPET(" + file + "," + g.Project.Dir + ")"
}

// writeTree creates files (path -> content) under a new temp dir and returns it. A path ending
// in "*" is made executable (the "*" is dropped).
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, "*") {
			p, mode = strings.TrimSuffix(p, "*"), 0o755
		}
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const fakeCIFile = ".github/workflows/ci.yml"

func npmTree(extra map[string]string) map[string]string {
	files := map[string]string{
		"package.json":      `{"name":"shop"}`,
		"package-lock.json": "{}",
		fakeCIFile:          "build: npm ci; npm run build\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

func change(p *plan.Plan, file string) *plan.FileChange {
	for i := range p.Changes {
		if p.Changes[i].Path == file {
			return &p.Changes[i]
		}
	}
	return nil
}

func changePaths(p *plan.Plan) []string {
	var out []string
	for _, c := range p.Changes {
		out = append(out, c.Path)
	}
	return out
}

func computeWith(t *testing.T, root string, ci *fakeCI, answers map[string]string) *plan.Plan {
	t.Helper()
	return compute(Options{Root: root, Answers: answers, Yes: true, Version: "1.2.3"}, ecosystems, []plan.CI{ci})
}

func TestSetupErrors(t *testing.T) {
	root := writeTree(t, map[string]string{fakeCIFile: "build: make\n", "README.md": "hi\n", "app/bun.lockb": "", "app/package.json": "{}"})
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error == nil || p.Error.Code != "NO_PROJECT" {
		t.Fatalf("error = %+v, want NO_PROJECT", p.Error)
	}
	if !strings.Contains(p.Error.Message, "bun install --save-text-lockfile") {
		t.Errorf("NO_PROJECT message lacks the BUN_LOCKB note: %s", p.Error.Message)
	}

	root = writeTree(t, map[string]string{"package.json": "{}", "package-lock.json": "{}"})
	p = computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error == nil || p.Error.Code != "NO_CI" {
		t.Fatalf("error = %+v, want NO_CI", p.Error)
	}
	for _, want := range []string{"Shearbolt is a CI gate", "shearbolt scan --source ."} {
		if !strings.Contains(p.Error.Message, want) {
			t.Errorf("NO_CI message lacks %q: %s", want, p.Error.Message)
		}
	}
	if len(p.Projects) != 1 || len(p.Changes) != 0 {
		t.Errorf("an error plan has projects %v and changes %v", p.Projects, changePaths(p))
	}

	root = writeTree(t, npmTree(nil))
	p = computeWith(t, root, &fakeCI{files: []string{fakeCIFile}, jobsErr: errors.New("yaml: line 3: did not find expected key")}, nil)
	if p.Error == nil || p.Error.Code != "PARSE" || p.Error.File != fakeCIFile || !strings.Contains(p.Error.Message, "line 3") {
		t.Fatalf("error = %+v, want PARSE naming the file and line", p.Error)
	}

	p = computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, map[string]string{"nope": "1"})
	if p.Error == nil || p.Error.Code != "BAD_ANSWER" || !strings.Contains(p.Error.Message, "nope") {
		t.Fatalf("error = %+v, want BAD_ANSWER", p.Error)
	}
}

func TestSetupExcludes(t *testing.T) {
	root := writeTree(t, npmTree(map[string]string{
		"testdata/projects/a/package.json":      "{}",
		"testdata/projects/a/package-lock.json": "{}",
		"node_modules/x/package.json":           "{}",
		"node_modules/x/package-lock.json":      "{}",
		"docs/notes.txt":                        "no lockfile\n",
	}))
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	cfg := change(p, "shearbolt.yaml")
	if cfg == nil {
		t.Fatalf("no shearbolt.yaml change: %v", changePaths(p))
	}
	if !strings.Contains(cfg.Content, "exclude: [testdata/]") {
		t.Errorf("config lacks exclude: [testdata/]:\n%s", cfg.Content)
	}
	loadConfig(t, cfg.Content)

	root = writeTree(t, npmTree(map[string]string{
		"tools/cli/package.json":      `{"name":"cli"}`,
		"tools/cli/package-lock.json": "{}",
	}))
	p = computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if cfg := change(p, "shearbolt.yaml"); cfg == nil || !strings.Contains(cfg.Content, "exclude: [tools/cli/]") {
		t.Errorf("root config lacks exclude: [tools/cli/]: %+v", cfg)
	}
	if cfg := change(p, "tools/cli/shearbolt.yaml"); cfg == nil || strings.Contains(cfg.Content, "exclude") {
		t.Errorf("nested config should have no excludes: %+v", cfg)
	}
}

func TestSetupNotesAndNoEditableJob(t *testing.T) {
	root := writeTree(t, npmTree(nil))
	unsupported := plan.Note{Code: "UNSUPPORTED_EDIT", Message: "job x cannot be edited", File: fakeCIFile}
	ci := &fakeCI{files: []string{fakeCIFile}, noJobs: true, jobNotes: []plan.Note{unsupported}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if got := changePaths(p); !slices.Equal(got, []string{".gitignore", "shearbolt.yaml"}) {
		t.Errorf("changes = %v, want .gitignore and shearbolt.yaml only", got)
	}
	if !slices.ContainsFunc(p.Notes, func(n plan.Note) bool { return n == unsupported }) {
		t.Errorf("the CI's Jobs note is missing: %v", codes(p.Notes))
	}
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "NO_GATE_PLACED" })
	if i < 0 || !strings.Contains(p.Notes[i].Message, "SNIPPET("+fakeCIFile+",.)") {
		t.Errorf("NO_GATE_PLACED with the snippet is missing: %v", p.Notes)
	}
	if len(p.Questions) != 0 {
		t.Errorf("questions = %v, want none", questionIDs(p.Questions))
	}
	if !p.Complete {
		t.Error("a plan without questions is complete")
	}
}

func TestSetupChangesSortedWithDiffBaseMode(t *testing.T) {
	root := writeTree(t, npmTree(map[string]string{
		".gitignore": "node_modules/\n",
		"Dockerfile": "FROM node:22\n",
		fakeCIFile:   "build: docker build -t shop:1 . && docker push shop:1\n",
	}))
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}, editNotes: []plan.Note{{Code: "TAG_ADDED", Message: "m"}}}, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	want := []string{".dockerignore", ".github/workflows/ci.yml", ".gitignore", "shearbolt.yaml"}
	if got := changePaths(p); !slices.Equal(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	for _, c := range p.Changes {
		if c.Diff == "" || c.Content == "" {
			t.Errorf("%s: empty diff or content", c.Path)
		}
		switch c.Action {
		case "create":
			if c.Base != nil || c.Mode != 0o644 || !strings.HasPrefix(c.Diff, "--- /dev/null") {
				t.Errorf("%s: create with base %q, mode %v, diff %q", c.Path, c.Base, c.Mode, c.Diff)
			}
		case "modify":
			disk, _ := os.ReadFile(filepath.Join(root, c.Path))
			if string(c.Base) != string(disk) || c.Mode != 0o644 || !strings.HasPrefix(c.Diff, "--- a/"+c.Path) {
				t.Errorf("%s: modify with base %q, mode %v", c.Path, c.Base, c.Mode)
			}
		default:
			t.Errorf("%s: action %q", c.Path, c.Action)
		}
	}
	if !hasCode(p.Notes, "TAG_ADDED") {
		t.Errorf("Edit's notes are missing: %v", codes(p.Notes))
	}
	if !p.Complete || p.ShearboltVersion != "1.2.3" || p.SchemaVersion != 1 || len(p.CI) != 1 || p.CI[0] != (plan.CIFile{System: "github", File: fakeCIFile}) {
		t.Errorf("plan header = %+v", p)
	}
}

func TestSetupComplete(t *testing.T) {
	root := writeTree(t, npmTree(map[string]string{
		"tools/cli/package.json":      `{"name":"cli"}`,
		"tools/cli/package-lock.json": "{}",
	}))
	ci := &fakeCI{files: []string{fakeCIFile}}
	p := compute(Options{Root: root, Version: "dev"}, ecosystems, []plan.CI{ci})
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if p.Complete {
		t.Errorf("questions %v are unanswered, so the plan is not complete", questionIDs(p.Questions))
	}
	if len(p.Changes) == 0 {
		t.Error("changes are computed with the defaults even when answers are missing")
	}
	p = compute(Options{Root: root, Version: "dev", Answers: map[string]string{"projects": "."}}, ecosystems, []plan.CI{ci})
	if !p.Complete || p.Questions[0].Answer != "." {
		t.Errorf("complete = %v, answer = %q", p.Complete, p.Questions[0].Answer)
	}
	if change(p, "tools/cli/shearbolt.yaml") != nil {
		t.Error("an unselected project gets no config")
	}
}

// rustMakeTree is a Rust project whose CI runs `make release` in each of the given jobs.
func rustMakeTree(jobs ...string) map[string]string {
	ci := ""
	for _, j := range jobs {
		ci += j + ": make release\n"
	}
	return map[string]string{
		"Cargo.toml":        "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"Cargo.lock":        "version = 3\n",
		"src/main.rs":       "fn main() {}\n",
		"Makefile":          "release:\n\tcargo build --release --locked\n\nother:\n\tcargo build --release --bin x\n",
		"scripts/build.sh*": "#!/bin/sh\ncargo build --release\n",
		fakeCIFile:          ci + "script: ./scripts/build.sh\n",
	}
}

func TestSetupPrepsAndViaInserts(t *testing.T) {
	root := writeTree(t, rustMakeTree("linux", "macos"))
	ci := &fakeCI{files: []string{fakeCIFile}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	// Preps go to the CI per job, with the build's position, independent of gates.
	for _, job := range []string{"linux", "macos", "script"} {
		ps := ci.preps[fakeCIFile+"#"+job]
		if len(ps) != 1 || ps[0].Install == nil || ps[0].Install.Marker != "cargo-auditable" || ps[0].Pos.Job != fakeCIFile+"#"+job {
			t.Errorf("preps[%s] = %+v, want one install at the build", job, ps)
		}
	}
	mk := change(p, "Makefile")
	if mk == nil {
		t.Fatalf("Makefile not rewritten: %v", changePaths(p))
	}
	// Two jobs reach the same insert: it is applied once. The RewriteOnly build in `other` is
	// rewritten too (file-wide rewrite).
	want := "release:\n\tcargo auditable build --release --locked\n\nother:\n\tcargo auditable build --release --bin x\n"
	if mk.Content != want || mk.Mode != 0o644 {
		t.Errorf("Makefile = %q (mode %v), want %q", mk.Content, mk.Mode, want)
	}
	sh := change(p, "scripts/build.sh")
	if sh == nil || sh.Content != "#!/bin/sh\ncargo auditable build --release\n" || sh.Mode != 0o755 {
		t.Errorf("scripts/build.sh = %+v, want the rewrite with mode 0755", sh)
	}
}

func TestSetupFailedJobDropsViaInserts(t *testing.T) {
	root := writeTree(t, rustMakeTree("linux", "macos"))
	// One of the two jobs running `make release` cannot be edited, so the Makefile's reached build
	// keeps its plain cargo build (its job gets no install). The script's job is fine.
	ci := &fakeCI{files: []string{fakeCIFile}, fail: []string{fakeCIFile + "#macos"}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	mk := change(p, "Makefile")
	want := "release:\n\tcargo build --release --locked\n\nother:\n\tcargo auditable build --release --bin x\n"
	if mk == nil || mk.Content != want {
		t.Errorf("Makefile = %+v, want only the RewriteOnly build rewritten:\n%s", mk, want)
	}
	if sh := change(p, "scripts/build.sh"); sh == nil {
		t.Error("the script of a job that was edited is still rewritten")
	}
}

func TestSetupWriteRefusesChangedFiles(t *testing.T) {
	root := writeTree(t, npmTree(map[string]string{".gitignore": "node_modules/\n", "Dockerfile": "FROM node:22\n"}))
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(root, p); err == nil {
		t.Fatal("Write succeeded although .gitignore changed after the plan")
	}
	for _, f := range []string{"shearbolt.yaml", ".dockerignore"} {
		if _, err := os.Stat(filepath.Join(root, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was written although Write failed", f)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(root, ".gitignore")); string(got) != "changed\n" {
		t.Errorf(".gitignore = %q", got)
	}

	// A file created after the plan blocks its create, too.
	p = computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if err := os.WriteFile(filepath.Join(root, "shearbolt.yaml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(root, p); err == nil {
		t.Fatal("Write succeeded although shearbolt.yaml appeared after the plan")
	}
}

func TestSetupWrite(t *testing.T) {
	root := writeTree(t, rustMakeTree("linux"))
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if err := Write(root, p); err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Changes {
		full := filepath.Join(root, c.Path)
		got, err := os.ReadFile(full)
		if err != nil || string(got) != c.Content {
			t.Errorf("%s = %q (%v), want %q", c.Path, got, err, c.Content)
		}
		info, _ := os.Stat(full)
		if info.Mode().Perm() != c.Mode.Perm() {
			t.Errorf("%s: mode %v, want %v", c.Path, info.Mode().Perm(), c.Mode.Perm())
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".shearbolt-") || strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	// A plan with an error writes nothing.
	if err := Write(root, &plan.Plan{Error: &plan.Note{Code: "NO_CI"}}); err == nil {
		t.Error("Write accepted a plan with an error")
	}
}

func TestSetupComputeIsPure(t *testing.T) {
	root := writeTree(t, rustMakeTree("linux"))
	before := snapshot(t, root)
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error != nil || len(p.Changes) == 0 {
		t.Fatalf("plan: %+v", p)
	}
	if after := snapshot(t, root); !mapsEqual(before, after) {
		t.Error("Compute changed the repository")
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		info, _ := d.Info()
		out[p] = string(b) + "|" + info.Mode().String()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Write refuses a change whose directory resolves outside the repository through a symlink, and
// writes nothing at all.
func TestSetupWriteRefusesSymlinkedDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	p := &plan.Plan{Changes: []plan.FileChange{
		{Path: "a.txt", Action: "create", Content: "a\n", Mode: 0o644},
		{Path: "link/x.txt", Action: "create", Content: "x\n", Mode: 0o644},
	}}
	if err := Write(root, p); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Fatalf("Write = %v, want a refusal", err)
	}
	for _, f := range []string{filepath.Join(root, "a.txt"), filepath.Join(outside, "x.txt")} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was written", f)
		}
	}
	for _, bad := range []string{"../x", "/etc/x", "a/../../x", ""} {
		if err := Write(root, &plan.Plan{Changes: []plan.FileChange{{Path: bad, Action: "create", Content: "x"}}}); err == nil {
			t.Errorf("Write accepted path %q", bad)
		}
	}
}

func TestSetupDedupeNotes(t *testing.T) {
	a := plan.Note{Code: "RUST_PATH_UNKNOWN", Message: "m", File: "Makefile", Line: 3}
	b := plan.Note{Code: "RUST_PATH_UNKNOWN", Message: "m", File: "Makefile", Line: 4}
	got := dedupeNotes([]plan.Note{a, b, a, b, a})
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("dedupeNotes = %+v", got)
	}
	if dedupeNotes(nil) != nil {
		t.Error("no notes stay nil")
	}
}

// The same note about a build in a called file, reached by two jobs, appears once in the plan.
func TestSetupDedupesNotesThroughCompute(t *testing.T) {
	files := rustMakeTree("linux", "macos")
	files["Makefile"] = "release:\n\tcargo build --release --target $(TRIPLE)\n"
	p := computeWith(t, writeTree(t, files), &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if n := countCode(p.Notes, "RUST_PATH_UNKNOWN"); n != 1 {
		t.Errorf("RUST_PATH_UNKNOWN appears %d times: %v", n, p.Notes)
	}
}

// A CI file that cannot be edited at all gets UNSUPPORTED_EDIT with the snippet of each of its
// gates (Batch 5 review).
func TestSetupFileEditFailureCarriesSnippet(t *testing.T) {
	root := writeTree(t, npmTree(nil))
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}, editErr: errors.New("boom")}, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
	if i < 0 || !strings.Contains(p.Notes[i].Message, "boom") || !strings.Contains(p.Notes[i].Message, "SNIPPET("+fakeCIFile+",.)") {
		t.Errorf("notes %+v", p.Notes)
	}
}

// A compose target the user removed is not added back: the plan has no changes and says so.
func TestSetupRemovedComposeTargetStaysRemoved(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docker-compose.yml": composeImage,
		"test/compose.yml":   composeImage,
		fakeCIFile:           "test: docker compose up -d\n",
	})
	ci := &fakeCI{files: []string{fakeCIFile}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	cfg := change(p, "shearbolt.yaml")
	if cfg == nil || !strings.Contains(cfg.Content, "compose: docker-compose.yml") || !strings.Contains(cfg.Content, "compose: test/compose.yml") || strings.Contains(cfg.Content, "source:") {
		t.Fatalf("config: %+v", cfg)
	}
	if err := Write(root, p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "shearbolt.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), "  - name: test-compose")
	if i < 0 {
		t.Fatalf("no test-compose target:\n%s", b)
	}
	if err := os.WriteFile(path, b[:i], 0o644); err != nil {
		t.Fatal(err)
	}
	p = computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if change(p, "shearbolt.yaml") != nil {
		t.Fatalf("config changed:\n%s", change(p, "shearbolt.yaml").Content)
	}
	n := noteOf(t, p.Notes, "COMPOSE_NOT_GATED")
	if !strings.HasPrefix(n.Message, "test/compose.yml is not gated") || countCode(p.Notes, "COMPOSE_NOT_GATED") != 1 {
		t.Fatalf("notes: %+v", p.Notes)
	}
}

// A compose target the user added for a file setup did not plan stays, and the gates scan it.
func TestSetupHandAddedComposeTarget(t *testing.T) {
	cfg := "version: 1\ntargets:\n  - name: docker-compose\n    compose: docker-compose.yml\n  - name: stack\n    compose: stack.yml\n"
	root := writeTree(t, map[string]string{
		"docker-compose.yml": composeImage,
		"stack.yml":          composeImage,
		"shearbolt.yaml":     cfg,
		fakeCIFile:           "test: docker compose up -d\ndeploy: docker stack deploy -c stack.yml app\n",
	})
	ci := &fakeCI{files: []string{fakeCIFile}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if change(p, "shearbolt.yaml") != nil {
		t.Fatalf("config changed: %s", change(p, "shearbolt.yaml").Content)
	}
	if len(ci.gates) == 0 {
		t.Fatal("no gate placed")
	}
	for _, g := range ci.gates {
		if g.Targets != nil && !slices.Contains(g.Targets, "stack") {
			t.Fatalf("gate targets %v lack stack", g.Targets)
		}
	}
}
