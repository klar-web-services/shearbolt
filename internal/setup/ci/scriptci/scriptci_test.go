package scriptci_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/ci/scriptci"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

// listCI is a minimal script-list CI built from scriptci alone: ci.yml maps job names to
// mappings with a block script sequence. It is named "gitlab", so scan tags use $CI_COMMIT_SHA,
// and its clone directory is $CI_PROJECT_DIR.
type listCI struct {
	failExtra bool // the jobs' Extra hook fails
}

const file = "ci.yml"

func (listCI) Name() string { return "gitlab" }

func (listCI) Files(r plan.Repo) []string {
	if r.Exists(file) {
		return []string{file}
	}
	return nil
}

func scripts(d *yamledit.Doc) []*scriptci.Script {
	var out []*scriptci.Script
	root := d.Root()
	for i := 0; root != nil && i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		seq := d.Get(root.Content[i+1], "script")
		if seq == nil || seq.Kind != yaml.SequenceNode {
			continue
		}
		s, err := scriptci.NewScript(d, file, file+"#"+key, seq, shell.State{Dir: "."}, true)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (listCI) Jobs(r plan.Repo, f string, src []byte, classify plan.Classifier) ([]plan.Job, []plan.Note, error) {
	d, err := yamledit.Parse(src)
	if err != nil {
		return nil, nil, err
	}
	var jobs []plan.Job
	for _, s := range scripts(d) {
		jobs = append(jobs, plan.Job{ID: s.Job, Label: s.Job, Steps: s.Steps(classify, nil)})
	}
	return jobs, nil, nil
}

func (c listCI) Edit(r plan.Repo, f string, src []byte, jobs []plan.Job, gates []plan.Gate, preps map[string][]plan.Prep) (plan.Edited, error) {
	d, err := yamledit.Parse(src)
	if err != nil {
		return plan.Edited{}, err
	}
	var js []scriptci.Job
	for _, s := range scripts(d) {
		for _, pj := range jobs {
			if pj.ID == s.Job {
				j := scriptci.Job{Plan: pj, Script: s, Key: strings.TrimPrefix(s.Job, file+"#")}
				if c.failExtra {
					j.Extra = func() ([]plan.Note, error) { return nil, errors.New("the cache edit failed") }
				}
				js = append(js, j)
			}
		}
	}
	return scriptci.Edit(d, src, f, js, gates, preps, scriptci.Options{
		CloneDir: "$CI_PROJECT_DIR",
		Snippet:  func(g plan.Gate) string { return c.Snippet(r, f, g) },
	})
}

func (listCI) Snippet(r plan.Repo, f string, g plan.Gate) string {
	return scriptci.Snippet(g, "$CI_PROJECT_DIR", 4)
}

// tree writes files under a new temp dir.
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

// npmRepo is an npm project at dir (with a Dockerfile) plus ci.yml.
func npmRepo(dir, ci string) map[string]string {
	return map[string]string{
		path(dir, "package.json"):      `{"name":"web"}`,
		path(dir, "package-lock.json"): "{}",
		path(dir, "Dockerfile"):        "FROM node:22\n",
		file:                           ci,
	}
}

func path(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

// run runs setup with version on root and returns the plan and ci.yml afterwards ("" when it
// is unchanged). The result must be valid YAML.
func run(t *testing.T, root, version string) (*plan.Plan, string) {
	t.Helper()
	p := setup.ComputeWith(setup.Options{Root: root, Yes: true, Version: version}, listCI{})
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
	return p, out
}

// result runs setup on files, writes the result, and checks that a second run changes nothing.
func result(t *testing.T, files map[string]string, version string) (*plan.Plan, string) {
	t.Helper()
	p, out, _ := rerun(t, files, version)
	return p, out
}

// rerun is result that also returns the second run's plan.
func rerun(t *testing.T, files map[string]string, version string) (*plan.Plan, string, *plan.Plan) {
	t.Helper()
	root := tree(t, files)
	p, out := run(t, root, version)
	if err := setup.Write(root, p); err != nil {
		t.Fatal(err)
	}
	again, _ := run(t, root, version)
	for _, c := range again.Changes {
		t.Errorf("a second run changes %s:\n%s", c.Path, c.Diff)
	}
	return p, out, again
}

func lines(s ...string) string { return strings.Join(s, "\n") + "\n" }

func want(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("ci.yml:\n%s\nwant:\n%s", got, want)
	}
}

func hasNote(p *plan.Plan, code string) bool {
	return slices.ContainsFunc(p.Notes, func(n plan.Note) bool { return n.Code == code })
}

func noteCodes(p *plan.Plan) []string {
	var out []string
	for _, n := range p.Notes {
		out = append(out, n.Code+": "+n.Message)
	}
	return out
}

const (
	install    = "(curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | SHEARBOLT_VERSION=1.2.3 sh"
	installDev = "(curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | sh"
)

// Test 1: a combined line is split at the gate; the push follows it as its own item.
func TestScriptCombinedLine(t *testing.T) {
	p, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - docker build -t $I:$CI_COMMIT_SHA . && docker push $I:$CI_COMMIT_SHA",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - docker build -t $I:$CI_COMMIT_SHA -t $I:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=$I:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - docker push $I:$CI_COMMIT_SHA",
	))
	if !hasNote(p, "TAG_ADDED") || hasNote(p, "UNPINNED_VERSION") {
		t.Errorf("notes = %v", noteCodes(p))
	}
}

// Test 2: a multi-line block is split at the line boundary into block-free items.
func TestScriptBlockSplit(t *testing.T) {
	_, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - |",
		"      docker build -t x:$C .",
		"      docker push x:$C",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - docker build -t x:$C -t x:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - docker push x:$C",
	))

	// A longer remainder stays one block item; a multi-line first part stays a block.
	_, got = result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - |",
		"      export X=1",
		"      docker build -t x:$C .",
		"      docker push x:$C",
		"      echo pushed",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - |",
		"      export X=1",
		"      docker build -t x:$C -t x:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - |",
		"      docker push x:$C",
		"      echo pushed",
	))
}

// Test 3: builds that cannot be followed by a gate leave the job unchanged.
func TestScriptUnsupported(t *testing.T) {
	for name, item := range map[string][]string{
		"conditional": {`    - if [ -n "$X" ]; then docker build -t x . ; fi`},
		"piped":       {"    - docker build -t x . | tee log"},
		"continued":   {"    - |", "      docker build -t x . && \\", "      docker push x"},
		"or":          {"    - docker build -t x . || echo failed"},
	} {
		t.Run(name, func(t *testing.T) {
			p, got := result(t, npmRepo(".", lines(append([]string{"build:", "  script:"}, item...)...)), "1.2.3")
			if got != "" {
				t.Errorf("ci.yml changed:\n%s", got)
			}
			i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
			if i < 0 {
				t.Fatalf("notes = %v, want UNSUPPORTED_EDIT", noteCodes(p))
			}
			if n := p.Notes[i]; !strings.Contains(n.Message, `"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`) || n.File != file || n.Line == 0 {
				t.Errorf("the note has no snippet or location: %+v", n)
			}
			if hasNote(p, "TAG_ADDED") {
				t.Errorf("a failed job adds no tag: %v", noteCodes(p))
			}
		})
	}

	// A continuation inside the build itself stays splittable.
	_, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - |",
		"      docker build \\",
		"        -t x:$C . && docker push x:$C",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - |",
		"      docker build \\",
		"        -t x:$C -t x:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - docker push x:$C",
	))
}

// Test 4: a development build installs the latest release and says so.
func TestScriptDevVersion(t *testing.T) {
	p, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
	)), "dev")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - docker build -t web:1 -t web:shearbolt-$CI_COMMIT_SHA .",
		"    - "+installDev,
		`    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
	))
	if !hasNote(p, "UNPINNED_VERSION") {
		t.Errorf("notes = %v, want UNPINNED_VERSION", noteCodes(p))
	}
}

// Test 5: an existing gate gets only its pin updated by a release, and nothing from dev.
func TestScriptExistingGatePin(t *testing.T) {
	ci := lines(
		"build:",
		"  script:",
		"    - docker build -t web:1 -t web:shearbolt-$CI_COMMIT_SHA .",
		"    - "+strings.Replace(install, "1.2.3", "1.0.0", 1),
		`    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - docker push web:1",
	)
	files := npmRepo(".", ci)
	_, got := result(t, files, "1.2.3")
	want(t, got, strings.Replace(ci, "SHEARBOLT_VERSION=1.0.0", "SHEARBOLT_VERSION=1.2.3", 1))

	p, got := result(t, files, "dev")
	if got != "" || hasNote(p, "UNPINNED_VERSION") {
		t.Errorf("a dev re-run changes the job:\n%s\n%v", got, noteCodes(p))
	}
}

// Test 6: cd carries across items, and a gate after a cd names its config by the clone dir.
func TestScriptParseItemsCarriesState(t *testing.T) {
	cmds, end := scriptci.ParseItems([]string{"cd services/api", "export IMAGE=x", "docker build -t $IMAGE:$C ."}, shell.State{Dir: "."})
	if len(cmds) != 3 || len(cmds[2]) != 1 {
		t.Fatalf("commands: %+v", cmds)
	}
	if b := cmds[2][0].Before; b.Dir != "services/api" || !slices.Contains(b.Env, "IMAGE") {
		t.Errorf("third item starts in %+v", b)
	}
	if end.Dir != "services/api" {
		t.Errorf("end state %+v", end)
	}

	_, got := result(t, npmRepo("services/api", lines(
		"build:",
		"  script:",
		"    - cd services/api",
		"    - docker build -t x:$C .",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - cd services/api",
		"    - docker build -t x:$C -t x:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/services/api/shearbolt.yaml"`,
	))

	// The root project after a cd also names its config.
	files := npmRepo(".", lines(
		"build:",
		"  script:",
		"    - cd docker",
		"    - docker build -f ../Dockerfile -t x:$C ..",
	))
	_, got = result(t, files, "1.2.3")
	if !strings.Contains(got, `scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`) {
		t.Errorf("ci.yml:\n%s", got)
	}
}

// Test 7: --push becomes --load, and the original tags are pushed after the gate.
func TestScriptPushBuild(t *testing.T) {
	p, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - docker buildx build --push -t a:$CI_COMMIT_SHA -t a:latest .",
		"    - echo built",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - docker buildx build --load -t a:$CI_COMMIT_SHA -t a:shearbolt-$CI_COMMIT_SHA -t a:latest .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=a:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - docker push a:$CI_COMMIT_SHA",
		"    - docker push a:latest",
		"    - echo built",
	))
	if !hasNote(p, "ATTESTATIONS_DROPPED") {
		t.Errorf("notes = %v", noteCodes(p))
	}
	if strings.Contains(got, "docker push a:shearbolt") {
		t.Error("the scan tag is pushed")
	}

	// The rest of a --push build's item moves after the gate with its pushes, and an earlier
	// --push build's pushes come after the gate too.
	_, got = result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - docker buildx build --push -t a:1 . && echo pushed a",
		"    - docker build -f Dockerfile -t b:1 .",
		"    - docker push b:1",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - docker buildx build --load -t a:1 -t a:shearbolt-$CI_COMMIT_SHA .",
		"    - docker build -f Dockerfile -t b:1 -t b:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=a:shearbolt-$CI_COMMIT_SHA SHEARBOLT_IMAGE_B=b:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - docker push a:1",
		"    - echo pushed a",
		"    - docker push b:1",
	))

	// Tags setup cannot read fail the job instead of dropping the push.
	p, got = result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - docker buildx build --push $TAG_ARGS .",
	)), "1.2.3")
	if got != "" || !hasNote(p, "UNSUPPORTED_EDIT") {
		t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
	}
}

// Test 8: items are rendered with yamledit.Scalar and read back as the intended commands.
func TestScriptQuoting(t *testing.T) {
	files := npmRepo(".", lines(
		"build:",
		"  script:",
		"    - npm ci",
		"    - npm run build  # the bundle",
		"    - npx vercel deploy --prod",
	))
	_, got := result(t, files, "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - npm ci",
		"    - npm run build  # the bundle",
		"    - "+install,
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`,
		"    - npx vercel deploy --prod",
	))
	var v map[string]map[string][]string
	if err := yaml.Unmarshal([]byte(got), &v); err != nil {
		t.Fatal(err)
	}
	items := v["build"]["script"]
	if len(items) != 5 || items[2] != install || items[3] != `"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"` {
		t.Errorf("items read back as %q", items)
	}
}

// A trailing comment on a split item stays on its first part; P1 splits before an unrecognized
// deploy too.
func TestScriptSplitKeepsComment(t *testing.T) {
	_, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    # build the image",
		"    - docker build -t web:1 . && ./deploy.sh web:1  # then ship it",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    # build the image",
		"    - docker build -t web:1 -t web:shearbolt-$CI_COMMIT_SHA .  # then ship it",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - ./deploy.sh web:1",
	))
}

// The Rust rewrite and its install item, before the first build that needs it.
func TestScriptRust(t *testing.T) {
	files := map[string]string{
		"Cargo.toml":  "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"Cargo.lock":  "version = 3\n",
		"src/main.rs": "fn main() {}\n",
		file: lines(
			"build:",
			"  script:",
			"    - rustup show",
			"    # the release binary",
			"    - cargo build --release --locked && gh release upload v1 target/release/demo",
		),
	}
	_, got := result(t, files, "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - rustup show",
		"    - cargo install cargo-auditable --locked",
		"    # the release binary",
		"    - cargo auditable build --release --locked",
		"    - "+install,
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`,
		"    - gh release upload v1 target/release/demo",
	))

	// An install already in an earlier item is not added again.
	files[file] = lines(
		"build:",
		"  script:",
		"    - cargo install cargo-auditable --locked",
		"    - cargo build --release",
	)
	_, got = result(t, files, "1.2.3")
	if strings.Count(got, "cargo-auditable") != 1 || !strings.Contains(got, "cargo auditable build --release") {
		t.Errorf("ci.yml:\n%s", got)
	}
}

// A push before the first build: the gate goes before the build, and the push is reported.
// A job without builds gets the gate before its first item.
func TestScriptBeforeGates(t *testing.T) {
	_, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - npm ci",
		"    - docker buildx build --platform linux/amd64,linux/arm64 --push -t web:1 .",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - npm ci",
		"    - "+install,
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`,
		"    - docker buildx build --platform linux/amd64,linux/arm64 --push -t web:1 .",
	))

	_, got = result(t, npmRepo(".", lines(
		"test:",
		"  script:",
		"    # tests",
		"    - npm test",
	)), "1.2.3")
	want(t, got, lines(
		"test:",
		"  script:",
		"    - "+install,
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`,
		"    # tests",
		"    - npm test",
	))
}

// An existing scan is re-rendered with the current image variables; arguments setup does not
// manage are kept, and an install is added when none runs before it.
func TestScriptExistingGateUpdated(t *testing.T) {
	_, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - docker build -t web:1 .",
		`    - SHEARBOLT_IMAGE=old:tag "$HOME/.local/bin/shearbolt" scan --output-dir out`,
		"    - docker push web:1",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - docker build -t web:1 -t web:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml" --output-dir out`,
		"    - docker push web:1",
	))
}

// Each job is edited completely or not at all: a job whose second build cannot be followed
// keeps its first build's tag too, while another job is still edited.
func TestScriptJobAtomicity(t *testing.T) {
	ci := lines(
		"a:",
		"  script:",
		"    - docker build -t web:1 .",
		"    - docker build -f Dockerfile -t worker:1 . | tee log",
		"b:",
		"  script:",
		"    - docker build -t web:2 .",
	)
	p, got := result(t, npmRepo(".", ci), "1.2.3")
	want(t, got, lines(
		"a:",
		"  script:",
		"    - docker build -t web:1 .",
		"    - docker build -f Dockerfile -t worker:1 . | tee log",
		"b:",
		"  script:",
		"    - docker build -t web:2 -t web:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml" --target source --target web`,
	))
	if !hasNote(p, "UNSUPPORTED_EDIT") {
		t.Errorf("notes = %v", noteCodes(p))
	}

	// A failure after edits were queued (the second build is an alias item that would have to
	// change) rolls back the first build's tag too.
	ci = lines(
		"defs:",
		"  - &worker docker build -f Dockerfile -t worker:1 . && docker push worker:1",
		"a:",
		"  script:",
		"    - docker build -t web:1 .",
		"    - *worker",
	)
	p, got = result(t, npmRepo(".", ci), "1.2.3")
	if got != "" || !hasNote(p, "UNSUPPORTED_EDIT") {
		t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
	}
}

// Items that are aliases, or that define anchors, are not changed in place.
func TestScriptAliasItem(t *testing.T) {
	ci := lines(
		"x:",
		"  script:",
		"    - &build docker build -t web:1 . && docker push web:1",
		"y:",
		"  script:",
		"    - *build",
	)
	p, got := result(t, npmRepo(".", ci), "1.2.3")
	if got != "" || !hasNote(p, "UNSUPPORTED_EDIT") {
		t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
	}
}

func TestScriptSnippet(t *testing.T) {
	g := plan.Gate{
		Project: plan.Project{Dir: "apps/web"}, Config: "apps/web/shearbolt.yaml", Version: "1.2.3",
		Images:  []plan.ImageVar{{Var: "SHEARBOLT_IMAGE", ScanTag: `"$IMAGE:shearbolt-$CI_COMMIT_SHA"`}},
		Targets: []string{"source", "web"},
	}
	got := scriptci.Snippet(g, "$CI_PROJECT_DIR", 4)
	exp := "    - " + install + "\n" +
		`    - SHEARBOLT_IMAGE="$IMAGE:shearbolt-$CI_COMMIT_SHA" "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/apps/web/shearbolt.yaml" --target source --target web`
	if got != exp {
		t.Errorf("snippet:\n%s\nwant:\n%s", got, exp)
	}
}

// A railpack build gets setup's docker tag right after it: as an item of its own when the build
// ends its item, joined with && inside a combined line.
func TestScriptRailpack(t *testing.T) {
	_, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - railpack build --name web:1 .",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - railpack build --name web:1 .",
		"    - docker tag web:1 web:shearbolt-$CI_COMMIT_SHA",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
	))

	_, got = result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - railpack build --name web:1 . && docker push web:1",
	)), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - railpack build --name web:1 . && docker tag web:1 web:shearbolt-$CI_COMMIT_SHA",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - docker push web:1",
	))
}

// A build in a called file: the gate follows the calling command, the Makefile is rewritten by
// the core, and the install item goes before the call.
func TestScriptViaBuild(t *testing.T) {
	files := map[string]string{
		"Cargo.toml":  "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"Cargo.lock":  "version = 3\n",
		"src/main.rs": "fn main() {}\n",
		"Makefile":    "release:\n\tcargo build --release\n",
		file: lines(
			"build:",
			"  script:",
			"    - make release && ./upload.sh",
		),
	}
	p, got := result(t, files, "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - cargo install cargo-auditable --locked",
		"    - make release",
		"    - "+install,
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`,
		"    - ./upload.sh",
	))
	i := slices.IndexFunc(p.Changes, func(c plan.FileChange) bool { return c.Path == "Makefile" })
	if i < 0 || p.Changes[i].Content != "release:\n\tcargo auditable build --release\n" {
		t.Errorf("changes: %+v", p.Changes)
	}
}

// Two projects built in one job get a gate each; the second reuses the first one's install.
func TestScriptTwoProjects(t *testing.T) {
	files := map[string]string{
		"apps/a/package.json": `{"name":"a"}`, "apps/a/package-lock.json": "{}", "apps/a/Dockerfile": "FROM node:22\n",
		"apps/b/package.json": `{"name":"b"}`, "apps/b/package-lock.json": "{}", "apps/b/Dockerfile": "FROM node:22\n",
		file: lines(
			"build:",
			"  script:",
			"    - docker build -t a:1 apps/a",
			"    - docker build -t b:1 apps/b",
			"    - docker push a:1 && docker push b:1",
		),
	}
	_, got := result(t, files, "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - docker build -t a:1 -t a:shearbolt-$CI_COMMIT_SHA apps/a",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=a:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/apps/a/shearbolt.yaml"`,
		"    - docker build -t b:1 -t b:shearbolt-$CI_COMMIT_SHA apps/b",
		`    - SHEARBOLT_IMAGE=b:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/apps/b/shearbolt.yaml"`,
		"    - docker push a:1 && docker push b:1",
	))
}

// A job whose edited file would not be valid YAML (here a key it already repeats) is rolled back
// on its own and reported with its snippet.
func TestScriptRollbackOnInvalidResult(t *testing.T) {
	p, got := result(t, npmRepo(".", lines(
		"build:",
		"  stage: one",
		"  stage: two",
		"  script:",
		"    - docker build -t web:1 .",
	)), "1.2.3")
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
	if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, "would not be valid YAML") || !strings.Contains(p.Notes[i].Message, "scan --ci") {
		t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
	}
}

// The gate shares the job's shell: after a set +e that nothing turns back on, a failing scan
// would not stop the job, so the job is left unchanged.
func TestScriptErrexitOff(t *testing.T) {
	for name, items := range map[string][]string{
		"set +e":      {"    - set +e", "    - docker build -t web:1 .", "    - docker push web:1"},
		"+o errexit":  {"    - set -x +o errexit", "    - docker build -t web:1 . && docker push web:1"},
		"same item":   {"    - set +ex && docker build -t web:1 . && docker push web:1"},
		"conditional": {"    - set +e", `    - if [ -n "$CI" ]; then set -e; fi`, "    - docker build -t web:1 ."},
		"before scan": {"    - docker build -t web:1 .", "    - set +e", `    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`},
	} {
		t.Run(name, func(t *testing.T) {
			p, got := result(t, npmRepo(".", lines(append([]string{"build:", "  script:"}, items...)...)), "1.2.3")
			i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
			if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, "set +e") {
				t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
			}
		})
	}

	// Turned back on before the gate, or only after it: the job is edited.
	for _, items := range [][]string{
		{"    - set +e", "    - npm test || true", "    - set -e", "    - docker build -t web:1 ."},
		{"    - docker build -t web:1 .", "    - set +e", "    - echo after"},
		{"    - set -euo pipefail", "    - docker build -t web:1 ."},
	} {
		_, got := result(t, npmRepo(".", lines(append([]string{"build:", "  script:"}, items...)...)), "1.2.3")
		if !strings.Contains(got, "scan --ci") {
			t.Errorf("%q: ci.yml:\n%s", items, got)
		}
	}
}

// Builds that ask for attestations are refused where setup would load them (Batch 4 review
// decision): --push builds converted to --load, and buildx builds that get --load.
func TestScriptAttestationsRefused(t *testing.T) {
	for _, build := range []string{
		"docker buildx build --push --attest type=sbom -t a:1 .",
		"docker buildx build --push --sbom=true -t a:1 .",
		"docker buildx build --push --provenance -t a:1 .",
		"docker buildx build --push --attest=false -t a:1 .",
		"docker buildx build --provenance=mode=max -t a:1 .",
	} {
		p, got := result(t, npmRepo(".", lines("build:", "  script:", "    - "+build)), "1.2.3")
		i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
		if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, "attestations") {
			t.Errorf("%s: ci.yml:\n%s\nnotes %v", build, got, noteCodes(p))
		}
	}
	for _, build := range []string{
		"docker buildx build --push --sbom=false --provenance=false -t a:1 .",
		"docker buildx build --load --provenance=mode=max -t a:1 .", // already loads: setup adds nothing
	} {
		_, got := result(t, npmRepo(".", lines("build:", "  script:", "    - "+build)), "1.2.3")
		if !strings.Contains(got, "scan --ci") {
			t.Errorf("%s: ci.yml:\n%s", build, got)
		}
	}
}

// An existing scan counts only when its failure stops the job: top level, not negated, not in
// the background or a pipeline, joined only by ;, a newline or nothing (Batch 5 review). Any
// other one fails the job, which is left unchanged.
func TestScriptExistingGateMustStopTheJob(t *testing.T) {
	const scan = `"$HOME/.local/bin/shearbolt" scan --ci`
	for name, item := range map[string]string{
		"negated":    `    - '! ` + scan + `'`,
		"or true":    `    - SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA ` + scan + ` || true`,
		"piped":      `    - SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA ` + scan + ` | tee scan.log`,
		"background": `    - "\"$HOME/.local/bin/shearbolt\" scan --ci &"`,
		"if":         `    - if [ "$CI_COMMIT_BRANCH" = main ]; then ` + scan + `; fi`,
		"and echo":   `    - '` + scan + ` && echo scanned'`,
		"after and":  `    - 'cd . && ` + scan + `'`,
		"subshell":   `    - '(` + scan + `) || echo ignored'`,
	} {
		t.Run(name, func(t *testing.T) {
			p, got := result(t, npmRepo(".", lines(
				"build:",
				"  script:",
				"    - docker build -t x:$CI_COMMIT_SHA -t x:shearbolt-$CI_COMMIT_SHA .",
				"    - "+install,
				item,
				"    - docker push x:$CI_COMMIT_SHA",
			)), "1.2.3")
			i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
			if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, "would not stop the job") {
				t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
			}
		})
	}

	// Joined by ; or a newline it is a gate, and is brought up to date.
	_, got := result(t, npmRepo(".", lines(
		"build:",
		"  script:",
		"    - docker build -t x:$CI_COMMIT_SHA .",
		"    - "+install,
		"    - |",
		"      echo scanning; \"$HOME/.local/bin/shearbolt\" scan --ci",
		"      echo scanned",
		"    - docker push x:$CI_COMMIT_SHA",
	)), "1.2.3")
	if !strings.Contains(got, `echo scanning; SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`) {
		t.Errorf("ci.yml:\n%s", got)
	}
}

// An existing scan with a config of the user's is never re-pointed: the job fails and the note
// names that config (Batch 5 review).
func TestScriptExistingCustomConfig(t *testing.T) {
	files := npmRepo(".", lines(
		"build:",
		"  script:",
		"    - docker build -t x:$CI_COMMIT_SHA .",
		"    - "+install,
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config shearbolt.prod.yaml'`,
		"    - docker push x:$CI_COMMIT_SHA",
	))
	files["shearbolt.prod.yaml"] = "version: 1\nproject: web\ntargets:\n  - name: source\n    source: .\n"
	p, got := result(t, files, "1.2.3")
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
	if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, "shearbolt.prod.yaml") {
		t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
	}

	// The project's own config, by the clone directory, relative, or relative to a cd, is fine.
	for _, cfg := range []string{`"$CI_PROJECT_DIR/app/shearbolt.yaml"`, `"${CI_PROJECT_DIR}/app/shearbolt.yaml"`, "app/shearbolt.yaml", "--cd-- shearbolt.yaml"} {
		pre := ""
		if c, ok := strings.CutPrefix(cfg, "--cd-- "); ok {
			pre, cfg = "cd app; ", c
		}
		_, got := result(t, npmRepo("app", lines(
			"build:",
			"  script:",
			"    - docker build -t x:$CI_COMMIT_SHA app",
			"    - "+install,
			"    - '"+pre+`"$HOME/.local/bin/shearbolt" scan --ci --config `+cfg+"'",
			"    - docker push x:$CI_COMMIT_SHA",
		)), "1.2.3")
		if !strings.Contains(got, `scan --ci --config "$CI_PROJECT_DIR/app/shearbolt.yaml"`) {
			t.Errorf("--config %s: ci.yml:\n%s", cfg, got)
		}
	}
}

// Mutant reproductions from the Batch 5 review: each guard below, when removed, lets a push run
// before its gate or breaks the job.
func TestScriptReviewMutants(t *testing.T) {
	refused := func(t *testing.T, files map[string]string, reason string) {
		t.Helper()
		p, got := result(t, files, "1.2.3")
		i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
		if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, reason) {
			t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
		}
	}

	t.Run("two projects needing two splits", func(t *testing.T) {
		files := map[string]string{
			"apps/a/package.json": `{"name":"a"}`, "apps/a/package-lock.json": "{}", "apps/a/Dockerfile": "FROM node:22\n",
			"apps/b/package.json": `{"name":"b"}`, "apps/b/package-lock.json": "{}", "apps/b/Dockerfile": "FROM node:22\n",
			file: lines("build:", "  script:", "    - docker build -t a:1 apps/a && docker push a:1 && docker build -t b:1 apps/b && docker push b:1"),
		}
		refused(t, files, "two projects")
	})

	t.Run("existing gate in the --push item", func(t *testing.T) {
		refused(t, npmRepo(".", lines("build:", "  script:",
			`    - 'docker buildx build --push -t x:1 . ; "$HOME/.local/bin/shearbolt" scan --ci'`,
		)), "same item as the --push build")
	})

	t.Run("--push before an existing gate pushes after it", func(t *testing.T) {
		_, got := result(t, npmRepo(".", lines("build:", "  script:",
			"    - docker buildx build --push -t x:1 .",
			`    - '"$HOME/.local/bin/shearbolt" scan --ci'`,
		)), "1.2.3")
		want(t, got, lines("build:", "  script:",
			"    - docker buildx build --load -t x:1 -t x:shearbolt-$CI_COMMIT_SHA .",
			"    - "+install,
			`    - 'SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`,
			"    - docker push x:1",
		))
	})

	t.Run("split at a pipe", func(t *testing.T) {
		refused(t, map[string]string{
			"Cargo.toml": "[package]\nname = \"demo\"\nversion = \"0.1.0\"\n", "Cargo.lock": "version = 3\n", "src/main.rs": "fn main() {}\n",
			file: lines("release:", "  script:", "    - cargo build --release | tee log; gh release upload v1 target/release/demo"),
		}, "piped")
	})

	t.Run("install item with more commands is kept", func(t *testing.T) {
		mixed := strings.Replace(install, "1.2.3", "1.0.0", 1) + " && echo installed && docker login -u u -p p r.example.com"
		_, got := result(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t x:1 -t x:shearbolt-$CI_COMMIT_SHA .",
			"    - "+mixed,
			`    - SHEARBOLT_IMAGE=x:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci`,
			"    - docker push x:1",
		)), "1.2.3")
		if !strings.Contains(got, "    - "+mixed+"\n") || !strings.Contains(got, `scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`) {
			t.Errorf("ci.yml:\n%s", got)
		}
	})
}

// docker push --all-tags would push the local scan tag too, which must never be pushed (R5):
// a gated job with one for a scanned repository, or for a repository setup cannot read, fails.
func TestScriptAllTagsPushRefused(t *testing.T) {
	for _, push := range []string{
		"docker push --all-tags reg.example.com/x",
		"docker push -a reg.example.com/x",
		"docker image push --all-tags reg.example.com/x",
		`docker push --all-tags "$IMAGE"`,
	} {
		p, got := result(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t reg.example.com/x:$CI_COMMIT_SHA -t reg.example.com/x:latest .",
			"    - "+push,
		)), "1.2.3")
		i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
		if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, "--all-tags") {
			t.Errorf("%s: ci.yml:\n%s\nnotes %v", push, got, noteCodes(p))
		}
	}
	// Another literal repository is fine.
	_, got := result(t, npmRepo(".", lines("build:", "  script:",
		"    - docker build -t reg.example.com/x:$CI_COMMIT_SHA .",
		"    - docker push --all-tags reg.example.com/other",
	)), "1.2.3")
	if !strings.Contains(got, "scan --ci") {
		t.Errorf("ci.yml:\n%s", got)
	}
}

// Guards the Batch 5 review found untested.
func TestScriptMoreGuards(t *testing.T) {
	refused := func(t *testing.T, files map[string]string, version, reason string) *plan.Plan {
		t.Helper()
		p, got := result(t, files, version)
		i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
		if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, reason) {
			t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
		}
		return p
	}
	const scan = `"$HOME/.local/bin/shearbolt" scan`

	t.Run("two builds share a scan tag", func(t *testing.T) {
		refused(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t x:1 -t x:shearbolt-$CI_COMMIT_SHA .",
			"    - docker build -f Dockerfile -t x:2 -t x:shearbolt-$CI_COMMIT_SHA .",
			"    - docker push x:1 && docker push x:2",
		)), "1.2.3", "share the scan tag")
	})

	t.Run("scan through a wrapper", func(t *testing.T) {
		refused(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t x:1 .", "    - "+install, "    - sudo "+scan+" --ci", "    - docker push x:1",
		)), "1.2.3", "wrapper")
	})

	t.Run("redirection among the scan's words", func(t *testing.T) {
		refused(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t x:1 .", "    - "+install, "    - '"+scan+" > scan.log --ci'", "    - docker push x:1",
		)), "1.2.3", "redirection")
		// A redirection at the end is kept.
		_, got := result(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t x:1 .", "    - "+install, "    - '"+scan+" --ci > scan.log'", "    - docker push x:1",
		)), "1.2.3")
		if !strings.Contains(got, `scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml" > scan.log`) {
			t.Errorf("ci.yml:\n%s", got)
		}
	})

	t.Run("an install after the gate does not count", func(t *testing.T) {
		_, got := result(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t x:1 .", "    - '"+scan+" --ci'", "    - "+install, "    - docker push x:1",
		)), "1.2.3")
		if strings.Count(got, install) != 2 || strings.Index(got, install) > strings.Index(got, "scan --ci") {
			t.Errorf("ci.yml:\n%s", got)
		}
	})

	t.Run("a marker after the build does not count", func(t *testing.T) {
		_, got := result(t, map[string]string{
			"Cargo.toml": "[package]\nname = \"demo\"\nversion = \"0.1.0\"\n", "Cargo.lock": "version = 3\n", "src/main.rs": "fn main() {}\n",
			file: lines("build:", "  script:", "    - cargo build --release", "    - cargo install cargo-auditable --locked"),
		}, "1.2.3")
		if !strings.HasPrefix(got, lines("build:", "  script:", "    - cargo install cargo-auditable --locked", "    - cargo auditable build --release")) {
			t.Errorf("ci.yml:\n%s", got)
		}
	})

	t.Run("a failed job's dev snippet is unpinned", func(t *testing.T) {
		p := refused(t, npmRepo(".", lines("build:", "  script:", "    - docker build -t x:1 . | tee log")), "dev", "piped")
		if !hasNote(p, "UNPINNED_VERSION") {
			t.Errorf("notes %v", noteCodes(p))
		}
	})

	t.Run("the plugin's job edit fails", func(t *testing.T) {
		root := tree(t, npmRepo(".", lines("build:", "  script:", "    - docker build -t x:1 .")))
		p := setup.ComputeWith(setup.Options{Root: root, Yes: true, Version: "1.2.3"}, listCI{failExtra: true})
		i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
		if p.Error != nil || slices.ContainsFunc(p.Changes, func(c plan.FileChange) bool { return c.Path == file }) ||
			i < 0 || !strings.Contains(p.Notes[i].Message, "the cache edit failed") || hasNote(p, "TAG_ADDED") {
			t.Errorf("plan %+v", p)
		}
	})
}

// Batch 5 re-review: guards whose mutants survived.
func TestScriptReReviewMutants(t *testing.T) {
	// A custom config named by the clone directory is still the user's.
	t.Run("clone-dir custom config", func(t *testing.T) {
		p, got := result(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t x:1 .",
			`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.prod.yaml"'`,
			"    - docker push x:1",
		)), "1.2.3")
		i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
		if got != "" || i < 0 || !strings.Contains(p.Notes[i].Message, "shearbolt.prod.yaml") {
			t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
		}
	})
	// An item that is only an sh holds no install: it is kept, and the gate gets its install.
	t.Run("an sh alone is no install", func(t *testing.T) {
		_, got := result(t, npmRepo(".", lines("build:", "  script:",
			"    - docker build -t x:1 .",
			"    - bash",
			`    - '"$HOME/.local/bin/shearbolt" scan --ci'`,
			"    - docker push x:1",
		)), "1.2.3")
		if !strings.Contains(got, lines("    - bash", "    - "+install)) {
			t.Errorf("ci.yml:\n%s", got)
		}
	})
}

// grabbing is listCI, recording the classifier setup passes to Jobs.
type grabbing struct {
	listCI
	classify *plan.Classifier
}

func (g grabbing) Jobs(r plan.Repo, f string, src []byte, classify plan.Classifier) ([]plan.Job, []plan.Note, error) {
	*g.classify = classify
	return g.listCI.Jobs(r, f, src, classify)
}

// classifier returns the core's classifier for a repository with files, and the scripts of its
// ci.yml.
func classifier(t *testing.T, files map[string]string) (plan.Classifier, []*scriptci.Script) {
	t.Helper()
	var classify plan.Classifier
	setup.ComputeWith(setup.Options{Root: tree(t, files), Yes: true}, grabbing{classify: &classify})
	if classify == nil {
		t.Fatal("setup did not read ci.yml")
	}
	d, err := yamledit.Parse([]byte(files[file]))
	if err != nil {
		t.Fatal(err)
	}
	return classify, scripts(d)
}

// P6: Step.Stops lists the commands of an item that can stop the job's shell early, and
// Script.Stops reports them with their lines, a sourced file setup cannot follow included.
func TestScriptStops(t *testing.T) {
	classify, ss := classifier(t, map[string]string{
		"Dockerfile": "FROM alpine\n",
		file: lines(
			"build:",
			"  script:",
			`    - if [ -z "$X" ]; then exit 0; fi`,
			"    - docker build .",
			"    - cd app || exit 1",
			"    - . ./ci/missing.sh",
			"    - |",
			"      echo one",
			"      exec docker push x",
		),
	})
	if len(ss) != 1 {
		t.Fatalf("scripts %d", len(ss))
	}
	s := ss[0]
	steps := s.Steps(classify, nil)
	exit := slices.IndexFunc(steps[0].Commands, func(c shell.Command) bool { return slices.Equal(c.Argv(), []string{"exit", "0"}) })
	if exit < 0 || !slices.Equal(steps[0].Stops, []int{exit}) {
		t.Errorf("first item: stops %v, exit at %d", steps[0].Stops, exit)
	}
	if len(steps[1].Stops) != 0 || len(steps[1].Builds) != 1 || len(steps[2].Stops) != 0 {
		t.Errorf("build and cd items: %+v, %+v", steps[1], steps[2])
	}
	if !slices.Equal(steps[3].Stops, []int{0}) || !slices.Equal(steps[4].Stops, []int{1}) {
		t.Errorf("sourced and exec items: stops %v, %v", steps[3].Stops, steps[4].Stops)
	}
	got := s.Stops(classify, nil)
	wantStops := []scriptci.Found{
		{What: "`exit 0`", Line: 3, Name: "exit"},
		{What: "`. ./ci/missing.sh`", Line: 6, Name: "source"}, // "(. at line 6)" reads badly
		{What: "`exec docker push x`", Line: 9, Name: "exec"},
	}
	if !slices.Equal(got, wantStops) {
		t.Errorf("Stops = %+v, want %+v", got, wantStops)
	}

	// Every stop the core reports has a name, whatever form it takes.
	classify, ss = classifier(t, map[string]string{
		"ci/skip.sh": "exit 0\n",
		file: lines(
			"build:",
			"  script:",
			"    - exit",
			`    - exit "$code"`,
			"    - FOO=1 exec ./run.sh",
			"    - exec docker push x 2> >(logger)",
			"    - source ci/skip.sh",
			"    - . ci/skip.sh",
			"    - (exit 0)",
			"    - npm ci && exit $?",
		),
	})
	got = ss[0].Stops(classify, nil)
	if len(got) != 8 {
		t.Errorf("Stops = %+v, want 8", got)
	}
	for _, f := range got {
		if f.Name == "" {
			t.Errorf("%s at line %d has no name", f.What, f.Line)
		}
	}
}

// stopMessage is the start of the reason an early stop before the gate fails a job (P12).
func stopMessage(cmd string, line int, what string) string {
	return fmt.Sprintf("the job can stop before the shearbolt gate (%s at line %d), which would skip its check of %s while later jobs still run; move the early stop into the job's rules or condition, or add the gate by hand where it runs on every path", cmd, line, what)
}

// stopRefused checks that setup leaves ci.yml unchanged and fails its job with UNSUPPORTED_EDIT
// on the early stop's line: the stop's name and line, what the gate would not check, the snippet.
func stopRefused(t *testing.T, files map[string]string, cmd string, line int, what string) {
	t.Helper()
	p, got := result(t, files, "1.2.3")
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" && n.File == file })
	if got != "" || i < 0 {
		t.Fatalf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
	}
	n := p.Notes[i]
	if !strings.Contains(n.Message, stopMessage(cmd, line, what)) || !strings.Contains(n.Message, `"$HOME/.local/bin/shearbolt" scan --ci --config`) || n.Line != line || n.File != file {
		t.Errorf("note %+v\nwant %q", n, stopMessage(cmd, line, what))
	}
}

// P12 (design §3.5): the gate runs in the job's shell, so an early stop before it ends the job's
// script successfully, skips the gate, and later jobs still run. A job whose gate checks
// anything other than images built after the stop is left unchanged with UNSUPPORTED_EDIT.
func TestScriptEarlyStop(t *testing.T) {
	npm := func(items ...string) map[string]string {
		return npmRepo(".", lines(append([]string{"build:", "  script:"}, items...)...))
	}
	// ES-5: an exit before an npm build; the gate would check the source.
	stopRefused(t, npm(`    - if [ -z "$CHANGED" ]; then exit 0; fi`, "    - npm ci", "    - npm run build", "    - ./deploy.sh"), "exit", 3, "source")

	// A sourced file that exits, and one setup cannot follow.
	files := npm("    - . ./ci/skip.sh", "    - npm ci", "    - npm run build", "    - ./deploy.sh")
	files["ci/skip.sh"] = "if [ -z \"$CHANGED\" ]; then exit 0; fi\n"
	stopRefused(t, files, "source", 3, "source")
	stopRefused(t, npm("    - npm ci", "    - . ./ci/missing.sh", "    - npm run build", "    - ./deploy.sh"), "source", 4, "source")

	// The last stop before the gate counts.
	stopRefused(t, npm("    - exec ./ci/run.sh", "    - npm ci", `    - if [ -z "$CHANGED" ]; then exit 0; fi`, "    - npm run build"), "exit", 5, "source")

	// A stop after the build in the build's item lands after the gate once the item is split.
	_, got := result(t, npm("    - npm ci", `    - npm run build; if [ -n "$X" ]; then exit 0; fi; ./deploy.sh`), "1.2.3")
	want(t, got, lines(
		"build:",
		"  script:",
		"    - npm ci",
		"    - npm run build",
		"    - "+install,
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`,
		`    - if [ -n "$X" ]; then exit 0; fi; ./deploy.sh`,
	))

	// A literal non-zero exit fails the job instead: no early stop.
	p, got := result(t, npmRepo("app", lines("build:", "  script:", "    - cd app || exit 1", "    - npm ci", "    - npm run build", "    - ./deploy.sh")), "1.2.3")
	if !strings.Contains(got, `scan --ci --config "$CI_PROJECT_DIR/app/shearbolt.yaml"`) || hasNote(p, "UNSUPPORTED_EDIT") {
		t.Errorf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
	}

	// A root project whose gate scans only the image built after the stop: skipping it is safe,
	// and the gate follows the build as usual.
	root := func(items ...string) map[string]string {
		return map[string]string{"Dockerfile": "FROM alpine\n", file: lines(append([]string{"deploy:", "  script:"}, items...)...)}
	}
	p, got, again := rerun(t, root(`    - if [ -z "$CHANGED" ]; then exit 0; fi`, "    - docker build -t web:$CI_COMMIT_SHA .", "    - docker push web:$CI_COMMIT_SHA"), "1.2.3")
	want(t, got, lines(
		"deploy:",
		"  script:",
		`    - if [ -z "$CHANGED" ]; then exit 0; fi`,
		"    - docker build -t web:$CI_COMMIT_SHA -t web:shearbolt-$CI_COMMIT_SHA .",
		"    - "+install,
		`    - SHEARBOLT_IMAGE=web:shearbolt-$CI_COMMIT_SHA "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"`,
		"    - docker push web:$CI_COMMIT_SHA",
	))
	if hasNote(p, "UNSUPPORTED_EDIT") || hasNote(again, "UNSUPPORTED_EDIT") {
		t.Errorf("notes %v\nsecond run %v", noteCodes(p), noteCodes(again))
	}

	// An image built before the stop would go unchecked: the job is refused, and the message
	// names that image's target but not the one built after the stop.
	p, got = result(t, root("    - docker build -t a .", `    - if [ -z "$CHANGED" ]; then exit 0; fi`, "    - docker build -t b .", "    - docker push a && docker push b"), "1.2.3")
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
	if got != "" || i < 0 {
		t.Fatalf("ci.yml:\n%s\nnotes %v", got, noteCodes(p))
	}
	m := regexp.MustCompile(`\(exit at line 4\), which would skip its check of (\S+) while later jobs still run`).FindStringSubmatch(p.Notes[i].Message)
	var cfg struct{ Targets []struct{ Name string } }
	for _, c := range p.Changes {
		if c.Path == "shearbolt.yaml" {
			if err := yaml.Unmarshal([]byte(c.Content), &cfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(cfg.Targets) != 2 || cfg.Targets[1].Name != "b" || m == nil || m[1] != cfg.Targets[0].Name {
		t.Errorf("note %q\ntargets %+v", p.Notes[i].Message, cfg.Targets)
	}
}

// P12 on re-runs and in the rarer shapes: a gate already in the job (here one added by hand)
// after an early stop fails the job too, as does a second project's gate after a stop between
// the two; when the config cannot be read, the message names every target; a stop the plan places
// outside its item fails the job.
func TestScriptEarlyStopMore(t *testing.T) {
	npm := func(items ...string) map[string]string {
		return npmRepo(".", lines(append([]string{"build:", "  script:"}, items...)...))
	}
	stopRefused(t, npm(
		`    - if [ -z "$CHANGED" ]; then exit 0; fi`,
		"    - npm ci",
		"    - npm run build",
		"    - "+install,
		`    - '"$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"'`,
		"    - ./deploy.sh",
	), "exit", 3, "source")

	// Two projects: the first gate runs before the stop, the second after it.
	stopRefused(t, map[string]string{
		"apps/a/package.json": `{"name":"a"}`, "apps/a/package-lock.json": "{}", "apps/a/Dockerfile": "FROM node:22\n",
		"apps/b/package.json": `{"name":"b"}`, "apps/b/package-lock.json": "{}", "apps/b/Dockerfile": "FROM node:22\n",
		file: lines(
			"build:",
			"  script:",
			"    - docker build -t a:1 apps/a",
			`    - if [ -z "$CHANGED" ]; then exit 0; fi`,
			"    - docker build -t b:1 apps/b",
			"    - docker push a:1 && docker push b:1",
		),
	}, "exit", 4, "source")

	// A config whose targets setup cannot read: the gate scans every target it has.
	files := npm(`    - if [ -z "$CHANGED" ]; then exit 0; fi`, "    - npm ci", "    - npm run build", "    - ./deploy.sh")
	files["shearbolt.yaml"] = "version: 1\ntargets: [{name: source, source: .}]\n"
	stopRefused(t, files, "exit", 3, "every target in shearbolt.yaml")

	p := setup.ComputeWith(setup.Options{Root: tree(t, npm("    - npm ci", "    - npm run build", "    - ./deploy.sh")), Yes: true, Version: "1.2.3"}, badStops{})
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
	if p.Error != nil || slices.ContainsFunc(p.Changes, func(c plan.FileChange) bool { return c.Path == file }) || i < 0 || !strings.Contains(p.Notes[i].Message, "a command that can stop the job early is not in the job") {
		t.Errorf("error %v, changes %+v, notes %v", p.Error, p.Changes, noteCodes(p))
	}
}

// badStops is listCI with a stop index past the end of each job's first item.
type badStops struct{ listCI }

func (b badStops) Jobs(r plan.Repo, f string, src []byte, classify plan.Classifier) ([]plan.Job, []plan.Note, error) {
	js, ns, err := b.listCI.Jobs(r, f, src, classify)
	for i := range js {
		js[i].Steps[0].Stops = append(js[i].Steps[0].Stops, len(js[i].Steps[0].Commands))
	}
	return js, ns, err
}
