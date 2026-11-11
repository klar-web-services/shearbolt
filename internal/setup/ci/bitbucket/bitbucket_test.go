package bitbucket_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/ci/bitbucket"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

const file = "bitbucket-pipelines.yml"

const (
	install = "(curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | SHEARBOLT_VERSION=1.2.3 sh"
	scanWeb = `SHEARBOLT_IMAGE=web:shearbolt-$BITBUCKET_COMMIT "$HOME/.local/bin/shearbolt" scan --ci --config "$BITBUCKET_CLONE_DIR/shearbolt.yaml"`
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

func npmRepo(pipelines string) map[string]string {
	return map[string]string{
		"package.json":      `{"name":"web"}`,
		"package-lock.json": "{}",
		"Dockerfile":        "FROM node:22\n",
		file:                pipelines,
	}
}

// result runs setup on files and returns the plan and bitbucket-pipelines.yml afterwards (""
// when unchanged). The result must be valid YAML, and a second run on it must change nothing.
func result(t *testing.T, files map[string]string) (*plan.Plan, string) {
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
	return p, out
}

func lines(s ...string) string { return strings.Join(s, "\n") + "\n" }

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
	js, ns, err := bitbucket.Plugin{}.Jobs(plan.Repo{Root: t.TempDir()}, file, []byte(src), func(shell.Command, plan.Context) plan.Classified { return plan.Classified{} })
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

func TestBitbucketFiles(t *testing.T) {
	root := tree(t, map[string]string{file: "pipelines: {}\n", "sub/" + file: "x: 1\n"})
	if got := (bitbucket.Plugin{}).Files(plan.Repo{Root: root}); !slices.Equal(got, []string{file}) {
		t.Errorf("Files = %q", got)
	}
	if got := (bitbucket.Plugin{}).Files(plan.Repo{Root: tree(t, map[string]string{file + "/x": "x"})}); got != nil {
		t.Errorf("a directory is not a pipelines file: %q", got)
	}
}

// Every step under default, branches, tags, pull-requests and custom is a job, inside parallel
// (a list or steps:) and stage steps too. Labels are the step name and its path.
func TestBitbucketJobs(t *testing.T) {
	js, ns := jobs(t, lines(
		"pipelines:",
		"  default:",
		"    - step:",
		"        name: Test",
		"        script:",
		"          - npm test",
		"    - parallel:",
		"        - step:",
		"            script:",
		"              - echo a",
		"        - step:",
		"            script:",
		"              - echo b",
		"  branches:",
		"    'release/*':",
		"      - parallel:",
		"          fail-fast: true",
		"          steps:",
		"            - step:",
		"                script:",
		"                  - echo c",
		"      - stage:",
		"          name: Deploy",
		"          steps:",
		"            - step:",
		"                name: Ship",
		"                script:",
		"                  - pipe: atlassian/aws-ecs-deploy:1.0.0",
		"  tags:",
		"    'v*':",
		"      - step:",
		"          script:",
		"            - echo d",
		"  pull-requests:",
		"    '**':",
		"      - step:",
		"          script:",
		"            - echo e",
		"  custom:",
		"    manual:",
		"      - variables:",
		"          - name: X",
		"      - step:",
		"          script:",
		"            - echo f",
	))
	want := []string{
		file + "#default/0", file + "#default/1/0", file + "#default/1/1",
		file + "#branches/release/*/0/0", file + "#branches/release/*/1/0",
		file + "#tags/v*/0", file + "#pull-requests/**/0", file + "#custom/manual/1",
	}
	if got := ids(js); !slices.Equal(got, want) {
		t.Fatalf("jobs = %q, want %q", got, want)
	}
	if js[0].Label != "Test (default/0)" || js[1].Label != "default/1/0" {
		t.Errorf("labels %q, %q", js[0].Label, js[1].Label)
	}
	if st := js[4].Steps[0]; st.Uses != "atlassian/aws-ecs-deploy:1.0.0" || !slices.Equal(st.Push, []int{-1}) {
		t.Errorf("a pipe is a step that pushes: %+v", st)
	}
	if len(ns) != 0 {
		t.Errorf("notes %+v", ns)
	}
}

// A step defined once with an anchor and used in two pipelines is one job, edited at the anchor.
func TestBitbucketAnchoredStepEditedOnce(t *testing.T) {
	src := lines(
		"definitions:",
		"  steps:",
		"    - step: &build",
		"        name: Build",
		"        script:",
		"          - docker build -t web:1 .",
		"          - docker push web:1",
		"pipelines:",
		"  default:",
		"    - step: *build",
		"  branches:",
		"    main:",
		"      - step: *build",
	)
	if js, _ := jobs(t, src); !slices.Equal(ids(js), []string{file + "#default/0"}) {
		t.Fatalf("jobs = %q", ids(js))
	}
	_, got := result(t, npmRepo(src))
	want(t, got, lines(
		"definitions:",
		"  steps:",
		"    - step: &build",
		"        name: Build",
		"        caches:",
		"          - shearbolt",
		"        script:",
		"          - docker build -t web:1 -t web:shearbolt-$BITBUCKET_COMMIT .",
		"          - "+install,
		"          - "+scanWeb,
		"          - docker push web:1",
		"  caches:",
		"    shearbolt: ~/.cache/shearbolt",
		"pipelines:",
		"  default:",
		"    - step: *build",
		"  branches:",
		"    main:",
		"      - step: *build",
	))
}

// A parallel step is found and edited; an existing definitions.caches gets the entry appended,
// and a step whose script is its first key gets caches before it.
func TestBitbucketParallelAndExistingCaches(t *testing.T) {
	_, got := result(t, npmRepo(lines(
		"definitions:",
		"  caches:",
		"    npm: ~/.npm",
		"",
		"pipelines:",
		"  default:",
		"    - parallel:",
		"        - step:",
		"            script:",
		"              - npm test",
		"        - step:",
		"            script:",
		"              - docker build -t web:1 .",
		"              - docker push web:1",
	)))
	want(t, got, lines(
		"definitions:",
		"  caches:",
		"    npm: ~/.npm",
		"    shearbolt: ~/.cache/shearbolt",
		"",
		"pipelines:",
		"  default:",
		"    - parallel:",
		"        - step:",
		"            script:",
		"              - npm test",
		"        - step:",
		"            caches:",
		"              - shearbolt",
		"            script:",
		"              - docker build -t web:1 -t web:shearbolt-$BITBUCKET_COMMIT .",
		"              - "+install,
		"              - "+scanWeb,
		"              - docker push web:1",
	))
}

// Without definitions, they are created before pipelines (and its head comment).
func TestBitbucketCreatesDefinitions(t *testing.T) {
	_, got := result(t, npmRepo(lines(
		"image: node:22",
		"# CI",
		"pipelines:",
		"  default:",
		"    - step:",
		"        script:",
		"          - docker build -t web:1 .",
	)))
	want(t, got, lines(
		"image: node:22",
		"definitions:",
		"  caches:",
		"    shearbolt: ~/.cache/shearbolt",
		"",
		"# CI",
		"pipelines:",
		"  default:",
		"    - step:",
		"        caches:",
		"          - shearbolt",
		"        script:",
		"          - docker build -t web:1 -t web:shearbolt-$BITBUCKET_COMMIT .",
		"          - "+install,
		"          - "+scanWeb,
	))
}

// Steps with a merge key are not jobs; one note lists them all, with the snippet.
func TestBitbucketMergeKeySteps(t *testing.T) {
	src := lines(
		"definitions:",
		"  steps:",
		"    - step: &defaults",
		"        image: node:22",
		"pipelines:",
		"  default:",
		"    - step:",
		"        <<: *defaults",
		"        name: Build",
		"        script:",
		"          - docker build -t web:1 .",
		"    - step:",
		"        <<: *defaults",
		"        name: Push",
		"        script:",
		"          - docker push web:1",
		"    - step:",
		"        name: Test",
		"        script:",
		"          - npm test",
	)
	js, ns := jobs(t, src)
	if !slices.Equal(ids(js), []string{file + "#default/2"}) {
		t.Fatalf("jobs = %q", ids(js))
	}
	if len(ns) != 1 || ns[0].Code != "UNSUPPORTED_EDIT" || !strings.Contains(ns[0].Message, "Build (default/0)") ||
		!strings.Contains(ns[0].Message, "Push (default/1)") || !strings.Contains(ns[0].Message, `"$HOME/.local/bin/shearbolt" scan --ci`) {
		t.Errorf("notes %+v", ns)
	}

	// When every step uses a merge key, the config is still written but no gate is placed.
	p, got := result(t, npmRepo(lines(
		"definitions:",
		"  steps:",
		"    - step: &defaults",
		"        image: node:22",
		"pipelines:",
		"  default:",
		"    - step:",
		"        <<: *defaults",
		"        script:",
		"          - docker build -t web:1 .",
		"          - docker push web:1",
	)))
	if got != "" {
		t.Errorf("%s changed:\n%s", file, got)
	}
	if n := notes(p, "NO_GATE_PLACED"); len(n) != 1 || !strings.Contains(n[0].Message, "          - "+install) {
		t.Errorf("notes %v", noteCodes(p))
	}
	if !slices.ContainsFunc(p.Changes, func(c plan.FileChange) bool { return c.Path == "shearbolt.yaml" }) {
		t.Error("the config is not written")
	}
	if len(p.Questions) != 0 {
		t.Errorf("questions %+v", p.Questions)
	}
}

// A flow-style caches list keeps the script edit but drops the cache edit.
func TestBitbucketFlowCaches(t *testing.T) {
	p, got := result(t, npmRepo(lines(
		"pipelines:",
		"  default:",
		"    - step:",
		"        caches: [docker]",
		"        script:",
		"          - docker build -t web:1 .",
	)))
	want(t, got, lines(
		"pipelines:",
		"  default:",
		"    - step:",
		"        caches: [docker]",
		"        script:",
		"          - docker build -t web:1 -t web:shearbolt-$BITBUCKET_COMMIT .",
		"          - "+install,
		"          - "+scanWeb,
	))
	if n := notes(p, "CACHE_NOT_ADDED"); len(n) != 1 || !strings.Contains(n[0].Message, "- shearbolt") || !strings.Contains(n[0].Message, "shearbolt: ~/.cache/shearbolt") {
		t.Errorf("notes %v", noteCodes(p))
	}
}

// A push in after-script runs even when the gate fails; it is reported, not edited.
func TestBitbucketAfterScriptPush(t *testing.T) {
	p, got := result(t, npmRepo(lines(
		"pipelines:",
		"  default:",
		"    - step:",
		"        script:",
		"          - docker build -t web:1 .",
		"        after-script:",
		"          - echo done",
		"          - docker push web:1",
		"    - step:",
		"        script:",
		"          - npm test",
		"        after-script:",
		"          - pipe: atlassian/slack-notify:2.0.0",
	)))
	if !strings.Contains(got, "          - docker push web:1\n") {
		t.Errorf("after-script changed:\n%s", got)
	}
	n := notes(p, "PUSH_IN_AFTER_SCRIPT")
	if len(n) != 2 || n[0].Line != 8 || n[0].File != file || !strings.Contains(n[0].Message, "docker push web:1") {
		t.Errorf("notes %+v", n)
	}
}

func TestBitbucketSnippet(t *testing.T) {
	got := bitbucket.Plugin{}.Snippet(plan.Repo{}, file, plan.Gate{Project: plan.Project{Dir: "api"}, Config: "api/shearbolt.yaml", Version: "1.2.3"})
	exp := "            - " + install + "\n" + `            - '"$HOME/.local/bin/shearbolt" scan --ci --config "$BITBUCKET_CLONE_DIR/api/shearbolt.yaml"'`
	if got != exp {
		t.Errorf("snippet:\n%s\nwant:\n%s", got, exp)
	}
}

// Batch 5 review, minor: a step on a Windows runner, or with an after-script setup cannot read,
// is left out; an imported pipeline gets INCLUDE_NOT_READ.
func TestBitbucketMoreSkips(t *testing.T) {
	js, ns := jobs(t, lines(
		"pipelines:",
		"  default:",
		"    - step:",
		"        name: Win",
		"        runs-on:",
		"          - self.hosted",
		"          - windows",
		"        script:",
		"          - docker build -t x:1 . ; docker push x:1",
		"    - step:",
		"        name: Flow",
		"        script:",
		"          - npm test",
		"        after-script: [echo done]",
		"    - step:",
		"        name: Fine",
		"        script:",
		"          - npm test",
		"  custom:",
		"    shared:",
		"      import: shared-pipelines:master:build",
	))
	if !slices.Equal(ids(js), []string{file + "#default/2"}) {
		t.Errorf("jobs %q", ids(js))
	}
	var unsupported, include []plan.Note
	for _, n := range ns {
		switch n.Code {
		case "UNSUPPORTED_EDIT":
			unsupported = append(unsupported, n)
		case "INCLUDE_NOT_READ":
			include = append(include, n)
		}
	}
	if len(unsupported) != 1 || !strings.Contains(unsupported[0].Message, "Win (default/0) (it runs on a Windows runner") || !strings.Contains(unsupported[0].Message, "Flow (default/1) (its after-script") {
		t.Errorf("notes %+v", ns)
	}
	if len(include) != 1 || !strings.Contains(include[0].Message, "import:") || !strings.Contains(include[0].Message, "scan --ci") {
		t.Errorf("notes %+v", ns)
	}
}

// Bitbucket steps left out for their layout, and a --push build in after-script (Batch 5 review:
// paths without a test).
func TestBitbucketSkipPathsAndAfterScriptBuildPush(t *testing.T) {
	_, ns := jobs(t, lines(
		"definitions:",
		"  scripts:",
		"    - &s",
		"      - npm test",
		"pipelines:",
		"  default:",
		"    - step: {name: Flow, script: [npm test]}",
		"    - step:",
		"        name: Shared",
		"        script: *s",
		"    - step:",
		"        name: Odd",
		"        script:",
		"          - {run: npm test}",
	))
	if len(ns) != 1 {
		t.Fatalf("notes %+v", ns)
	}
	for _, s := range []string{"Flow (default/0) (flow-style step)", "Shared (default/1) (its script is shared through a YAML anchor)", "Odd (default/2) (script item 1 is not a plain string)"} {
		if !strings.Contains(ns[0].Message, s) {
			t.Errorf("the note does not say %q:\n%s", s, ns[0].Message)
		}
	}

	p, _ := result(t, npmRepo(lines(
		"pipelines:",
		"  default:",
		"    - step:",
		"        script:",
		"          - docker build -t web:1 .",
		"        after-script:",
		"          - docker buildx build --push -t web:2 .",
	)))
	if n := notes(p, "PUSH_IN_AFTER_SCRIPT"); len(n) != 1 || !strings.Contains(n[0].Message, "--push") {
		t.Errorf("notes %v", noteCodes(p))
	}
}

// runs-on labels are compared case-insensitively (Batch 5 re-review).
func TestBitbucketWindowsRunnerAnyCase(t *testing.T) {
	js, ns := jobs(t, lines(
		"pipelines:",
		"  default:",
		"    - step:",
		"        runs-on: Windows",
		"        script:",
		"          - npm test",
	))
	if len(js) != 0 || len(ns) != 1 || !strings.Contains(ns[0].Message, "Windows runner") {
		t.Errorf("jobs %q, notes %+v", ids(js), ns)
	}
}

// The hand-edit snippets in per-file notes (UNSUPPORTED_EDIT, INCLUDE_NOT_READ) and
// NO_GATE_PLACED pin the version like setup's own edits; a development build leaves them
// unpinned and says so with UNPINNED_VERSION.
func TestBitbucketSnippetsPinTheVersion(t *testing.T) {
	const devInstall = "(curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | sh"
	src := lines(
		"definitions:",
		"  steps:",
		"    - step: &defaults",
		"        image: node:22",
		"pipelines:",
		"  default:",
		"    - step:",
		"        <<: *defaults",
		"        script:",
		"          - docker build -t web:1 .",
		"  custom:",
		"    shared:",
		"      import: shared-pipelines:main:build",
	)
	for _, v := range []string{"1.2.3", "dev"} {
		p := setup.Compute(setup.Options{Root: tree(t, npmRepo(src)), Yes: true, Version: v})
		if p.Error != nil {
			t.Fatal(p.Error)
		}
		codes := map[string]bool{}
		for _, note := range p.Notes {
			if !strings.Contains(note.Message, "tools.klar.ws/shearbolt/install.sh") {
				continue
			}
			codes[note.Code] = true
			if want := map[string]string{"1.2.3": install, "dev": devInstall}[v]; !strings.Contains(note.Message, "- "+want+"\n") {
				t.Errorf("%s: %s note has no %q:\n%s", v, note.Code, want, note.Message)
			}
		}
		if !codes["UNSUPPORTED_EDIT"] || !codes["INCLUDE_NOT_READ"] || !codes["NO_GATE_PLACED"] || (v == "dev") != (len(notes(p, "UNPINNED_VERSION")) == 1) {
			t.Errorf("%s: notes %v", v, noteCodes(p))
		}
	}
}

// P12 (design §3.5): an exit before the gate ends the step's script successfully and skips the
// gate, while later steps still run; a step whose gate checks the source is left unchanged.
func TestBitbucketEarlyStop(t *testing.T) {
	p, got := result(t, npmRepo(lines(
		"pipelines:",
		"  default:",
		"    - step:",
		"        name: Build",
		"        script:",
		`          - if [ -z "$CHANGED" ]; then exit 0; fi`,
		"          - npm ci",
		"          - npm run build",
		"          - ./deploy.sh",
	)))
	n := notes(p, "UNSUPPORTED_EDIT")
	msg := "the job can stop before the shearbolt gate (exit at line 6), which would skip its check of source while later jobs still run"
	if got != "" || len(n) != 1 || !strings.Contains(n[0].Message, msg) || !strings.Contains(n[0].Message, "- "+install+"\n") || n[0].Line != 6 {
		t.Errorf("%s:\n%s\nnotes %v", file, got, noteCodes(p))
	}
}
