package gitlab_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/ci/gitlab"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

const file = ".gitlab-ci.yml"

const (
	install = "(curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | SHEARBOLT_VERSION=1.2.3 sh"
	scanWeb = `SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`
)

// vars and cache are what a job that gets a gate gains when it has neither.
var (
	vars  = []string{"  variables:", "    SHEARBOLT_CACHE_DIR: $CI_PROJECT_DIR/.shearbolt/cache"}
	cache = []string{"  cache:", "    - key: shearbolt", "      paths:", "        - .shearbolt/cache"}
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func npmRepo(ci string) map[string]string {
	return map[string]string{
		"package.json":      `{"name":"web"}`,
		"package-lock.json": "{}",
		"Dockerfile":        "FROM node:22\n",
		file:                ci,
	}
}

// result runs setup on files and returns the plan and .gitlab-ci.yml afterwards ("" when it is
// unchanged). The result must be valid YAML, and a second run on it must change nothing.
func result(t *testing.T, files map[string]string) (*plan.Plan, string) {
	t.Helper()
	p, out, _ := rerun(t, files)
	return p, out
}

// rerun is result that also returns the second run's plan.
func rerun(t *testing.T, files map[string]string) (*plan.Plan, string, *plan.Plan) {
	t.Helper()
	root := tree(t, files)
	p := setup.Compute(setup.Options{Root: root, Yes: true, Version: "1.2.3"})
	if p.Error != nil {
		t.Fatalf("plan error: %+v", p.Error)
	}
	out := ""
	for _, c := range p.Changes {
		if c.Path == file {
			out = c.Content
			if _, err := yamledit.Parse([]byte(c.Content)); err != nil {
				t.Fatalf("the edited file is not valid YAML: %v\n%s", err, c.Content)
			}
		}
	}
	if err := setup.Write(root, p); err != nil {
		t.Fatal(err)
	}
	again := setup.Compute(setup.Options{Root: root, Yes: true, Version: "1.2.3"})
	if again.Error != nil {
		t.Fatalf("second plan error: %+v", again.Error)
	}
	for _, c := range again.Changes {
		t.Errorf("a second run changes %s:\n%s", c.Path, c.Diff)
	}
	return p, out, again
}

func lines(s ...string) string { return strings.Join(s, "\n") + "\n" }

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func want(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s:\n%s\nwant:\n%s", file, got, want)
	}
}

func notes(p *plan.Plan, code string) []plan.Note {
	var out []plan.Note
	for _, n := range p.Notes {
		if n.Code == code {
			out = append(out, n)
		}
	}
	return out
}

func noteCodes(p *plan.Plan) []string {
	var out []string
	for _, n := range p.Notes {
		out = append(out, n.Code+": "+n.Message)
	}
	return out
}

// jobs runs the plugin's Jobs with a classifier that recognizes nothing.
func jobs(t *testing.T, src string) ([]plan.Job, []plan.Note) {
	t.Helper()
	js, ns, err := gitlab.Plugin{}.Jobs(plan.Repo{Root: t.TempDir()}, file, []byte(src), func(shell.Command, plan.Context) plan.Classified { return plan.Classified{} })
	if err != nil {
		t.Fatal(err)
	}
	return js, ns
}

func ids(js []plan.Job) []string {
	var out []string
	for _, j := range js {
		out = append(out, j.ID)
	}
	return out
}

func TestGitLabFiles(t *testing.T) {
	root := tree(t, map[string]string{file: "x:\n  script: [a]\n", "sub/" + file: "x: 1\n"})
	if got := (gitlab.Plugin{}).Files(plan.Repo{Root: root}); !slices.Equal(got, []string{file}) {
		t.Errorf("Files = %q", got)
	}
}

// Jobs are the top-level keys that are not keywords or hidden; one note lists the jobs without
// their own block script of plain strings.
func TestGitLabJobs(t *testing.T) {
	src := lines(
		"stages: [build]",
		"variables:",
		"  GLOBAL: x",
		"default:",
		"  image: node:22",
		"include:",
		"  - local: other.yml",
		"workflow:",
		"  rules:",
		"    - when: always",
		"before_script:",
		"  - echo hi",
		".build-template:",
		"  script:",
		"    - docker build -t web:1 .",
		"build:",
		"  extends: .build-template",
		"release:",
		"  script: make release",
		"refs:",
		"  script:",
		"    - !reference [.build-template, script]",
		"trigger-job:",
		"  trigger: other/project",
		"test:",
		"  variables:",
		"    JOB_VAR: y",
		"  script:",
		"    - npm test",
	)
	js, ns := jobs(t, src)
	if !slices.Equal(ids(js), []string{file + "#test"}) {
		t.Fatalf("jobs = %q", ids(js))
	}
	if len(ns) != 2 || ns[0].Code != "INCLUDE_NOT_READ" || ns[1].Code != "UNSUPPORTED_EDIT" {
		t.Fatalf("notes %+v", ns)
	}
	ns = ns[1:]
	for _, s := range []string{"build", "release", "refs", `"$HOME/.local/bin/shearbolt" scan --ci`} {
		if !strings.Contains(ns[0].Message, s) {
			t.Errorf("the note does not mention %q:\n%s", s, ns[0].Message)
		}
	}
	if strings.Contains(ns[0].Message, ".build-template") || strings.Contains(ns[0].Message, "trigger-job") {
		t.Errorf("hidden and trigger jobs are not listed:\n%s", ns[0].Message)
	}
	if js[len(js)-1].Label != file+": test" {
		t.Errorf("label %q", js[len(js)-1].Label)
	}
}

// A job with a merge key is not a job; neither is one defined through an anchor.
func TestGitLabMergeKeyJob(t *testing.T) {
	js, ns := jobs(t, lines(
		".base: &base",
		"  image: node:22",
		"build:",
		"  <<: *base",
		"  script:",
		"    - docker build -t web:1 .",
	))
	if len(js) != 0 || len(ns) != 1 || !strings.Contains(ns[0].Message, "build (it uses a YAML merge key)") {
		t.Errorf("jobs %q, notes %+v", ids(js), ns)
	}
}

// The variables and cache of a job with neither go before its before_script (or script).
func TestGitLabVariablesAndCache(t *testing.T) {
	_, got := result(t, npmRepo(lines(
		"build:",
		"  image: docker:27",
		"  script:",
		"    - docker build -t web:1 .",
	)))
	want(t, got, lines(cat(
		[]string{"build:", "  image: docker:27"}, vars, cache,
		[]string{"  script:", "    - docker build -t web:1 -t web:shearbolt-$CI_COMMIT_SHA .", "    - " + install, "    - " + scanWeb},
	)...))

	// Existing variables get the entry; the cache list goes after them.
	_, got = result(t, npmRepo(lines(
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
		"  variables:",
		"    DOCKER_HOST: tcp://docker:2375",
	)))
	want(t, got, lines(
		"build:",
		"  script:",
		"    - docker build -t web:1 -t web:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		"    - "+scanWeb,
		"  variables:",
		"    DOCKER_HOST: tcp://docker:2375",
		"    SHEARBOLT_CACHE_DIR: $CI_PROJECT_DIR/.shearbolt/cache",
		"  cache:",
		"    - key: shearbolt",
		"      paths:",
		"        - .shearbolt/cache",
	))
}

// A cache: mapping becomes a one-item list, then gets the shearbolt entry.
func TestGitLabCacheMappingConverted(t *testing.T) {
	_, got := result(t, npmRepo(lines(
		"build:",
		"  cache:",
		"    key: node",
		"    paths: [node_modules/]",
		"  script:",
		"    - docker build -t web:1 .",
	)))
	want(t, got, lines(cat(
		[]string{"build:"},
		[]string{"  cache:", "    - key: node", "      paths: [node_modules/]", "    - key: shearbolt", "      paths:", "        - .shearbolt/cache"},
		vars, // before the first of before_script and script
		[]string{"  script:", "    - docker build -t web:1 -t web:shearbolt-$CI_COMMIT_SHA .", "    - " + install, "    - " + scanWeb},
	)...))

	// A list gets the entry appended.
	p, got := result(t, npmRepo(lines(
		"build:",
		"  cache:",
		"    - key: node",
		"      paths:",
		"        - node_modules/",
		"  script:",
		"    - docker build -t web:1 .",
	)))
	if !strings.Contains(got, lines("        - node_modules/", "    - key: shearbolt", "      paths:", "        - .shearbolt/cache")) {
		t.Errorf("%s:\n%s\n%v", file, got, noteCodes(p))
	}
}

// No cache edit, but CACHE_NOT_ADDED with the snippet: an inherited default or global cache, a
// job at GitLab's limit of 4 caches, and a flow-style cache.
func TestGitLabCacheNotAdded(t *testing.T) {
	for name, ci := range map[string]string{
		"default": lines(
			"default:",
			"  cache:",
			"    paths: [node_modules/]",
			"build:",
			"  script:",
			"    - docker build -t web:1 .",
		),
		"global": lines(
			"cache:",
			"  paths: [node_modules/]",
			"build:",
			"  script:",
			"    - docker build -t web:1 .",
		),
		"four": lines(
			"build:",
			"  cache:",
			"    - key: a",
			"      paths: [a/]",
			"    - key: b",
			"      paths: [b/]",
			"    - key: c",
			"      paths: [c/]",
			"    - key: d",
			"      paths: [d/]",
			"  script:",
			"    - docker build -t web:1 .",
		),
		"flow": lines(
			"build:",
			"  cache: {key: node, paths: [node_modules/]}",
			"  script:",
			"    - docker build -t web:1 .",
		),
	} {
		t.Run(name, func(t *testing.T) {
			p, got := result(t, npmRepo(ci))
			if !strings.Contains(got, scanWeb) || strings.Contains(got, "key: shearbolt") {
				t.Errorf("%s:\n%s", file, got)
			}
			if n := notes(p, "CACHE_NOT_ADDED"); len(n) != 1 || !strings.Contains(n[0].Message, "- key: shearbolt") {
				t.Errorf("notes %v", noteCodes(p))
			}
		})
	}
}

// Jobs without an editable script are left alone and listed; the editable job is edited.
func TestGitLabUnsupportedJobsUnchanged(t *testing.T) {
	src := lines(
		".build-template:",
		"  script:",
		"    - docker build -t web:1 .",
		"build:",
		"  extends: .build-template",
		"release:",
		"  script: make release",
		"test:",
		"  script:",
		"    - npm test",
	)
	p, got := result(t, npmRepo(src))
	want(t, got, strings.TrimSuffix(src, lines("  script:", "    - npm test"))+lines(cat(
		vars, cache, []string{"  script:", "    - " + install, `    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`, "    - npm test"},
	)...))
	n := notes(p, "UNSUPPORTED_EDIT")
	if len(n) != 1 || !strings.Contains(n[0].Message, "build (") || !strings.Contains(n[0].Message, "release (") {
		t.Errorf("notes %v", noteCodes(p))
	}
}

// No editable job: no gate-job question, no CI change, the config and .gitignore are written,
// and NO_GATE_PLACED carries the GitLab snippet.
func TestGitLabNoEditableJob(t *testing.T) {
	files := npmRepo(lines(
		".build-template:",
		"  script:",
		"    - docker build -t web:1 .",
		"build:",
		"  extends: .build-template",
		"push:",
		"  extends: .build-template",
	))
	delete(files, "Dockerfile") // no .dockerignore then
	p, got := result(t, files)
	if got != "" || len(p.Questions) != 0 {
		t.Errorf("%s:\n%s\nquestions %+v", file, got, p.Questions)
	}
	if n := notes(p, "NO_GATE_PLACED"); len(n) != 1 || !strings.Contains(n[0].Message, "    - "+install) {
		t.Errorf("notes %v", noteCodes(p))
	}
	var paths []string
	for _, c := range p.Changes {
		paths = append(paths, c.Path)
	}
	if !slices.Equal(paths, []string{".gitignore", "shearbolt.yaml"}) {
		t.Errorf("changes %q", paths)
	}
}

// A file with a spec: header is not parsed further: UNSUPPORTED_EDIT, not PARSE.
func TestGitLabSpecHeader(t *testing.T) {
	src := lines(
		"spec:",
		"  inputs:",
		"    stage:",
		"      default: test",
		"---",
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
	)
	js, ns := jobs(t, src)
	if len(js) != 0 || len(ns) != 1 || ns[0].Code != "UNSUPPORTED_EDIT" || !strings.Contains(ns[0].Message, "spec:") {
		t.Fatalf("jobs %q, notes %+v", ids(js), ns)
	}
	p, got := result(t, npmRepo(src))
	if got != "" || !slices.ContainsFunc(p.Notes, func(n plan.Note) bool { return n.Code == "NO_GATE_PLACED" }) {
		t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
	}

	// Any other multi-document file is a parse error.
	if _, _, err := (gitlab.Plugin{}).Jobs(plan.Repo{}, file, []byte("a:\n  script: [x]\n---\nb: 1\n"), nil); err == nil {
		t.Error("a second document without a spec: header is accepted")
	}
}

// before_script runs in the job's shell before the script: its cd sets where the script starts,
// so the gate names its config. Top-level and job variables are in each command's env.
func TestGitLabBeforeScriptAndVariables(t *testing.T) {
	files := npmRepo(lines(
		"default:",
		"  before_script:",
		"    - cd app",
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
	))
	delete(files, "package.json")
	delete(files, "package-lock.json")
	delete(files, "Dockerfile")
	files["app/package.json"] = `{"name":"web"}`
	files["app/package-lock.json"] = "{}"
	files["app/Dockerfile"] = "FROM node:22\n"
	_, got := result(t, files)
	if !strings.Contains(got, `scan --ci --config "$CI_PROJECT_DIR/app/shearbolt.yaml"`) || !strings.Contains(got, "web:shearbolt-") {
		t.Errorf("%s:\n%s", file, got)
	}

	// A set +e there runs in the gate's shell too.
	p, got := result(t, npmRepo(lines(
		"default:",
		"  before_script:",
		"    - set +e",
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
	)))
	if got != "" || len(notes(p, "UNSUPPORTED_EDIT")) != 1 {
		t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
	}

	var env map[string]bool
	_, _, err := gitlab.Plugin{}.Jobs(plan.Repo{Root: t.TempDir()}, file, []byte(lines(
		"variables:",
		"  GLOBAL: x",
		"build:",
		"  variables:",
		"    LOCAL: y",
		"  script:",
		"    - X=1 echo hi",
	)), func(_ shell.Command, ctx plan.Context) plan.Classified { env = ctx.Env; return plan.Classified{} })
	if err != nil || !env["GLOBAL"] || !env["LOCAL"] || !env["X"] {
		t.Errorf("env %v (%v)", env, err)
	}
}

// A push in after_script (the job's own, or default's) is reported, not edited.
func TestGitLabAfterScriptPush(t *testing.T) {
	p, got := result(t, npmRepo(lines(
		"default:",
		"  after_script:",
		"    - docker push web:1",
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
		"test:",
		"  script:",
		"    - npm test",
		"  after_script:",
		"    - echo done",
	)))
	if !strings.Contains(got, scanWeb) {
		t.Errorf("%s:\n%s", file, got)
	}
	n := notes(p, "PUSH_IN_AFTER_SCRIPT")
	if len(n) != 1 || n[0].Line != 3 || !strings.Contains(n[0].Message, "build") {
		t.Errorf("notes %+v", n)
	}
}

func TestGitLabSnippet(t *testing.T) {
	got := gitlab.Plugin{}.Snippet(plan.Repo{}, file, plan.Gate{Project: plan.Project{Dir: "."}, Version: "dev"})
	exp := "    - (curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | sh\n" +
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`
	if got != exp {
		t.Errorf("snippet:\n%s\nwant:\n%s", got, exp)
	}
}

// A CRLF file keeps its line endings, inserted lines included.
func TestGitLabCRLF(t *testing.T) {
	src := strings.ReplaceAll(lines(
		"build:",
		"  script:",
		"    - |",
		"      docker build -t web:1 .",
		"      docker push web:1",
	), "\n", "\r\n")
	_, got := result(t, npmRepo(src))
	exp := strings.ReplaceAll(lines(cat(
		[]string{"build:"}, vars, cache,
		[]string{"  script:", "    - docker build -t web:1 -t web:shearbolt-$CI_COMMIT_SHA .", "    - " + install, "    - " + scanWeb, "    - docker push web:1"},
	)...), "\n", "\r\n")
	want(t, got, exp)
}

// before_script runs before the script, so before any gate: an image or binary build there
// makes the job unsupported, and a push there gets PUSH_BEFORE_GATE (Batch 5 review).
func TestGitLabBeforeScriptClassified(t *testing.T) {
	for name, ci := range map[string]string{
		"own": lines(
			"build:",
			"  before_script:",
			"    - docker build -t x:$CI_COMMIT_SHA .",
			"  script:",
			"    - docker push x:$CI_COMMIT_SHA",
		),
		"default": lines(
			"default:",
			"  before_script:",
			"    - docker build -t x:$CI_COMMIT_SHA .",
			"build:",
			"  script:",
			"    - docker push x:$CI_COMMIT_SHA",
		),
		"unreadable": lines(
			".setup:",
			"  before_script:",
			"    - docker build -t x:$CI_COMMIT_SHA .",
			"build:",
			"  before_script:",
			"    - !reference [.setup, before_script]",
			"  script:",
			"    - docker push x:$CI_COMMIT_SHA",
		),
	} {
		t.Run(name, func(t *testing.T) {
			js, ns := jobs(t, ci) // with a classifier that sees nothing, only the unreadable one is left out
			if name == "unreadable" && len(js) != 0 {
				t.Errorf("jobs %q", ids(js))
			}
			p, got := result(t, npmRepo(ci))
			n := notes(p, "UNSUPPORTED_EDIT")
			if got != "" || len(n) == 0 || !strings.Contains(n[0].Message, "build (") || !strings.Contains(n[0].Message, "before_script") {
				t.Errorf("%s:\n%s\nnotes %v (%v)", file, got, noteCodes(p), ns)
			}
		})
	}

	// A push there is reported, and the job is still edited.
	p, got := result(t, npmRepo(lines(
		"build:",
		"  before_script:",
		"    - docker push x:previous",
		"  script:",
		"    - docker build -t x:$CI_COMMIT_SHA .",
	)))
	n := notes(p, "PUSH_BEFORE_GATE")
	if !strings.Contains(got, "scan --ci") || len(n) != 1 || n[0].Line != 3 || !strings.Contains(n[0].Message, "docker push x:previous") {
		t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
	}
}

// An --all-tags push of the scanned repository in after_script fails the job too.
func TestGitLabAfterScriptAllTags(t *testing.T) {
	p, got := result(t, npmRepo(lines(
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
		"  after_script:",
		"    - docker push --all-tags web",
	)))
	if n := notes(p, "UNSUPPORTED_EDIT"); got != "" || len(n) != 1 || !strings.Contains(n[0].Message, "--all-tags") {
		t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
	}
}

// Batch 5 review, minor: jobs whose after_script setup cannot read, and extends/inherit jobs
// without their own before_script when a before_script in the file turns errexit off, are left
// out; include: gets INCLUDE_NOT_READ.
func TestGitLabMoreSkips(t *testing.T) {
	js, ns := jobs(t, lines(
		".base:",
		"  before_script:",
		"    - set +e",
		"flow:",
		"  script:",
		"    - npm test",
		"  after_script: [echo done]",
		"ref:",
		"  script:",
		"    - npm test",
		"  after_script: !reference [.base, before_script]",
		"inherits:",
		"  extends: .other",
		"  script:",
		"    - docker build -t web:1 .",
		"own:",
		"  extends: .other",
		"  before_script:",
		"    - echo own",
		"  script:",
		"    - npm test",
	))
	if !slices.Equal(ids(js), []string{file + "#own"}) {
		t.Errorf("jobs %q", ids(js))
	}
	if len(ns) != 1 {
		t.Fatalf("notes %+v", ns)
	}
	for _, s := range []string{"flow (its after_script", "ref (its after_script", "inherits (", "set +e"} {
		if !strings.Contains(ns[0].Message, s) {
			t.Errorf("the note does not say %q:\n%s", s, ns[0].Message)
		}
	}

	_, ns = jobs(t, lines(
		"include:",
		"  - project: acme/ci",
		"    file: /templates/defaults.yml",
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
	))
	if len(ns) != 1 || ns[0].Code != "INCLUDE_NOT_READ" || !strings.Contains(ns[0].Message, "include:") || !strings.Contains(ns[0].Message, "scan --ci") {
		t.Errorf("notes %+v", ns)
	}
}

// GitLab jobs left out for their layout (Batch 5 review: skip paths without a test).
func TestGitLabSkipPaths(t *testing.T) {
	_, ns := jobs(t, lines(
		".tpl: &tpl",
		"  script:",
		"    - npm test",
		".s: &s",
		"  - npm test",
		"flowjob: {script: [npm test]}",
		"aliased: *tpl",
		"aliasscript:",
		"  script: *s",
		"mapitem:",
		"  script:",
		"    - {a: b}",
		"emptyscript:",
		"  script: []",
	))
	if len(ns) != 1 {
		t.Fatalf("notes %+v", ns)
	}
	for _, s := range []string{"flowjob (", "aliased (it uses a YAML anchor or alias)", "aliasscript (its script is a YAML alias)", "mapitem (script item 1 is not a plain string)", "emptyscript ("} {
		if !strings.Contains(ns[0].Message, s) {
			t.Errorf("the note does not say %q:\n%s", s, ns[0].Message)
		}
	}
}

// A job with extends or inherit: and no before_script of its own may run any before_script in
// the file: a build in one leaves the job out, a push gets PUSH_BEFORE_GATE (Batch 5 re-review).
func TestGitLabInheritedBeforeScript(t *testing.T) {
	for name, ci := range map[string]string{
		"extends": lines(
			".base:",
			"  before_script:",
			"    - docker build -t x:$CI_COMMIT_SHA .",
			"build:",
			"  extends: .base",
			"  script:",
			"    - docker push x:$CI_COMMIT_SHA",
		),
		"inherit": lines(
			"default:",
			"  before_script:",
			"    - docker build -t x:$CI_COMMIT_SHA .",
			"build:",
			"  inherit:",
			"    default: [before_script]",
			"  script:",
			"    - docker push x:$CI_COMMIT_SHA",
		),
	} {
		t.Run(name, func(t *testing.T) {
			p, got := result(t, npmRepo(ci))
			n := notes(p, "UNSUPPORTED_EDIT")
			if got != "" || len(n) != 1 || !strings.Contains(n[0].Message, "build (") || !strings.Contains(n[0].Message, "docker build -t x:$CI_COMMIT_SHA .") {
				t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
			}
		})
	}

	p, got := result(t, npmRepo(lines(
		".base:",
		"  before_script:",
		"    - docker push x:old",
		"build:",
		"  extends: .base",
		"  script:",
		"    - docker build -t x:$CI_COMMIT_SHA . && docker push x:$CI_COMMIT_SHA",
	)))
	n := notes(p, "PUSH_BEFORE_GATE")
	if !strings.Contains(got, "scan --ci") || len(n) != 1 || n[0].Line != 3 || !strings.Contains(n[0].Message, "docker push x:old") {
		t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
	}
}

// Batch 5 re-review: block-style !reference scripts are not commands, and jobs that may take a
// cache from their template or default: get CACHE_NOT_ADDED instead of a job cache.
func TestGitLabReReviewMutants(t *testing.T) {
	_, ns := jobs(t, lines(
		".tpl:",
		"  script:",
		"    - npm test",
		"blockref:",
		"  script: !reference",
		"    - .tpl",
		"    - script",
		"beforeref:",
		"  before_script: !reference",
		"    - .tpl",
		"    - script",
		"  script:",
		"    - npm test",
		"afterref:",
		"  script:",
		"    - npm test",
		"  after_script: !reference",
		"    - .tpl",
		"    - script",
	))
	if len(ns) != 1 {
		t.Fatalf("notes %+v", ns)
	}
	for _, s := range []string{"blockref (its script uses !reference)", "beforeref (its before_script cannot be read", "afterref (its after_script is a YAML alias or !reference"} {
		if !strings.Contains(ns[0].Message, s) {
			t.Errorf("the note does not say %q:\n%s", s, ns[0].Message)
		}
	}

	for name, job := range map[string][]string{
		"extends": {"build:", "  extends: .tpl", "  script:", "    - docker build -t web:1 ."},
		"inherit": {"build:", "  inherit:", "    default: false", "  script:", "    - docker build -t web:1 ."},
	} {
		p, got := result(t, npmRepo(lines(append([]string{".tpl:", "  image: node:22"}, job...)...)))
		if !strings.Contains(got, scanWeb) || strings.Contains(got, "key: shearbolt") || len(notes(p, "CACHE_NOT_ADDED")) != 1 {
			t.Errorf("%s: %s:\n%s\nnotes %v", name, file, got, noteCodes(p))
		}
	}
}

// A job with extends or inherit: and no own before_script is left out when a before_script of the
// file cannot be read, or when the top-level one builds (Batch 5 final review).
func TestGitLabUnseenBeforeScriptFinal(t *testing.T) {
	for name, ci := range map[string]string{
		"unreadable template": lines(
			".build-steps:",
			"  script:",
			"    - docker build -t x:$CI_COMMIT_SHA .",
			".base:",
			"  before_script:",
			"    - !reference [.build-steps, script]",
			"build:",
			"  extends: .base",
			"  script:",
			"    - docker push x:$CI_COMMIT_SHA",
		),
		"top-level build": lines(
			"before_script:",
			"  - docker build -t x:$CI_COMMIT_SHA .",
			".base:",
			"  image: docker:27",
			"build:",
			"  extends: .base",
			"  script:",
			"    - docker push x:$CI_COMMIT_SHA",
		),
	} {
		t.Run(name, func(t *testing.T) {
			p, got := result(t, npmRepo(ci))
			n := notes(p, "UNSUPPORTED_EDIT")
			if got != "" || len(n) != 1 || !strings.Contains(n[0].Message, "build (") {
				t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
			}
		})
	}
}

// The hand-edit snippets in per-file notes (UNSUPPORTED_EDIT, INCLUDE_NOT_READ, a spec: header)
// and NO_GATE_PLACED pin the version like setup's own edits; a development build leaves them
// unpinned and says so with UNPINNED_VERSION.
func TestGitLabSnippetsPinTheVersion(t *testing.T) {
	const devInstall = "(curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | sh"
	for name, ci := range map[string]string{
		"include and extends": lines(
			"include:",
			"  - local: other.yml",
			".tpl:",
			"  image: node:22",
			"build:",
			"  extends: .tpl",
		),
		"spec header": lines(
			"spec:",
			"  inputs: {}",
			"---",
			"build:",
			"  script:",
			"    - docker build -t web:1 .",
		),
		"only NO_GATE_PLACED": lines(
			".tpl:",
			"  script:",
			"    - docker build -t web:1 .",
		),
	} {
		for _, v := range []string{"1.2.3", "dev"} {
			p := setup.Compute(setup.Options{Root: tree(t, npmRepo(ci)), Yes: true, Version: v})
			if p.Error != nil {
				t.Fatal(p.Error)
			}
			n := 0
			for _, note := range p.Notes {
				if !strings.Contains(note.Message, "tools.klar.ws/shearbolt/install.sh") {
					continue
				}
				n++
				if want := map[string]string{"1.2.3": install, "dev": devInstall}[v]; !strings.Contains(note.Message, "- "+want+"\n") {
					t.Errorf("%s, %s: %s note has no %q:\n%s", name, v, note.Code, want, note.Message)
				}
			}
			if n < 1 || (v == "dev") != (len(notes(p, "UNPINNED_VERSION")) == 1) {
				t.Errorf("%s, %s: %d notes with an install line; notes %v", name, v, n, noteCodes(p))
			}
		}
	}
}

// reading is the GitLab plugin, recording the jobs it reads with the classifier setup passes.
type reading struct {
	gitlab.Plugin
	jobs *[]plan.Job
}

func (p reading) Jobs(r plan.Repo, f string, src []byte, classify plan.Classifier) ([]plan.Job, []plan.Note, error) {
	js, ns, err := p.Plugin.Jobs(r, f, src, classify)
	*p.jobs = append(*p.jobs, js...)
	return js, ns, err
}

// classified returns the jobs setup reads from ci, classified by the core, by key.
func classified(t *testing.T, ci string) map[string]plan.Job {
	t.Helper()
	var js []plan.Job
	setup.ComputeWith(setup.Options{Root: tree(t, npmRepo(ci)), Yes: true}, reading{jobs: &js})
	out := map[string]plan.Job{}
	for _, j := range js {
		out[strings.TrimPrefix(j.ID, file+"#")] = j
	}
	return out
}

// P6: a job's PreludeStop is the line of the first command of the before_script it runs that can
// stop its shell early, and PreludeStopCmd its name; a job whose before_script is unknown takes
// the first in any before_script of the file.
func TestGitLabPreludeStop(t *testing.T) {
	type stop struct {
		line int
		cmd  string
	}
	for name, tc := range map[string]struct {
		ci   string
		want map[string]stop
	}{
		"own": {lines(
			"build:",
			"  before_script:",
			"    - echo hi",
			`    - if [ -n "$SKIP" ]; then exit 0; fi`,
			"    - exit 0",
			"  script:",
			"    - npm run build",
			"test:",
			"  script:",
			"    - npm test",
		), map[string]stop{"build": {4, "exit"}, "test": {}}},
		"default": {lines(
			"default:",
			"  before_script:",
			"    - cd app || exit 1",
			"    - exit 0",
			"build:",
			"  script:",
			"    - npm run build",
		), map[string]stop{"build": {4, "exit"}}},
		"top level, sourced": {lines(
			"before_script:",
			"  - source ./ci/missing.sh",
			"build:",
			"  script:",
			"    - npm run build",
		), map[string]stop{"build": {2, "source"}}},
		"unseen": {lines(
			".base:",
			"  image: node:22",
			"other:",
			"  before_script:",
			"    - exec ./run.sh",
			"  script:",
			"    - npm test",
			"build:",
			"  extends: .base",
			"  script:",
			"    - npm run build",
			"own:",
			"  extends: .base",
			"  before_script:",
			"    - echo own",
			"  script:",
			"    - npm run build",
		), map[string]stop{"other": {5, "exec"}, "build": {5, "exec"}, "own": {}}},
		"none": {lines(
			"default:",
			"  before_script:",
			"    - cd app || exit 1",
			"    - bash -c 'exit 0'",
			"build:",
			"  extends: .base",
			"  script:",
			"    - exit 0",
		), map[string]stop{"build": {}}},
	} {
		t.Run(name, func(t *testing.T) {
			js := classified(t, tc.ci)
			for key, want := range tc.want {
				j, ok := js[key]
				if !ok {
					t.Fatalf("no job %s in %v", key, js)
				}
				if got := (stop{j.PreludeStop, j.PreludeStopCmd}); got != want {
					t.Errorf("%s: prelude stop %+v, want %+v", key, got, want)
				}
			}
		})
	}
}

// P12 (design §3.5): an early stop in the script or in the before_script the job runs ends its
// shell before the gate, so the gate is skipped while later jobs and stages still run. A job
// whose gate checks anything other than images built after the stop is left unchanged.
func TestGitLabEarlyStop(t *testing.T) {
	const deploy = "    - ./deploy.sh"
	refused := func(t *testing.T, files map[string]string, job, cmd string, line int) {
		t.Helper()
		p, got := result(t, files)
		var n []plan.Note
		for _, note := range notes(p, "UNSUPPORTED_EDIT") {
			if strings.Contains(note.Message, "job "+job+" was left unchanged") {
				n = append(n, note)
			}
		}
		msg := fmt.Sprintf("the job can stop before the shearbolt gate (%s at line %d), which would skip its check of source while later jobs still run; move the early stop into the job's rules or condition, or add the gate by hand where it runs on every path", cmd, line)
		if got != "" || len(n) != 1 || !strings.Contains(n[0].Message, msg) || !strings.Contains(n[0].Message, "- "+install+"\n") || n[0].Line != line {
			t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
		}
	}
	t.Run("ES-5", func(t *testing.T) {
		refused(t, npmRepo(lines(
			"build:",
			"  script:",
			`    - if [ -z "$CHANGED" ]; then exit 0; fi`,
			"    - npm ci",
			"    - npm run build",
			deploy,
		)), "build", "exit", 3)
	})
	t.Run("own before_script", func(t *testing.T) {
		refused(t, npmRepo(lines(
			"build:",
			"  before_script:",
			"    - echo start",
			`    - if [ -z "$CHANGED" ]; then exit 0; fi`,
			"  script:",
			"    - npm ci",
			"    - npm run build",
			deploy,
		)), "build", "exit", 4)
	})
	t.Run("unseen before_script", func(t *testing.T) {
		refused(t, npmRepo(lines(
			".base:",
			"  image: node:22",
			"other:",
			"  before_script:",
			`    - if [ -z "$CHANGED" ]; then exit 0; fi`,
			"  script:",
			"    - npm test",
			"build:",
			"  extends: .base",
			"  script:",
			"    - npm ci",
			"    - npm run build",
			deploy,
		)), "build", "exit", 5)
	})
	t.Run("sourced in before_script", func(t *testing.T) {
		files := npmRepo(lines(
			"default:",
			"  before_script:",
			"    - . ./ci/skip.sh",
			"build:",
			"  script:",
			"    - npm ci",
			"    - npm run build",
			deploy,
		))
		files["ci/skip.sh"] = "exit 0\n"
		refused(t, files, "build", "source", 3)
	})

	// A root project whose gate scans only the image built after the stop in before_script: the
	// gate follows the build as usual.
	p, got, again := rerun(t, map[string]string{"Dockerfile": "FROM alpine\n", file: lines(
		"deploy:",
		"  before_script:",
		`    - if [ -z "$CHANGED" ]; then exit 0; fi`,
		"  script:",
		"    - docker build -t web:$CI_COMMIT_SHA .",
		"    - docker push web:$CI_COMMIT_SHA",
	)})
	if !strings.Contains(got, "    - "+install+"\n    - "+scanWeb+"\n    - docker push web:$CI_COMMIT_SHA\n") || len(notes(p, "UNSUPPORTED_EDIT")) != 0 || len(notes(again, "UNSUPPORTED_EDIT")) != 0 {
		t.Errorf("%s:\n%s\nnotes %v\nsecond run %v", file, got, noteCodes(p), noteCodes(again))
	}
}

// P12 on a re-run: an exit added before a gate already in the job (here setup's own from a first
// run) fails the job on the exit's line, and nothing changes.
func TestGitLabEarlyStopRerun(t *testing.T) {
	root := tree(t, npmRepo(lines(
		"build:",
		"  script:",
		"    - npm ci",
		"    - npm run build",
		"    - ./deploy.sh",
	)))
	o := setup.Options{Root: root, Yes: true, Version: "1.2.3"}
	p := setup.Compute(o)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	if err := setup.Write(root, p); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		t.Fatal(err)
	}
	before, after, ok := strings.Cut(string(src), "  script:\n")
	if !ok || !strings.Contains(after, "scan --ci") {
		t.Fatalf("%s after the first run:\n%s", file, src)
	}
	line := strings.Count(before, "\n") + 2
	stopped := before + "  script:\n" + `    - if [ -z "$CHANGED" ]; then exit 0; fi` + "\n" + after
	if err := os.WriteFile(filepath.Join(root, file), []byte(stopped), 0o644); err != nil {
		t.Fatal(err)
	}
	again := setup.Compute(o)
	if again.Error != nil {
		t.Fatal(again.Error)
	}
	n := notes(again, "UNSUPPORTED_EDIT")
	msg := fmt.Sprintf("job build was left unchanged: the job can stop before the shearbolt gate (exit at line %d), which would skip its check of source while later jobs still run", line)
	if len(again.Changes) != 0 || len(n) != 1 || !strings.Contains(n[0].Message, msg) || n[0].Line != line {
		t.Errorf("changes %+v\nnotes %v\nwant %q", again.Changes, noteCodes(again), msg)
	}
}
