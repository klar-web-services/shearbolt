package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/skills"
)

func skillCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"skill"}, args...), &out, &errb, setupDeps(false, ""))
	return code, out.String(), errb.String()
}

const skillUsageLine = "shearbolt skill install [--project]  Install the coding-agent skill (/shearbolt setup, /shearbolt fix)"

func TestUsageListsSkillInstall(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), skillUsageLine) {
		t.Fatalf("exit = %d, usage lacks %q:\n%s", code, skillUsageLine, out.String())
	}
	if !strings.Contains(out.String(), `"shearbolt skill install -h"`) {
		t.Errorf("the usage hint does not mention shearbolt skill install -h:\n%s", out.String())
	}
}

func TestSkillInstallInProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	code, out, errb := skillCmd(t, "install", "--project")
	if code != 0 || errb != "" {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "skills", "shearbolt", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	want := "Installed the Shearbolt skill in .claude/skills/shearbolt. In Claude Code, type /shearbolt setup.\n" +
		"Commit .claude/skills/shearbolt to share it with your team.\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code, out, _ := skillCmd(t, "install", "--project"); code != 0 || !strings.HasPrefix(out, "The Shearbolt skill in .claude/skills/shearbolt is up to date.\n") {
		t.Errorf("second run: exit = %d, stdout = %q", code, out)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "skills", "shearbolt", "references", "fixing.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := skillCmd(t, "install", "--project"); code != 0 || !strings.HasPrefix(out, "Updated the Shearbolt skill in .claude/skills/shearbolt.") {
		t.Errorf("update: exit = %d, stdout = %q", code, out)
	}
}

func TestSkillInstallGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	code, out, errb := skillCmd(t, "install")
	if code != 0 || errb != "" {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	installed := filepath.Join(home, ".claude", "skills", "shearbolt")
	if _, err := os.Stat(filepath.Join(installed, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude")); err == nil {
		t.Error("the user install also wrote into the current directory")
	}
	if want := "Installed the Shearbolt skill in " + installed + ". In Claude Code, type /shearbolt setup.\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	// --global, which 1.0.0 needed for this, still means the user install.
	if code, out, _ := skillCmd(t, "install", "--global"); code != 0 || out != "The Shearbolt skill in "+installed+" is up to date.\n" {
		t.Errorf("--global: exit = %d, stdout = %q", code, out)
	}
}

func TestSkillInstallGlobalWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Chdir(t.TempDir())
	if code, _, errb := skillCmd(t, "install"); code != 2 || !strings.Contains(errb, "HOME") {
		t.Errorf("exit = %d, stderr = %q", code, errb)
	}
}

func TestSkillInstallGlobalNeedsAbsoluteHome(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", "relative/home")
	if code, _, errb := skillCmd(t, "install"); code != 2 || !strings.Contains(errb, "absolute") {
		t.Errorf("exit = %d, stderr = %q", code, errb)
	}
	if _, err := os.Stat("relative"); err == nil {
		t.Error("a relative HOME was used")
	}
}

func TestSkillInstallInProjectStaysInside(t *testing.T) {
	base := t.TempDir()
	project, outside := filepath.Join(base, "project"), filepath.Join(base, "outside")
	writeFiles(t, outside, map[string]string{"references/fixing.md": "keep\n"})
	if err := os.MkdirAll(filepath.Join(project, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, ".claude", "skills", "shearbolt")); err != nil {
		t.Skip(err)
	}
	t.Chdir(project)
	code, out, errb := skillCmd(t, "install", "--project")
	if code != 2 || out != "" || !strings.Contains(errb, "outside the project") || !strings.Contains(errb, "without --project") {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, out, errb)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "references", "fixing.md")); string(b) != "keep\n" {
		t.Errorf("a file outside the project changed (now %d bytes)", len(b))
	}
	if _, err := os.Lstat(filepath.Join(outside, "SKILL.md")); err == nil {
		t.Error("SKILL.md was written outside the project")
	}
}

func TestSkillInstallGlobalFollowsDotfilesSymlink(t *testing.T) {
	base := t.TempDir()
	home, dotfiles := filepath.Join(base, "home"), filepath.Join(base, "dotfiles", "claude")
	for _, d := range []string{home, dotfiles} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(dotfiles, filepath.Join(home, ".claude")); err != nil {
		t.Skip(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	if code, _, errb := skillCmd(t, "install"); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(dotfiles, "skills", "shearbolt", "SKILL.md")); err != nil {
		t.Error(err)
	}
}

func TestSkillUsageErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{}, {"nope"}, {"install", "extra"}, {"install", "--nope"}, {"install", "--project", "--global"}} {
		code, out, errb := skillCmd(t, args...)
		if code != 2 || out != "" || !strings.Contains(errb, "Usage: shearbolt skill install [--project]") {
			t.Errorf("skill %v: exit = %d, stdout = %q, stderr = %q", args, code, out, errb)
		}
	}
	if _, err := os.Stat(".claude"); err == nil {
		t.Error("a usage error wrote the skill")
	}
	if code, out, _ := skillCmd(t, "install", "-h"); code != 0 || out != "" {
		t.Errorf("install -h: exit = %d, stdout = %q", code, out)
	}
}

func TestSkillInstallRefusesForeignSkill(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	foreign := filepath.Join(dir, ".claude", "skills", "shearbolt", "SKILL.md")
	writeFiles(t, dir, map[string]string{".claude/skills/shearbolt/SKILL.md": "---\nname: shearbolt\n---\nsomeone else's\n"})
	code, out, errb := skillCmd(t, "install", "--project")
	want := ".claude/skills/shearbolt already holds a SKILL.md that shearbolt did not write; remove or rename that folder, then run shearbolt skill install --project again."
	if code != 2 || out != "" || !strings.Contains(errb, want) {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, out, errb)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "---\nname: shearbolt\n---\nsomeone else's\n" {
		t.Errorf("the foreign SKILL.md changed: %q", b)
	}
}

// The commands the skill documents, as whole lines in SKILL.md. The scripted flows below run
// exactly these, with each `--answer 'ID=VALUE'` passed as the shell would.
const (
	skillPlanLine  = "shearbolt setup --plan --json"
	skillApplyLine = "shearbolt setup --apply --json --answer 'ID=VALUE' ..."
)

// skillRule is SKILL.md's section 3.3 skip rule, line by line; applicable implements it.
var skillRule = []string{
	"- it already has an `answer`;",
	"- its `project` is set and that project is not among the chosen `projects` values;",
	"- any question ID in its `unlessChosen` list has a chosen value equal to its `project`.",
}

// documented returns the setup flags of a documented command line, up to its first --answer.
func documented(line string) []string {
	words := strings.Fields(line)[2:] // drop "shearbolt setup"
	if i := slices.Index(words, "--answer"); i >= 0 {
		words = words[:i]
	}
	return words
}

// applicable is the skill's section 3.3 rule for a question without an answer: its project, if
// set, is selected, and no question in its unlessChosen list has a chosen value equal to that
// project. chosen holds the values chosen so far (answers, or accepted defaults); without a
// projects question every project is selected.
func applicable(q plan.Question, chosen map[string]string) bool {
	if q.Project != "" {
		if sel, ok := chosen["projects"]; ok && !slices.Contains(strings.Split(sel, ","), q.Project) {
			return false
		}
	}
	for _, id := range q.UnlessChosen {
		if v, ok := chosen[id]; ok && v == q.Project {
			return false
		}
	}
	return true
}

// skillAnswers walks p's questions in order as the skill does: a question that already has an
// answer keeps it, an applicable one is asked (pick returns the user's value), others are
// skipped. It returns every chosen value.
func skillAnswers(p plan.Plan, pick func(plan.Question) string) map[string]string {
	chosen := map[string]string{}
	for _, q := range p.Questions {
		switch {
		case q.Answer != "":
			chosen[q.ID] = q.Answer
		case applicable(q, chosen):
			chosen[q.ID] = pick(q)
		}
	}
	return chosen
}

// answerArgs renders chosen values as --answer arguments, in question order.
func answerArgs(p plan.Plan, chosen map[string]string) []string {
	var args []string
	for _, q := range p.Questions {
		if v, ok := chosen[q.ID]; ok {
			args = append(args, "--answer", q.ID+"="+v)
		}
	}
	return args
}

// fixtureAnswers reads testdata/setup/<name>/answers.txt: one id=value per line; blank lines and
// # comments are ignored.
func fixtureAnswers(t *testing.T, name string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(setupFixtures, name, "answers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		id, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("answers.txt line %q has no =", l)
		}
		out[id] = v
	}
	return out
}

// contents returns the files under dir by slash path, without modes, leaving out the paths
// below skip.
func contents(t *testing.T, dir, skip string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for rel, v := range tree(t, dir) {
		if skip != "" && strings.HasPrefix(rel, skip) {
			continue
		}
		_, content, _ := strings.Cut(v, "\n")
		out[rel] = content
	}
	return out
}

func setupPlan(t *testing.T, args ...string) (int, plan.Plan) {
	t.Helper()
	code, out, errb := setupCmd(t, setupDeps(false, ""), args...)
	if out == "" {
		t.Fatalf("setup %v: exit = %d, empty stdout, stderr = %s", args, code, errb)
	}
	return code, decodePlan(t, out)
}

// TestSkillSetupFlowFollowsTheSkill is AC-16: the skill's setup flow on Plan 2's monorepo
// fixture gives Plan 2's golden result.
func TestSkillSetupFlowFollowsTheSkill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := fixtureRepo(t, "monorepo")

	// 1. Install the skill into the repository.
	if code, _, errb := skillCmd(t, "install", "--project"); code != 0 {
		t.Fatalf("skill install: exit = %d, stderr = %s", code, errb)
	}
	// 2. The installed skill documents the commands this test runs.
	b, err := os.ReadFile(filepath.Join(dir, ".claude", "skills", "shearbolt", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	for _, l := range []string{skillPlanLine, skillApplyLine} {
		if !slices.Contains(lines, l) {
			t.Fatalf("SKILL.md lacks the command line %q", l)
		}
	}
	// ...the question rule that applicable implements, and never --yes.
	for _, l := range skillRule {
		if !slices.Contains(lines, l) {
			t.Errorf("SKILL.md lacks the rule line %q", l)
		}
	}
	yes := regexp.MustCompile(`^shearbolt setup .*--yes`)
	for _, l := range lines {
		if yes.MatchString(l) {
			t.Errorf("SKILL.md runs setup with --yes: %q", l)
		}
	}
	planArgs, applyArgs := documented(skillPlanLine), documented(skillApplyLine)

	// 3. The plan writes nothing.
	before := tree(t, dir)
	code, p := setupPlan(t, planArgs...)
	if code != 0 || p.Error != nil {
		t.Fatalf("plan: exit = %d, error = %+v", code, p.Error)
	}
	sameTree(t, dir, before)

	// 4. Every applicable question gets the fixture's answer or, as a user accepting it, its
	// default, passed explicitly.
	given := fixtureAnswers(t, "monorepo")
	var asked []string
	chosen := skillAnswers(p, func(q plan.Question) string {
		asked = append(asked, q.ID)
		if v, ok := given[q.ID]; ok {
			return v
		}
		return q.Default
	})
	if !slices.Equal(asked, []string{"projects", "gate-job:apps/api"}) {
		t.Errorf("asked %v", asked)
	}
	answers := answerArgs(p, chosen)

	// 5. The plan with those answers is complete.
	if code, p := setupPlan(t, append(planArgs, answers...)...); code != 0 || !p.Complete {
		t.Fatalf("plan with answers: exit = %d, complete = %v, error = %+v", code, p.Complete, p.Error)
	}
	sameTree(t, dir, before)

	// 6. Apply with exactly those answers and no --yes.
	code, applied := setupPlan(t, append(applyArgs, answers...)...)
	if code != 0 || !applied.Complete || applied.Error != nil {
		t.Fatalf("apply: exit = %d, complete = %v, error = %+v", code, applied.Complete, applied.Error)
	}

	// 7. The tree, without .claude/, is Plan 2's golden result.
	want := contents(t, filepath.Join(setupFixtures, "monorepo", "before"), "")
	for rel, c := range contents(t, filepath.Join(setupFixtures, "monorepo", "after"), "") {
		want[rel] = c
	}
	got := contents(t, dir, ".claude/")
	for rel, w := range want {
		if g, ok := got[rel]; !ok {
			t.Errorf("%s is missing", rel)
		} else if g != w {
			t.Errorf("%s differs from the golden file:\n%s", rel, plan.UnifiedDiff(rel, []byte(w), []byte(g)))
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s was created, but is not in the golden result", rel)
		}
	}
}

// TestSkillSkipsQuestionsMadeMootByAnswers checks the skill's unlessChosen rule: assigning an
// unattributed build to a project makes that project's gate-job question moot, and setup agrees.
func TestSkillSkipsQuestionsMadeMootByAnswers(t *testing.T) {
	dir := tempRepo(t, map[string]string{
		"apps/a/package.json":      `{"name":"a"}`,
		"apps/a/package-lock.json": "{}",
		"apps/b/package.json":      `{"name":"b"}`,
		"apps/b/package-lock.json": "{}",
		"docker/Dockerfile":        "FROM scratch\n",
		".github/workflows/ci.yml": `name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - run: npm test
        working-directory: apps/a
  image:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - run: docker build -f docker/Dockerfile -t x:${{ github.sha }} docker
      - run: docker push x:${{ github.sha }}
`,
	})
	planArgs, applyArgs := documented(skillPlanLine), documented(skillApplyLine)

	// 1. The questions, and the unlessChosen links.
	_, p := setupPlan(t, planArgs...)
	if p.Error != nil {
		t.Fatalf("plan error: %+v", p.Error)
	}
	var ids []string
	bp := ""
	for _, q := range p.Questions {
		ids = append(ids, q.ID)
		if strings.HasPrefix(q.ID, "build-project:.github/workflows/ci.yml:") {
			bp = q.ID
		}
	}
	if bp == "" || !slices.Equal(ids, []string{"projects", bp, "gate-job:apps/a", "gate-job:apps/b"}) {
		t.Fatalf("questions = %v", ids)
	}
	for _, q := range p.Questions[2:] {
		if !slices.Equal(q.UnlessChosen, []string{bp}) {
			t.Errorf("%s: unlessChosen = %v, want [%s]", q.ID, q.UnlessChosen, bp)
		}
	}

	// 2–3. The user keeps both projects and gives the build to apps/b; then gate-job:apps/b is
	// moot and gate-job:apps/a is asked, and the user accepts its default.
	user := map[string]string{"projects": "apps/a,apps/b", bp: "apps/b"}
	var asked []string
	chosen := skillAnswers(p, func(q plan.Question) string {
		asked = append(asked, q.ID)
		if v, ok := user[q.ID]; ok {
			return v
		}
		return q.Default
	})
	if !slices.Equal(asked, []string{"projects", bp, "gate-job:apps/a"}) {
		t.Fatalf("asked %v, want gate-job:apps/b skipped", asked)
	}
	if chosen["gate-job:apps/a"] != ".github/workflows/ci.yml#test" {
		t.Errorf("gate-job:apps/a default = %q", chosen["gate-job:apps/a"])
	}
	answers := answerArgs(p, chosen)

	// 4. Those three answers complete the plan.
	if _, p := setupPlan(t, append(planArgs, answers...)...); !p.Complete || p.Error != nil {
		t.Fatalf("plan with answers: complete = %v, error = %+v", p.Complete, p.Error)
	}

	// 6. Control, before anything is written: without the gate-job:apps/a answer the plan is
	// not complete, and --apply names that question and writes nothing.
	partial := maps.Clone(chosen)
	delete(partial, "gate-job:apps/a")
	if _, p := setupPlan(t, append(planArgs, answerArgs(p, partial)...)...); p.Complete {
		t.Error("plan without gate-job:apps/a is complete")
	}
	before := tree(t, dir)
	code, failed := setupPlan(t, append(applyArgs, answerArgs(p, partial)...)...)
	if code != 2 || failed.Error == nil || failed.Error.Code != "UNANSWERED" || !strings.Contains(failed.Error.Message, "gate-job:apps/a") {
		t.Errorf("apply without gate-job:apps/a: exit = %d, error = %+v", code, failed.Error)
	}
	sameTree(t, dir, before)

	// 5. Apply with the three answers and no --yes.
	code, applied := setupPlan(t, append(applyArgs, answers...)...)
	if code != 0 || applied.Error != nil || !applied.Complete {
		t.Fatalf("apply: exit = %d, complete = %v, error = %+v", code, applied.Complete, applied.Error)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "apps", "b", "shearbolt.yaml"))
	if err != nil || !strings.Contains(string(cfg), "image: ${SHEARBOLT_IMAGE:-") {
		t.Errorf("apps/b/shearbolt.yaml has no image target (%v):\n%s", err, cfg)
	}
	ci, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	testJob, imageJob, ok := strings.Cut(string(ci), "\n  image:\n")
	if !ok || !strings.Contains(imageJob, "uses: klar-web-services/shearbolt@v1") || !strings.Contains(imageJob, "working-directory: apps/b") {
		t.Errorf("the apps/b gate is not in job image:\n%s", ci)
	}
	if strings.Contains(testJob, "working-directory: apps/b") || !strings.Contains(testJob, "working-directory: apps/a") {
		t.Errorf("job test should hold only the apps/a gate:\n%s", ci)
	}
}

// TestSkillFixFieldsExistInRunJSON checks every run.json path the skill's fixing reference lists
// against run.json's shape: model.Run's types, walked by their json tags. One sample run cannot
// show every path (errors is often empty, and targets[].note is omitted when empty).
func TestSkillFixFieldsExistInRunJSON(t *testing.T) {
	b, err := fs.ReadFile(skills.Files, "shearbolt/references/fixing.md")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	start, end := strings.Index(src, "<!-- run.json fields -->"), strings.Index(src, "<!-- /run.json fields -->")
	if start < 0 || end < start {
		t.Fatal("fixing.md lacks the run.json fields markers")
	}
	paths := regexp.MustCompile("(?m)^- `([^`]+)`").FindAllStringSubmatch(src[start:end], -1)
	if len(paths) == 0 {
		t.Fatal("no paths between the markers")
	}
	runType := reflect.TypeOf(model.Run{})
	for _, m := range paths {
		if err := jsonPath(runType, m[1]); err != nil {
			t.Error(err)
		}
	}
	// The walker itself rejects what run.json does not have.
	for _, bad := range []string{"-", "nope", "findings[].nope", "verdict[]", "findings.id", "targets[].note.x", "evidenceDir", "startedAt.wall"} {
		if jsonPath(runType, bad) == nil {
			t.Errorf("jsonPath accepts %q", bad)
		}
	}
}

// jsonPath checks that path (dot-separated json names, "[]" after a list) exists in the JSON
// encoding of typ.
func jsonPath(typ reflect.Type, path string) error {
	for _, part := range strings.Split(path, ".") {
		name, list := strings.CutSuffix(part, "[]")
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return fmt.Errorf("%s: %s is not inside an object (%s)", path, name, typ)
		}
		f, ok := jsonField(typ, name)
		if !ok {
			return fmt.Errorf("%s: %s has no json field %q", path, typ, name)
		}
		typ = f.Type
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if list {
			if typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array {
				return fmt.Errorf("%s: %s is not a list (%s)", path, name, typ)
			}
			typ = typ.Elem()
		}
	}
	return nil
}

// jsonField finds the field encoding/json writes as name.
func jsonField(typ reflect.Type, name string) (reflect.StructField, bool) {
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if !f.IsExported() || tag == "-" {
			continue
		}
		n, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && n == "" && f.Type.Kind() == reflect.Struct {
			if g, ok := jsonField(f.Type, name); ok {
				return g, true
			}
			continue
		}
		if n == "" {
			n = f.Name
		}
		if n == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// TestSkillFixingCoversCompose checks that the fixing reference teaches the compose fix flow.
func TestSkillFixingCoversCompose(t *testing.T) {
	b, err := fs.ReadFile(skills.Files, "shearbolt/references/fixing.md")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"### Compose images",
		"newest build of the same tag",
		"docker buildx imagetools inspect",
		"tag@sha256:",
		"shearbolt scan --target",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("fixing.md lacks %q", want)
		}
	}
}
