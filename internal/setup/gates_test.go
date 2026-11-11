package setup

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

const ciFile = ".github/workflows/ci.yml"

// fstep is one step of a hand-built job: a run script, or an action that may push and may carry
// builds its plugin recognized.
type fstep struct {
	run    string
	dir    string
	uses   string
	push   bool
	builds []plan.Build
}

func run(s string) fstep { return fstep{run: s} }

// mkJob builds an editable job the way a CI plugin does: each run script is parsed from its
// directory and its commands classified; builds and gates get the command's Pos. Each step
// starts at line 100*(index+1), and a command's line adds its line in the script.
func mkJob(t *testing.T, r plan.Repo, key string, steps ...fstep) plan.Job {
	t.Helper()
	classify := newClassifier(r, ecosystems)
	j := plan.Job{ID: ciFile + "#" + key, Label: path.Base(ciFile) + ": " + key}
	for s, fs := range steps {
		st := plan.Step{Dir: ".", Uses: fs.uses}
		if fs.dir != "" {
			st.Dir = fs.dir
		}
		base := 100 * (s + 1)
		if fs.uses != "" {
			if fs.push {
				st.Push = []int{-1}
			}
			for _, b := range fs.builds {
				b.Pos = plan.Pos{File: ciFile, Job: j.ID, Step: s, Command: -1, Line: base}
				st.Builds = append(st.Builds, b)
			}
			j.Steps = append(j.Steps, st)
			continue
		}
		cmds, _ := shell.Parse(fs.run, shell.State{Dir: st.Dir})
		for c, cmd := range cmds {
			pos := plan.Pos{File: ciFile, Job: j.ID, Step: s, Command: c, Line: base + strings.Count(fs.run[:cmd.Start], "\n")}
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
		j.Steps = append(j.Steps, st)
	}
	return j
}

func npmProject(dir, name string) plan.Project {
	return plan.Project{Ecosystem: "npm", Dir: dir, Name: name, Lockfiles: []string{"package-lock.json"}}
}

func questionIDs(qs []plan.Question) []string {
	var out []string
	for _, q := range qs {
		out = append(out, q.ID)
	}
	return out
}

func resolve(t *testing.T, qs []plan.Question, answers map[string]string) map[string]string {
	t.Helper()
	values, _, err := plan.Resolve(qs, answers, true)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

// planGates runs the gate steps with the given answers (defaults for the rest), an empty
// repository config, and GitHub's commit expression. It returns the gates and the planned
// targets per project.
func planGates(t *testing.T, r plan.Repo, projects []plan.Project, jobs []plan.Job, answers map[string]string) ([]plan.Gate, map[string][]config.Target, *placement) {
	t.Helper()
	m := newGateModel(projects, jobs, []string{ciFile})
	pl := m.place(resolve(t, m.questions(), answers), "1.2.3", fakeSnippet)
	var gates []plan.Gate
	planned := map[string][]config.Target{}
	used := map[string]map[string]bool{}
	for _, p := range projects {
		pp := pl.projects[p.Dir]
		if pp == nil {
			continue
		}
		planned[p.Dir] = pp.targets
		_, merged, names, vars, _ := configChange(r, p, pp.targets)
		gates = append(gates, pp.finish(merged, names, vars, func(string) string { return commitExprs["github"] }, used)...)
	}
	return gates, planned, pl
}

func fakeSnippet(file string, g plan.Gate) string {
	return "SNIPPET(" + file + "," + g.Project.Dir + ")"
}

func hasCode(notes []plan.Note, code string) bool {
	return slices.ContainsFunc(notes, func(n plan.Note) bool { return n.Code == code })
}

func countCode(notes []plan.Note, code string) int {
	n := 0
	for _, x := range notes {
		if x.Code == code {
			n++
		}
	}
	return n
}

func TestGatesProjectsQuestion(t *testing.T) {
	r := repo(t, nil)
	job := mkJob(t, r, "test", run("npm run build"))
	qs := newGateModel([]plan.Project{npmProject(".", "web")}, []plan.Job{job}, []string{ciFile}).questions()
	if len(qs) != 0 {
		t.Fatalf("one project: %v", questionIDs(qs))
	}
	projects := []plan.Project{npmProject("apps/a", "a"), {Ecosystem: "rust", Dir: "crates/b", Name: "b"}}
	projects[0].Framework = "nextjs"
	qs = newGateModel(projects, []plan.Job{job}, []string{ciFile}).questions()
	if len(qs) == 0 || qs[0].ID != "projects" || qs[0].Kind != "multi" || qs[0].Default != "apps/a,crates/b" {
		t.Fatalf("two projects: %+v", qs)
	}
	if qs[0].Options[0].Label != "apps/a (npm, Next.js)" || qs[0].Options[1].Label != "crates/b (rust)" {
		t.Fatalf("labels: %+v", qs[0].Options)
	}
}

func TestGatesBuildProjectAndGateJob(t *testing.T) {
	r := repo(t, nil)
	projects := []plan.Project{npmProject("apps/a", "a"), npmProject("apps/b", "b")}
	jobs := []plan.Job{
		mkJob(t, r, "test", run("cd apps/a && npm run build"), run("npm run build")),
		mkJob(t, r, "lint", run("npm run lint")),
	}
	m := newGateModel(projects, jobs, []string{ciFile})
	qs := m.questions()
	bp := "build-project:" + ciFile + ":200"
	if got := questionIDs(qs); !slices.Equal(got, []string{"projects", bp, "gate-job:apps/b"}) {
		t.Fatalf("questions: %v", got)
	}
	q := qs[1]
	if q.Kind != "choice" || q.Default != "apps/a" || len(q.Options) != 3 || q.Options[2].Value != "none" {
		t.Fatalf("build-project: %+v", q)
	}
	gj := qs[2]
	if gj.Project != "apps/b" || gj.Default != jobs[0].ID || !slices.Equal(gj.UnlessChosen, []string{bp}) ||
		len(gj.Options) != 2 || gj.Options[1].Value != jobs[1].ID || gj.Options[1].Label != "ci.yml: lint" {
		t.Fatalf("gate-job: %+v", gj)
	}

	// Answering the build-project question with b makes gate-job:apps/b inapplicable, and b's
	// gate follows that build.
	values := resolve(t, qs, map[string]string{bp: "apps/b"})
	if _, ok := values["gate-job:apps/b"]; ok {
		t.Fatalf("gate-job:apps/b still applies: %v", values)
	}
	pl := m.place(values, "1.2.3", fakeSnippet)
	gb := pl.projects["apps/b"].gates
	if len(gb) != 1 || gb[0].gate.Job != jobs[0].ID || gb[0].gate.At.Step != 1 || gb[0].gate.Before || hasCode(pl.notes, "NO_BUILD_FOUND") {
		t.Fatalf("b's gate: %+v, notes %v", gb, pl.notes)
	}

	// Answering none leaves the build ungated; b gets the gate-job default and NO_BUILD_FOUND.
	pl = m.place(resolve(t, qs, map[string]string{bp: "none"}), "1.2.3", fakeSnippet)
	gb = pl.projects["apps/b"].gates
	if len(gb) != 1 || len(gb[0].gate.Builds) != 0 || gb[0].gate.At.Step != 0 || !gb[0].gate.Before || !hasCode(pl.notes, "NO_BUILD_FOUND") {
		t.Fatalf("none: %+v, notes %v", gb, pl.notes)
	}
	for _, g := range append(pl.projects["apps/a"].gates, pl.projects["apps/b"].gates...) {
		for _, b := range g.gate.Builds {
			if b.Pos.Step == 1 {
				t.Fatalf("the unanswered build is gated: %+v", g.gate)
			}
		}
	}
	if len(pl.projects["apps/a"].gates) != 1 || pl.projects["apps/a"].gates[0].gate.At.Step != 0 {
		t.Fatalf("a's gate: %+v", pl.projects["apps/a"].gates)
	}
}

func TestGatesQuestionIDsOnOneLine(t *testing.T) {
	r := repo(t, nil)
	projects := []plan.Project{npmProject("apps/a", "a"), npmProject("apps/b", "b")}
	jobs := []plan.Job{mkJob(t, r, "test", run("npm run build && npm run build:docs"), run("cd $X && npm run build"))}
	got := questionIDs(newGateModel(projects, jobs, []string{ciFile}).questions())
	want := []string{"projects", "build-project:" + ciFile + ":100:1", "build-project:" + ciFile + ":100:2", "build-project:" + ciFile + ":200",
		"gate-job:apps/a", "gate-job:apps/b"}
	if !slices.Equal(got, want) {
		t.Fatalf("questions %v, want %v", got, want)
	}
}

func TestGatesRewriteOnly(t *testing.T) {
	// An unattributable RewriteOnly build: no question, no Prepare.
	r := repo(t, map[string]string{"Makefile": rustMake})
	projects := []plan.Project{npmProject("apps/a", "a"), npmProject("apps/b", "b")}
	jobs := []plan.Job{mkJob(t, r, "test", run("make test"))}
	m := newGateModel(projects, jobs, []string{ciFile})
	qs := m.questions()
	if got := questionIDs(qs); !slices.Equal(got, []string{"projects", "gate-job:apps/a", "gate-job:apps/b"}) {
		t.Fatalf("questions: %v", got)
	}
	pl := m.place(resolve(t, qs, nil), "1.2.3", fakeSnippet)
	if len(pl.attributed["apps/a"])+len(pl.attributed["apps/b"]) != 0 {
		t.Fatalf("attributed: %v", pl.attributed)
	}

	// A prefix-attributed RewriteOnly build is prepared, but its project still gets gate-job.
	r = repo(t, map[string]string{"crates/cli/Makefile": rustMake, "crates/cli/Cargo.toml": demoToml, "crates/cli/Cargo.lock": "", "crates/cli/src/main.rs": ""})
	projects = []plan.Project{npmProject("apps/a", "a"), {Ecosystem: "rust", Dir: "crates/cli", Name: "demo", Binaries: []string{"demo"}}}
	jobs = []plan.Job{mkJob(t, r, "test", run("cd apps/a && npm run build"), run("make -C crates/cli test"))}
	m = newGateModel(projects, jobs, []string{ciFile})
	qs = m.questions()
	if got := questionIDs(qs); !slices.Equal(got, []string{"projects", "gate-job:crates/cli"}) {
		t.Fatalf("questions: %v", got)
	}
	pl = m.place(resolve(t, qs, nil), "1.2.3", fakeSnippet)
	if bs := pl.attributed["crates/cli"]; len(bs) != 1 || !bs[0].RewriteOnly {
		t.Fatalf("attributed: %v", pl.attributed)
	}
	g := pl.projects["crates/cli"].gates
	if len(g) != 1 || len(g[0].gate.Builds) != 0 || !g[0].gate.Before || g[0].gate.At.Step != 0 {
		t.Fatalf("crates/cli gate: %+v", g)
	}
	if pp := pl.projects["crates/cli"]; !slices.Equal(targetNames(pp.targets), []string{"source"}) {
		t.Fatalf("targets: %v", pp.targets)
	}
}

func TestGatesUnknownDirectoryAsks(t *testing.T) {
	r := repo(t, nil)
	projects := []plan.Project{npmProject(".", "root"), npmProject("apps/b", "b")}
	jobs := []plan.Job{mkJob(t, r, "test", run("cd \"$X\" && npm run build"))}
	if got := questionIDs(newGateModel(projects, jobs, []string{ciFile}).questions()); !slices.Contains(got, "build-project:"+ciFile+":100") {
		t.Fatalf("questions: %v", got)
	}
	// With one project, it belongs to that project.
	m := newGateModel(projects[:1], jobs, []string{ciFile})
	if qs := m.questions(); len(qs) != 0 {
		t.Fatalf("one project: %v", questionIDs(qs))
	}
	pl := m.place(map[string]string{}, "1.2.3", fakeSnippet)
	if g := pl.projects["."].gates; len(g) != 1 || len(g[0].gate.Builds) != 1 {
		t.Fatalf("gate: %+v", g)
	}
}

func TestGatesNoEditableJob(t *testing.T) {
	projects := []plan.Project{npmProject("apps/a", "a"), npmProject("apps/b", "b")}
	m := newGateModel(projects, nil, []string{ciFile, ".gitlab-ci.yml"})
	qs := m.questions()
	if got := questionIDs(qs); !slices.Equal(got, []string{"projects"}) {
		t.Fatalf("questions: %v", got)
	}
	pl := m.place(resolve(t, qs, nil), "1.2.3", fakeSnippet)
	for _, pp := range pl.projects {
		if len(pp.gates) != 0 {
			t.Fatalf("gates: %+v", pp.gates)
		}
	}
	for _, p := range projects {
		if pp := pl.projects[p.Dir]; pp == nil || !slices.Equal(targetNames(pp.targets), []string{"source"}) {
			t.Fatalf("%s: planned %+v", p.Dir, pp)
		}
	}
	if countCode(pl.notes, "NO_GATE_PLACED") != 2 {
		t.Fatalf("notes: %v", pl.notes)
	}
	for i, n := range pl.notes {
		dir := projects[i].Dir
		if !strings.Contains(n.Message, "SNIPPET("+ciFile+","+dir+")") || !strings.Contains(n.Message, "SNIPPET(.gitlab-ci.yml,"+dir+")") {
			t.Fatalf("note %d: %q", i, n.Message)
		}
	}
}

func TestGatesGateJobs(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM node\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{
		mkJob(t, r, "test", fstep{uses: "actions/checkout@v5"}, run("npm ci && npm run build")),
		mkJob(t, r, "docker", fstep{uses: "actions/checkout@v5"}, run("docker build -t ghcr.io/acme/web:latest .")),
	}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	if len(gates) != 1 || gates[0].Job != jobs[1].ID || gates[0].At.Step != 1 || gates[0].Before {
		t.Fatalf("gates: %+v", gates)
	}
	if gates[0].Config != "shearbolt.yaml" || gates[0].Version != "1.2.3" || gates[0].Project.Dir != "." {
		t.Fatalf("gate: %+v", gates[0])
	}
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"web", "source"}) || hasCode(pl.notes, "NO_BUILD_FOUND") {
		t.Fatalf("planned %v, notes %v", got, pl.notes)
	}

	// No build at all: the gate goes after the checkout step of the gate-job, with NO_BUILD_FOUND.
	projects = append(projects, npmProject("apps/api", "api"))
	gates, _, pl = planGates(t, r, projects, jobs, map[string]string{"gate-job:apps/api": jobs[1].ID})
	var api []plan.Gate
	for _, g := range gates {
		if g.Project.Dir == "apps/api" {
			api = append(api, g)
		}
	}
	if len(api) != 1 || api[0].Job != jobs[1].ID || api[0].At.Step != 0 || api[0].At.Command != -1 || api[0].Before || len(api[0].Builds) != 0 {
		t.Fatalf("api gate: %+v", api)
	}
	if !hasCode(pl.notes, "NO_BUILD_FOUND") {
		t.Fatalf("notes: %v", pl.notes)
	}
	n := pl.notes[slices.IndexFunc(pl.notes, func(n plan.Note) bool { return n.Code == "NO_BUILD_FOUND" })]
	if n.File != ciFile || !strings.Contains(n.Message, "apps/api") || !strings.Contains(n.Message, "ci.yml: docker") {
		t.Fatalf("NO_BUILD_FOUND: %+v", n)
	}
	// Without a checkout step, the gate goes before the first step.
	jobs = []plan.Job{mkJob(t, r, "deploy", run("echo hi"))}
	gates, _, _ = planGates(t, r, projects[1:], jobs, nil)
	if len(gates) != 1 || !gates[0].Before || gates[0].At.Step != 0 || gates[0].At.Command != -1 {
		t.Fatalf("no checkout: %+v", gates)
	}
}

func TestGatesPlacementAfterPush(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{mkJob(t, r, "docker",
		run("docker build -t ghcr.io/acme/a:1 ."),
		run("docker push ghcr.io/acme/a:1"),
		run("docker build -t ghcr.io/acme/b:1 ."),
	)}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	g := gates[0]
	if g.At.Step != 0 || g.Before || len(g.Builds) != 1 || len(g.Images) != 1 || g.Targets != nil {
		t.Fatalf("gate: %+v", g)
	}
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"web", "source"}) {
		t.Fatalf("planned: %v", got)
	}
	if countCode(pl.notes, "BUILD_AFTER_PUSH") != 1 {
		t.Fatalf("notes: %v", pl.notes)
	}
	n := pl.notes[slices.IndexFunc(pl.notes, func(n plan.Note) bool { return n.Code == "BUILD_AFTER_PUSH" })]
	if n.File != ciFile || n.Line != 300 {
		t.Fatalf("BUILD_AFTER_PUSH location: %+v", n)
	}

	// A push before the first build does not count; neither does one in the same step before
	// the build.
	jobs = []plan.Job{mkJob(t, r, "docker",
		run("docker push ghcr.io/acme/old:1"),
		run("npm publish && docker build -t ghcr.io/acme/a:1 . && docker push ghcr.io/acme/a:1"),
	)}
	gates, _, pl = planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.Before || g.At.Step != 1 || g.At.Command != 1 || len(g.Images) != 1 || hasCode(pl.notes, "BUILD_AFTER_PUSH") {
		t.Fatalf("push before the build: %+v, notes %v", g, pl.notes)
	}
}

func TestGatesPlacementMultiPlatform(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "dash")}
	jobs := []plan.Job{mkJob(t, r, "image",
		fstep{uses: "actions/checkout@v5"},
		run("docker buildx build --platform linux/amd64,linux/arm64 --push -t ghcr.io/acme/dash:1 ."),
	)}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	g := gates[0]
	if !g.Before || g.At.Step != 1 || len(g.Builds) != 0 || len(g.Images) != 0 || g.Targets != nil {
		t.Fatalf("gate: %+v", g)
	}
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"source"}) || !hasCode(pl.notes, "MULTI_PLATFORM") || hasCode(pl.notes, "BUILD_AFTER_PUSH") {
		t.Fatalf("planned %v, notes %v", got, pl.notes)
	}

	// A registry output: PUSH_IN_BUILD. A later build of the project comes after that push.
	jobs = []plan.Job{mkJob(t, r, "image",
		run("docker build -t a ."),
		run("docker buildx build -o type=registry -t b ."),
		run("docker build -t c ."),
	)}
	gates, planned, pl = planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.Before || g.At.Step != 0 || len(g.Images) != 1 {
		t.Fatalf("gate: %+v", g)
	}
	if !hasCode(pl.notes, "PUSH_IN_BUILD") || countCode(pl.notes, "BUILD_AFTER_PUSH") != 1 || len(planned["."]) != 2 {
		t.Fatalf("notes %v, planned %v", pl.notes, planned["."])
	}
}

func TestGatesPushInBuild(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n", "apps/b/Dockerfile": "FROM x\n"})
	// The project's own gateable --push moves after the gate, so it is not a push here.
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{mkJob(t, r, "image",
		run("docker buildx build --push -t ghcr.io/acme/web:1 ."),
		run("docker build -t ghcr.io/acme/worker:1 ."),
	)}
	gates, _, pl := planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.At.Step != 1 || len(g.Images) != 2 || hasCode(pl.notes, "BUILD_AFTER_PUSH") {
		t.Fatalf("own --push: %+v, notes %v", g, pl.notes)
	}
	// Another project's --push is a push.
	projects = []plan.Project{npmProject(".", "web"), npmProject("apps/b", "b")}
	jobs = []plan.Job{mkJob(t, r, "image",
		run("docker build -t ghcr.io/acme/web:1 ."),
		run("docker buildx build --push -t ghcr.io/acme/b:1 apps/b"),
		run("docker build -t ghcr.io/acme/web2:1 ."),
	)}
	gates, _, pl = planGates(t, r, projects, jobs, nil)
	var web plan.Gate
	for _, g := range gates {
		if g.Project.Dir == "." {
			web = g
		}
	}
	if web.At.Step != 0 || len(web.Images) != 1 || countCode(pl.notes, "BUILD_AFTER_PUSH") != 1 {
		t.Fatalf("other project's --push: %+v, notes %v", web, pl.notes)
	}
}

func TestGatesPlacementCalledFileThatPushes(t *testing.T) {
	mk := "release:\n\tcargo build --release\n\tgh release upload v1 target/release/demo\n"
	r := demoRepo(t, map[string]string{"Makefile": mk})
	projects := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: "demo", Binaries: []string{"demo"}}}
	jobs := []plan.Job{mkJob(t, r, "linux", fstep{uses: "actions/checkout@v5"}, run("make release"))}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	g := gates[0]
	if !g.Before || g.At.Step != 1 || g.At.Command != 0 || len(g.Builds) != 0 || g.Targets != nil {
		t.Fatalf("gate: %+v", g)
	}
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"source"}) {
		t.Fatalf("planned: %v", got)
	}
	if countCode(pl.notes, "BUILD_AFTER_PUSH") != 1 {
		t.Fatalf("notes: %v", pl.notes)
	}
	n := pl.notes[slices.IndexFunc(pl.notes, func(n plan.Note) bool { return n.Code == "BUILD_AFTER_PUSH" })]
	if !strings.Contains(n.Message, "Makefile") || n.File != "Makefile" || n.Line != 2 {
		t.Fatalf("BUILD_AFTER_PUSH: %+v", n)
	}
	if bs := pl.attributed["."]; len(bs) != 1 {
		t.Fatalf("attributed: %v", bs)
	}
}

func TestGatesRewriteOnlyNeverMovesTheGate(t *testing.T) {
	r := demoRepo(t, map[string]string{"Makefile": rustMake, "Dockerfile": "FROM x\n"})
	projects := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: "demo", Binaries: []string{"demo"}}}
	jobs := []plan.Job{mkJob(t, r, "ci", run("docker build -t ghcr.io/acme/demo:1 ."), run("make test"), run("docker push ghcr.io/acme/demo:1"))}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.At.Step != 0 || g.Before || len(g.Builds) != 1 {
		t.Fatalf("gate: %+v", g)
	}
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"demo", "source"}) {
		t.Fatalf("planned: %v", got)
	}
	if bs := pl.attributed["."]; len(bs) != 2 || !bs[1].RewriteOnly {
		t.Fatalf("attributed: %v", bs)
	}
}

func TestGatesDuplicateViaBuilds(t *testing.T) {
	r := demoRepo(t, map[string]string{"Makefile": rustMake, "scripts/twice.sh": "make release\nmake release\n"})
	projects := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: "demo", Binaries: []string{"demo"}}}
	jobs := []plan.Job{mkJob(t, r, "ci", run("./scripts/twice.sh"))}
	if n := len(jobs[0].Steps[0].Builds); n != 2 {
		t.Fatalf("the classifier gives %d builds", n)
	}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	if len(gates[0].Builds) != 1 || len(pl.attributed["."]) != 1 {
		t.Fatalf("duplicates: %+v %v", gates[0].Builds, pl.attributed)
	}
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"demo", "source"}) {
		t.Fatalf("planned: %v", got)
	}
}

func TestGatesExistingGate(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{mkJob(t, r, "ci",
		run("docker build -t ghcr.io/acme/web:1 ."),
		run(`"$HOME/.local/bin/shearbolt" scan --ci`),
		run("docker build -t ghcr.io/acme/web2:1 ."),
	)}
	gates, _, pl := planGates(t, r, projects, jobs, nil)
	g := gates[0]
	if g.Existing == nil || g.At.Step != 1 || len(g.Builds) != 1 || g.Builds[0].Pos.Step != 0 || len(g.Images) != 1 {
		t.Fatalf("gate: %+v", g)
	}
	if countCode(pl.notes, "BUILD_AFTER_GATE") != 1 {
		t.Fatalf("notes: %v", pl.notes)
	}

	// A gate-job question defaults to the job that already holds the project's gate.
	projects = []plan.Project{npmProject("apps/a", "a"), npmProject("apps/b", "b")}
	jobs = []plan.Job{
		mkJob(t, r, "one", run("cd apps/a && npm run build")),
		mkJob(t, r, "two", run(`"$HOME/.local/bin/shearbolt" scan --ci --config apps/b/shearbolt.yaml`)),
	}
	qs := newGateModel(projects, jobs, []string{ciFile}).questions()
	if q := qs[len(qs)-1]; q.ID != "gate-job:apps/b" || q.Default != jobs[1].ID {
		t.Fatalf("gate-job default: %+v", q)
	}
	gates, _, _ = planGates(t, r, projects, jobs, nil)
	for _, g := range gates {
		if g.Project.Dir == "apps/b" && (g.Existing == nil || g.Job != jobs[1].ID) {
			t.Fatalf("existing gate-job gate: %+v", g)
		}
	}
}

func TestGatesBuildNotesLocated(t *testing.T) {
	r := repo(t, nil)
	projects := []plan.Project{npmProject(".", "blog")}
	jobs := []plan.Job{mkJob(t, r, "ci", run("echo hi\nrailpack build ."))}
	_, _, pl := planGates(t, r, projects, jobs, nil)
	i := slices.IndexFunc(pl.notes, func(n plan.Note) bool { return n.Code == "RAILPACK_NO_NAME" })
	if i < 0 || pl.notes[i].File != ciFile || pl.notes[i].Line != 101 {
		t.Fatalf("notes: %+v", pl.notes)
	}
}

func TestGatesSlotsAndTargets(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n", "worker.Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "@acme/web")}

	// Two jobs that each build "the image" share one target and scan everything.
	jobs := []plan.Job{
		mkJob(t, r, "x", run("docker build -t ghcr.io/acme/web:1 .")),
		mkJob(t, r, "y", run("docker build -t ghcr.io/acme/web:2 .")),
	}
	gates, planned, _ := planGates(t, r, projects, jobs, nil)
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"web", "source"}) {
		t.Fatalf("planned: %v", got)
	}
	if len(gates) != 2 || gates[0].Targets != nil || gates[1].Targets != nil {
		t.Fatalf("gates: %+v", gates)
	}
	if planned["."][0].Image != "${SHEARBOLT_IMAGE:-web:local}" || gates[0].Images[0].Var != "SHEARBOLT_IMAGE" ||
		gates[0].Images[0].ScanTag != "ghcr.io/acme/web:shearbolt-${{ github.sha }}" {
		t.Fatalf("image: %+v, %+v", planned["."][0], gates[0].Images)
	}

	// One job building two images: slots web and worker.
	jobs = []plan.Job{mkJob(t, r, "x", run("docker build -t ghcr.io/acme/web:1 .\ndocker build -f worker.Dockerfile -t ghcr.io/acme/worker:1 ."))}
	gates, planned, _ = planGates(t, r, projects, jobs, nil)
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"web", "worker", "source"}) {
		t.Fatalf("planned: %v", got)
	}
	if planned["."][1].Image != "${SHEARBOLT_IMAGE_WORKER:-worker:local}" {
		t.Fatalf("worker: %+v", planned["."][1])
	}
	if im := gates[0].Images; len(im) != 2 || im[0].Var != "SHEARBOLT_IMAGE" || im[1].Var != "SHEARBOLT_IMAGE_WORKER" ||
		im[1].ScanTag != "ghcr.io/acme/worker:shearbolt-${{ github.sha }}" || gates[0].Targets != nil {
		t.Fatalf("images: %+v", gates[0])
	}
}

func TestGatesTargetsPerJob(t *testing.T) {
	files := map[string]string{"Dockerfile": "FROM x\n"}
	r := demoRepo(t, files)
	projects := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: "web", Binaries: []string{"demo"}}}
	jobs := []plan.Job{
		mkJob(t, r, "x", run("cargo build --release --locked")),
		mkJob(t, r, "y", run("docker build -t ghcr.io/acme/web:1 .")),
	}
	gates, planned, _ := planGates(t, r, projects, jobs, nil)
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"web", "demo", "source"}) {
		t.Fatalf("planned: %v", got)
	}
	if !slices.Equal(gates[0].Targets, []string{"demo", "source"}) || !slices.Equal(gates[1].Targets, []string{"source", "web"}) {
		t.Fatalf("targets: %v, %v", gates[0].Targets, gates[1].Targets)
	}
	if planned["."][1].File != "target/release/demo" || planned["."][1].LockfileFallback != "Cargo.lock" {
		t.Fatalf("file target: %+v", planned["."][1])
	}

	// A hand-added target is always scanned.
	files["shearbolt.yaml"] = "version: 1\nproject: web\ntargets:\n  - name: extra\n    dir: public\n"
	r = demoRepo(t, files)
	gates, _, _ = planGates(t, r, projects, jobs, nil)
	if !slices.Equal(gates[0].Targets, []string{"demo", "extra", "source"}) || !slices.Equal(gates[1].Targets, []string{"extra", "source", "web"}) {
		t.Fatalf("with extra: %v, %v", gates[0].Targets, gates[1].Targets)
	}

	// An existing source target named repo appears as repo.
	files["shearbolt.yaml"] = "version: 1\nproject: web\ntargets:\n  - name: repo\n    source: .\n"
	r = demoRepo(t, files)
	gates, _, _ = planGates(t, r, projects, jobs, nil)
	if !slices.Equal(gates[0].Targets, []string{"demo", "repo"}) || !slices.Equal(gates[1].Targets, []string{"repo", "web"}) {
		t.Fatalf("with repo: %v, %v", gates[0].Targets, gates[1].Targets)
	}

	// A config that cannot be edited: the gates scan every target it has.
	files["shearbolt.yaml"] = "version: 1\nproject: web\ntargets: [{name: repo, source: .}]\n"
	r = demoRepo(t, files)
	gates, _, _ = planGates(t, r, projects, jobs, nil)
	if gates[0].Targets != nil || gates[1].Targets != nil || gates[1].Images[0].Var != "SHEARBOLT_IMAGE" {
		t.Fatalf("unsupported config: %+v", gates)
	}

	// A fixed image reference: the gate scans that target, without an image variable.
	files["shearbolt.yaml"] = "version: 1\nproject: web\ntargets:\n  - name: web\n    image: ghcr.io/acme/web:latest\n"
	r = demoRepo(t, files)
	gates, _, _ = planGates(t, r, projects, jobs, nil)
	if len(gates[1].Images) != 0 || !slices.Equal(gates[1].Targets, []string{"source", "web"}) {
		t.Fatalf("fixed reference: %+v", gates[1])
	}
}

func TestGatesFileTargetNames(t *testing.T) {
	r := demoRepo(t, nil)
	projects := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: "demo", Binaries: []string{"demo"}}}
	jobs := []plan.Job{
		mkJob(t, r, "x", run("cargo build --release")),
		mkJob(t, r, "y", run("cargo build --release --target aarch64-apple-darwin")),
		mkJob(t, r, "z", run("cargo build --profile dist")),
	}
	_, planned, _ := planGates(t, r, projects, jobs, nil)
	var got []string
	for _, tg := range planned["."] {
		got = append(got, tg.Name+"="+tg.File)
	}
	want := []string{"demo=target/release/demo", "demo-aarch64-apple-darwin=target/aarch64-apple-darwin/release/demo", "demo-2=target/dist/demo", "source="}
	if !slices.Equal(got, want) {
		t.Fatalf("file targets %v, want %v", got, want)
	}
	// A binary build attributed to a project of another ecosystem gives no file target.
	projects = []plan.Project{npmProject(".", "web")}
	_, planned, _ = planGates(t, r, projects, jobs[:1], nil)
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"source"}) {
		t.Fatalf("npm project: %v", got)
	}
}

func TestGatesRepositoryDerivation(t *testing.T) {
	gh := commitExprs["github"]
	for _, c := range []struct {
		img  plan.Image
		want string
	}{
		{plan.Image{Tool: "docker", Tags: []string{"ghcr.io/acme/web:latest"}}, "ghcr.io/acme/web:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "docker", Tags: []string{"$IMAGE:latest"}}, "$IMAGE:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "docker", Tags: []string{"ghcr.io/x/web:${TAG:-latest}"}}, "ghcr.io/x/web:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "docker", Tags: []string{"$TAG"}}, "web:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "docker", Tags: []string{"${{ steps.x.outputs.tag }}"}}, "web:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "docker"}, "web:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "build-push-action", Tags: []string{"${{ steps.meta.outputs.tags }}"}, Computed: "meta", ComputedRepo: "ghcr.io/acme/storefront"}, "ghcr.io/acme/storefront:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "build-push-action", Tags: []string{"${{ steps.meta.outputs.tags }}"}, Computed: "meta", ComputedRepo: "ghcr.io/${{ github.repository }}"}, "web:shearbolt-${{ github.sha }}"},
		// More forms.
		{plan.Image{Tool: "docker", Tags: []string{`"$IMAGE:${{ github.sha }}"`}}, `"$IMAGE:shearbolt-${{ github.sha }}"`},
		{plan.Image{Tool: "docker", Tags: []string{"localhost:5000/app"}}, "localhost:5000/app:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "docker", Tags: []string{"$REG/app"}}, "$REG/app:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "docker", Tags: []string{"'ghcr.io/acme/web:1'"}}, `"ghcr.io/acme/web:shearbolt-${{ github.sha }}"`},
		{plan.Image{Tool: "docker", Tags: []string{"${IMAGE}"}}, "web:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "docker", Tags: []string{"$(cat tag)"}}, "web:shearbolt-${{ github.sha }}"},
		{plan.Image{Tool: "build-push-action", Tags: []string{"ghcr.io/acme/web:latest", "ghcr.io/acme/web:1"}}, "ghcr.io/acme/web:shearbolt-${{ github.sha }}"},
		// An added tag that is already there is reused.
		{plan.Image{Tool: "docker", Tags: []string{"a:1", `"a:shearbolt-${{ github.sha }}"`}}, `"a:shearbolt-${{ github.sha }}"`},
	} {
		if got := scanTag(c.img, gh, "web"); got != c.want {
			t.Errorf("%+v: got %s, want %s", c.img, got, c.want)
		}
	}
	gl := commitExprs["gitlab"]
	if got := scanTag(plan.Image{Tool: "docker", Tags: []string{`"$CI_REGISTRY_IMAGE:$CI_COMMIT_SHA"`}}, gl, "ledger"); got != `"$CI_REGISTRY_IMAGE:shearbolt-$CI_COMMIT_SHA"` {
		t.Fatalf("gitlab quoting: %s", got)
	}
	if got := scanTag(plan.Image{Tool: "docker", Tags: []string{"$IMAGE:$BITBUCKET_COMMIT"}}, commitExprs["bitbucket"], "api"); got != "$IMAGE:shearbolt-$BITBUCKET_COMMIT" {
		t.Fatalf("bitbucket: %s", got)
	}
}

var scanStep = run(`"$HOME/.local/bin/shearbolt" scan --ci`)

func noteOf(t *testing.T, notes []plan.Note, code string) plan.Note {
	t.Helper()
	i := slices.IndexFunc(notes, func(n plan.Note) bool { return n.Code == code })
	if i < 0 {
		t.Fatalf("no %s in %v", code, notes)
	}
	return notes[i]
}

// Review fix 1: only an existing gate strictly before the first push is reused. A late one is
// left alone, a new gate is placed by rules 1-3, and GATE_AFTER_PUSH names the late gate.
func TestGatesLateExistingGate(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	build, push := run("docker build -t ghcr.io/acme/web:1 ."), run("docker push ghcr.io/acme/web:1")

	jobs := []plan.Job{mkJob(t, r, "ci", build, push, scanStep)}
	gates, _, pl := planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.Existing != nil || g.Before || g.At.Step != 0 || len(g.Builds) != 1 || len(g.Images) != 1 {
		t.Fatalf("late gate: %s", dumpGate(g))
	}
	if n := noteOf(t, pl.notes, "GATE_AFTER_PUSH"); n.File != ciFile || n.Line != 300 || !strings.Contains(n.Message, ciFile+":300") {
		t.Fatalf("GATE_AFTER_PUSH: %+v", n)
	}

	// The job as the first run leaves it: the new gate is reused, the late one stays, and
	// nothing else is planned (idempotent).
	jobs = []plan.Job{mkJob(t, r, "ci", build, scanStep, push, scanStep)}
	gates, _, pl = planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.Existing == nil || g.At.Step != 1 || len(g.Builds) != 1 || hasCode(pl.notes, "GATE_AFTER_PUSH") {
		t.Fatalf("gate before the push: %s, notes %v", dumpGate(g), pl.notes)
	}

	// Of several gates before the push, the last one is reused: not the first, nor the last
	// one overall.
	jobs = []plan.Job{mkJob(t, r, "ci", scanStep, build, scanStep, push, scanStep)}
	gates, _, pl = planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.Existing == nil || g.At.Step != 2 || len(g.Builds) != 1 || hasCode(pl.notes, "BUILD_AFTER_GATE") || hasCode(pl.notes, "GATE_AFTER_PUSH") {
		t.Fatalf("several gates: %s, notes %v", dumpGate(g), pl.notes)
	}

	// A multi-platform push with an existing gate after it: a new gate before the build.
	mp := run("docker buildx build --platform linux/amd64,linux/arm64 --push -t ghcr.io/acme/web:1 .")
	jobs = []plan.Job{mkJob(t, r, "ci", mp, scanStep)}
	gates, _, pl = planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.Existing != nil || !g.Before || g.At.Step != 0 || !hasCode(pl.notes, "GATE_AFTER_PUSH") {
		t.Fatalf("multi-platform: %s, notes %v", dumpGate(g), pl.notes)
	}
	if n := noteOf(t, pl.notes, "MULTI_PLATFORM"); !strings.Contains(n.Message, "runs before it") {
		t.Fatalf("MULTI_PLATFORM: %+v", n)
	}
	jobs = []plan.Job{mkJob(t, r, "ci", scanStep, mp, scanStep)}
	gates, _, pl = planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.Existing == nil || g.At.Step != 0 || hasCode(pl.notes, "GATE_AFTER_PUSH") {
		t.Fatalf("multi-platform re-run: %s, notes %v", dumpGate(g), pl.notes)
	}
}

// Review fix 1, rule 4: an existing gate after a deploy is not reused at the start of a job.
func TestGatesLateExistingGateAtJobStart(t *testing.T) {
	r := repo(t, nil)
	projects := []plan.Project{npmProject("apps/a", "a"), npmProject("apps/b", "b")}
	checkout := fstep{uses: "actions/checkout@v5"}
	bScan := run(`"$HOME/.local/bin/shearbolt" scan --ci --config apps/b/shearbolt.yaml`)
	gateOf := func(gates []plan.Gate, dir string) plan.Gate {
		for _, g := range gates {
			if g.Project.Dir == dir {
				return g
			}
		}
		t.Fatalf("no gate for %s", dir)
		return plan.Gate{}
	}

	jobs := []plan.Job{mkJob(t, r, "deploy", checkout, run("fly deploy"), bScan)}
	gates, _, pl := planGates(t, r, projects, jobs, nil)
	if g := gateOf(gates, "apps/b"); g.Existing != nil || g.Before || g.At.Step != 0 || g.At.Command != -1 {
		t.Fatalf("late gate at job start: %s", dumpGate(g))
	}
	if n := noteOf(t, pl.notes, "GATE_AFTER_PUSH"); n.Line != 300 {
		t.Fatalf("GATE_AFTER_PUSH: %+v", n)
	}
	// Re-run: the gate after checkout is reused.
	jobs = []plan.Job{mkJob(t, r, "deploy", checkout, bScan, run("fly deploy"), bScan)}
	gates, _, pl = planGates(t, r, projects, jobs, nil)
	if g := gateOf(gates, "apps/b"); g.Existing == nil || g.At.Step != 1 || hasCode(pl.notes, "GATE_AFTER_PUSH") {
		t.Fatalf("re-run: %s, notes %v", dumpGate(g), pl.notes)
	}

	// Something pushes before the checkout step: the gate goes before the first step.
	jobs = []plan.Job{mkJob(t, r, "deploy", run("docker push ghcr.io/acme/x:1"), checkout, run("echo hi"))}
	gates, _, _ = planGates(t, r, projects, jobs, nil)
	if g := gateOf(gates, "apps/b"); !g.Before || g.At.Step != 0 || g.At.Command != -1 {
		t.Fatalf("push before checkout: %s", dumpGate(g))
	}
}

// Review fix 1: notes never claim the gate runs before a build it follows.
func TestGatesMultiPlatformBeforeGateNote(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{mkJob(t, r, "ci",
		run("docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/acme/m:1 ."),
		run("docker build -t ghcr.io/acme/web:1 ."),
	)}
	gates, _, pl := planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.Before || g.At.Step != 1 {
		t.Fatalf("gate: %s", dumpGate(g))
	}
	if n := noteOf(t, pl.notes, "MULTI_PLATFORM"); strings.Contains(n.Message, "runs before it") {
		t.Fatalf("MULTI_PLATFORM claims the gate runs before it: %q", n.Message)
	}
}

func dumpGate(g plan.Gate) string {
	var bs []string
	for _, b := range g.Builds {
		bs = append(bs, fmt.Sprintf("%d/%d", b.Pos.Step, b.Pos.Command))
	}
	var iv []string
	for _, i := range g.Images {
		iv = append(iv, i.Var+"="+i.ScanTag)
	}
	return fmt.Sprintf("proj=%s at=%d/%d before=%v existing=%v builds=%v images=%v targets=%v", g.Project.Dir, g.At.Step, g.At.Command, g.Before, g.Existing != nil, bs, iv, g.Targets)
}

// Review fix 2: a job with only other builds of a project is a gate job too when a push follows
// one of them, even though the project has image builds elsewhere.
func TestGatesPublishingBuildOtherJob(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{
		mkJob(t, r, "release", run("npm run build && npm publish")),
		mkJob(t, r, "image", run("docker build -t ghcr.io/acme/web:1 ."), run("docker push ghcr.io/acme/web:1")),
		mkJob(t, r, "test", run("npm ci && npm run build"), run("npm test")),
		mkJob(t, r, "late", run("npm publish && npm run build")),
	}
	gates, _, _ := planGates(t, r, projects, jobs, nil)
	var got []string
	for _, g := range gates {
		got = append(got, g.Job)
	}
	if !slices.Equal(got, []string{jobs[0].ID, jobs[1].ID}) {
		t.Fatalf("gate jobs: %v", got)
	}
	if g := gates[0]; g.Before || g.At.Step != 0 || g.At.Command != 0 || len(g.Builds) != 1 || len(g.Images) != 0 || !slices.Equal(g.Targets, []string{"source"}) {
		t.Fatalf("release gate: %s", dumpGate(g))
	}
	if g := gates[1]; g.At.Step != 0 || len(g.Images) != 1 || !slices.Equal(g.Targets, nil) {
		t.Fatalf("image gate: %s", dumpGate(g))
	}
}

// Review fix 3: PUSH_BEFORE_GATE when a push runs before the first gated build, ahead of the gate.
func TestGatesPushBeforeGateNote(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{mkJob(t, r, "ci", run("npm publish"), run("docker build -t ghcr.io/acme/web:1 ."), run("docker push ghcr.io/acme/web:1"))}
	_, _, pl := planGates(t, r, projects, jobs, nil)
	if n := noteOf(t, pl.notes, "PUSH_BEFORE_GATE"); n.File != ciFile || n.Line != 200 || !strings.Contains(n.Message, "npm publish") {
		t.Fatalf("PUSH_BEFORE_GATE: %+v", n)
	}
	// An existing gate before that push checks it: no note.
	jobs = []plan.Job{mkJob(t, r, "ci", scanStep, run("npm publish"), run("docker build -t ghcr.io/acme/web:1 ."), run("docker push ghcr.io/acme/web:1"))}
	_, _, pl = planGates(t, r, projects, jobs, nil)
	if hasCode(pl.notes, "PUSH_BEFORE_GATE") {
		t.Fatalf("gated push: %v", pl.notes)
	}
	// No push before the build: no note.
	jobs = []plan.Job{mkJob(t, r, "ci", run("docker build -t ghcr.io/acme/web:1 ."), run("docker push ghcr.io/acme/web:1"))}
	if _, _, pl = planGates(t, r, projects, jobs, nil); hasCode(pl.notes, "PUSH_BEFORE_GATE") {
		t.Fatalf("no early push: %v", pl.notes)
	}
}

// Review fix 3: COMMANDS_BEFORE_GATE when commands or steps between gated builds run before the
// gate, which follows the last of them.
func TestGatesCommandsBeforeGateNote(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n", "worker/Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{mkJob(t, r, "ci",
		run("docker build -t ghcr.io/acme/web:1 ."),
		run("aws s3 sync dist s3://bucket"),
		fstep{uses: "acme/notify-action@v1"},
		run("docker build -t ghcr.io/acme/worker:1 worker"),
	)}
	gates, _, pl := planGates(t, r, projects, jobs, nil)
	if g := gates[0]; g.At.Step != 3 || len(g.Builds) != 2 {
		t.Fatalf("gate: %s", dumpGate(g))
	}
	n := noteOf(t, pl.notes, "COMMANDS_BEFORE_GATE")
	if n.File != ciFile || !strings.Contains(n.Message, "aws s3 sync dist s3://bucket") || !strings.Contains(n.Message, "acme/notify-action@v1") {
		t.Fatalf("COMMANDS_BEFORE_GATE: %+v", n)
	}
	// With an existing gate, commands between the last gated build and that gate count too.
	jobs = []plan.Job{mkJob(t, r, "ci", run("docker build -t ghcr.io/acme/web:1 ."), run("aws s3 sync dist s3://bucket"), scanStep)}
	if _, _, pl = planGates(t, r, projects, jobs, nil); !hasCode(pl.notes, "COMMANDS_BEFORE_GATE") {
		t.Fatalf("existing gate: %v", pl.notes)
	}

	// Shell state, setup actions, other builds and a single gated build give no note.
	for _, steps := range [][]fstep{
		{run("docker build -t ghcr.io/acme/web:1 . && cd worker && export X=1 && docker build -t ghcr.io/acme/worker:1 .")},
		{run("docker build -t ghcr.io/acme/web:1 ."), fstep{uses: "docker/login-action@v3"}, run("npm run build"), run("docker build -t ghcr.io/acme/worker:1 worker")},
		{run("docker build -t ghcr.io/acme/web:1 ."), run("aws s3 sync dist s3://bucket")},
	} {
		jobs = []plan.Job{mkJob(t, r, "ci", steps...)}
		if _, _, pl = planGates(t, r, projects, jobs, nil); hasCode(pl.notes, "COMMANDS_BEFORE_GATE") {
			t.Errorf("%v: %v", steps, pl.notes)
		}
	}
}

// Review 4: a build reached first outside the invoked targets (make test) and then inside them
// (make release) is not a duplicate: the reachable one stays gated.
func TestGatesDedupeKeepsReachedBuild(t *testing.T) {
	r := demoRepo(t, map[string]string{"Makefile": rustMake, "scripts/ci.sh": "make test\nmake release\n"})
	projects := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: "demo", Binaries: []string{"demo"}}}
	jobs := []plan.Job{mkJob(t, r, "ci", run("./scripts/ci.sh"))}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	if g := gates[0]; len(g.Builds) != 1 || g.Builds[0].RewriteOnly || g.Before {
		t.Fatalf("gate: %s", dumpGate(g))
	}
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"demo", "source"}) || len(pl.attributed["."]) != 2 {
		t.Fatalf("planned %v, attributed %d", got, len(pl.attributed["."]))
	}
}

// Review 4: builds of unselected projects are ignored: not prepared, no notes.
func TestGatesUnselectedProjectIgnored(t *testing.T) {
	r := demoRepo(t, map[string]string{"apps/a/Dockerfile": "FROM x\n", "Makefile": rustMake})
	projects := []plan.Project{npmProject("apps/a", "a"), {Ecosystem: "rust", Dir: ".", Name: "demo", Binaries: []string{"demo"}}}
	jobs := []plan.Job{mkJob(t, r, "ci", run("docker build -t a apps/a"), run("cargo build --release --target-dir $X"), run("make test"))}
	m := newGateModel(projects, jobs, []string{ciFile})
	pl := m.place(resolve(t, m.questions(), map[string]string{"projects": "apps/a"}), "1.2.3", fakeSnippet)
	if len(pl.attributed["."]) != 0 || hasCode(pl.notes, "RUST_PATH_UNKNOWN") || pl.projects["."] != nil {
		t.Fatalf("unselected: attributed %v, notes %v", pl.attributed["."], pl.notes)
	}
	if len(pl.attributed["apps/a"]) != 1 {
		t.Fatalf("selected: %v", pl.attributed["apps/a"])
	}
}

// Review 4: finish finds an image target's variable by its name in the file, also when the
// planned target was renamed or matched by value.
func TestGatesImageVarByMergedName(t *testing.T) {
	projects := []plan.Project{npmProject(".", "web")}
	for name, cfg := range map[string]string{
		"kind collision": "version: 1\nproject: web\ntargets:\n  - name: web\n    dir: public\n  - name: source\n    source: .\n",
		"value match":    "version: 1\nproject: web\ntargets:\n  - name: app\n    image: ${SHEARBOLT_IMAGE:-web:local}\n  - name: source\n    source: .\n",
	} {
		r := repo(t, map[string]string{"Dockerfile": "FROM x\n", "shearbolt.yaml": cfg})
		jobs := []plan.Job{mkJob(t, r, "ci", run("docker build -t ghcr.io/acme/web:1 ."), run("docker push ghcr.io/acme/web:1"))}
		gates, _, _ := planGates(t, r, projects, jobs, nil)
		if g := gates[0]; len(g.Images) != 1 || g.Images[0].Var != "SHEARBOLT_IMAGE" || g.Targets != nil {
			t.Errorf("%s: %s", name, dumpGate(g))
		}
	}
}

// Review 4: image variables that collide after upper-casing get _2.
func TestGatesImageVarCollision(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{mkJob(t, r, "ci", run("docker build -t ghcr.io/x/web:1 .\ndocker build -t ghcr.io/x/a.b:1 .\ndocker build -t ghcr.io/x/a-b:1 ."))}
	gates, planned, _ := planGates(t, r, projects, jobs, nil)
	var got []string
	for _, tg := range planned["."] {
		got = append(got, tg.Name+"="+tg.Image)
	}
	want := []string{"web=${SHEARBOLT_IMAGE:-web:local}", "a.b=${SHEARBOLT_IMAGE_A_B:-a.b:local}", "a-b=${SHEARBOLT_IMAGE_A_B_2:-a-b:local}", "source="}
	if !slices.Equal(got, want) {
		t.Fatalf("planned %v, want %v", got, want)
	}
	if im := gates[0].Images; len(im) != 3 || im[2].Var != "SHEARBOLT_IMAGE_A_B_2" {
		t.Fatalf("images: %s", dumpGate(gates[0]))
	}
}

// Review 5: metadata-action lower-cases its images entries, and build-push-action rejects upper
// case, so the added tag uses the lower-cased repository.
func TestGatesMetadataRepositoryLowerCase(t *testing.T) {
	img := plan.Image{Tool: "build-push-action", Tags: []string{"${{ steps.meta.outputs.tags }}"}, Computed: "meta", ComputedRepo: "ghcr.io/Acme/Web"}
	if got := scanTag(img, commitExprs["github"], "web"); got != "ghcr.io/acme/web:shearbolt-${{ github.sha }}" {
		t.Fatalf("scan tag: %s", got)
	}
}

// Two gated image builds of one repository in a job (per-platform builds, say) must not share a
// scan tag: the second build would overwrite the first, and the gate would scan it twice. The
// later one gets the slot number in its tag; a re-run reuses the tags already added.
func TestGatesScanTagsUniqueInGate(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject(".", "web")}
	jobs := []plan.Job{mkJob(t, r, "ci", run(`docker build --platform linux/amd64 -t "$IMG:amd64" .`+"\n"+`docker build --platform linux/arm64 -t "$IMG:arm64" -f Dockerfile .`))}
	gates, _, _ := planGates(t, r, projects, jobs, nil)
	im := gates[0].Images
	if len(im) != 2 || im[0].ScanTag != `"$IMG:shearbolt-${{ github.sha }}"` || im[1].ScanTag != `"$IMG:shearbolt-2-${{ github.sha }}"` {
		t.Fatalf("images: %s", dumpGate(gates[0]))
	}
	jobs = []plan.Job{mkJob(t, r, "ci", run(`docker build -t "$IMG:amd64" -t "$IMG:shearbolt-${{ github.sha }}" .`+"\n"+`docker build -t "$IMG:arm64" -t "$IMG:shearbolt-2-${{ github.sha }}" -f Dockerfile .`))}
	gates, _, _ = planGates(t, r, projects, jobs, nil)
	if im := gates[0].Images; len(im) != 2 || im[1].ScanTag != `"$IMG:shearbolt-2-${{ github.sha }}"` {
		t.Fatalf("re-run images: %s", dumpGate(gates[0]))
	}
}

// Batch 4 (skill review): the build-project default is the project whose existing gate follows
// the build in its job, so a re-run with the defaults keeps an earlier answer.
func TestGatesBuildProjectDefaultFollowsExistingGate(t *testing.T) {
	r := repo(t, map[string]string{"docker/Dockerfile": "FROM x\n"})
	projects := []plan.Project{npmProject("apps/a", "a"), npmProject("apps/b", "b")}
	jobs := []plan.Job{mkJob(t, r, "image",
		run("docker build -f docker/Dockerfile -t x:1 docker"),
		run(`"$HOME/.local/bin/shearbolt" scan --ci --config apps/b/shearbolt.yaml`),
		run("docker push x:1"),
	)}
	qs := newGateModel(projects, jobs, []string{ciFile}).questions()
	i := slices.IndexFunc(qs, func(q plan.Question) bool { return strings.HasPrefix(q.ID, "build-project:") })
	if i < 0 || qs[i].Default != "apps/b" {
		t.Fatalf("questions: %+v", qs)
	}
	// A gate before the build says nothing about it.
	jobs = []plan.Job{mkJob(t, r, "image",
		run(`"$HOME/.local/bin/shearbolt" scan --ci --config apps/b/shearbolt.yaml`),
		run("docker build -f docker/Dockerfile -t x:1 docker"),
	)}
	qs = newGateModel(projects, jobs, []string{ciFile}).questions()
	if i := slices.IndexFunc(qs, func(q plan.Question) bool { return strings.HasPrefix(q.ID, "build-project:") }); i < 0 || qs[i].Default != "apps/a" {
		t.Fatalf("questions: %+v", qs)
	}
}

// composeOnly is the compose-only project attachCompose makes for compose files that no npm or
// Rust project contains.
func composeOnly(files ...string) plan.Project {
	return plan.Project{Ecosystem: "docker", Dir: ".", Name: "repo", Lockfiles: []string{}, Compose: files}
}

// A compose-only project never owns builds. With an npm project in web/ and a root compose file,
// attribution and web's gate are those without the compose file; the only new questions are
// projects and the compose-only project's gate-job.
func TestGatesComposeOnlyOwnsNoBuilds(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n", "docker-compose.yml": composeImage})
	web := npmProject("web", "web")
	jobs := []plan.Job{mkJob(t, r, "ci", fstep{uses: "actions/checkout@v5"}, run("docker build -t ghcr.io/acme/web:1 ."), run("cd web && npm run build"))}
	if qs := newGateModel([]plan.Project{web}, jobs, []string{ciFile}).questions(); len(qs) != 0 {
		t.Fatalf("without compose: %v", questionIDs(qs))
	}
	before, _, plBefore := planGates(t, r, []plan.Project{web}, jobs, nil)

	projects := []plan.Project{composeOnly("docker-compose.yml"), web}
	qs := newGateModel(projects, jobs, []string{ciFile}).questions()
	if got := questionIDs(qs); !slices.Equal(got, []string{"projects", "gate-job:."}) {
		t.Fatalf("questions: %v", got)
	}
	if qs[0].Default != ".,web" || qs[0].Options[0].Label != ". (docker)" {
		t.Fatalf("projects: %+v", qs[0])
	}
	if gj := qs[1]; gj.Prompt != "Which job should check the compose images of repo?" || gj.UnlessChosen != nil || gj.Project != "." || gj.Default != jobs[0].ID {
		t.Fatalf("gate-job:.: %+v", gj)
	}

	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	var webGates, wantGates []string
	var dot []plan.Gate
	for _, g := range gates {
		if g.Project.Dir == "web" {
			webGates = append(webGates, dumpGate(g))
		} else {
			dot = append(dot, g)
		}
	}
	for _, g := range before {
		wantGates = append(wantGates, dumpGate(g))
	}
	if !slices.Equal(webGates, wantGates) {
		t.Fatalf("web's gates %v, want %v", webGates, wantGates)
	}
	pos := func(bs []plan.Build) []plan.Pos {
		var out []plan.Pos
		for _, b := range bs {
			out = append(out, b.Pos)
		}
		return out
	}
	if got, want := pos(pl.attributed["web"]), pos(plBefore.attributed["web"]); len(want) != 2 || !slices.Equal(got, want) || len(pl.attributed["."]) != 0 {
		t.Fatalf("attributed %v, want web %v", pl.attributed, want)
	}
	if len(dot) != 1 || dot[0].Job != jobs[0].ID || dot[0].At.Step != 0 || dot[0].Before || len(dot[0].Builds) != 0 {
		t.Fatalf("compose-only gate: %+v", dot)
	}
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"docker-compose"}) {
		t.Fatalf("compose-only targets: %v", got)
	}
}

// Build-project questions offer only npm and Rust projects, and an existing gate of the
// compose-only project after the build does not become the default.
func TestGatesComposeOnlyNotABuildOption(t *testing.T) {
	r := repo(t, map[string]string{"docker/Dockerfile": "FROM x\n", "compose.yml": composeImage})
	projects := []plan.Project{composeOnly("compose.yml"), npmProject("apps/a", "a"), npmProject("apps/b", "b")}
	bScan := run(`"$HOME/.local/bin/shearbolt" scan --ci --config apps/b/shearbolt.yaml`)
	jobs := []plan.Job{mkJob(t, r, "ci", run(`cd "$X" && npm run build`), scanStep)}
	qs := newGateModel(projects, jobs, []string{ciFile}).questions()
	bp := "build-project:" + ciFile + ":100"
	if got := questionIDs(qs); !slices.Equal(got, []string{"projects", bp, "gate-job:.", "gate-job:apps/a", "gate-job:apps/b"}) {
		t.Fatalf("questions: %v", got)
	}
	var opts []string
	for _, o := range qs[1].Options {
		opts = append(opts, o.Value)
	}
	if !slices.Equal(opts, []string{"apps/a", "apps/b", "none"}) || qs[1].Default != "apps/a" {
		t.Fatalf("build-project: %+v", qs[1])
	}
	if gj := qs[2]; gj.UnlessChosen != nil || gj.Default != jobs[0].ID {
		t.Fatalf("gate-job:.: %+v", gj)
	}
	if gj := qs[3]; !slices.Equal(gj.UnlessChosen, []string{bp}) {
		t.Fatalf("gate-job:apps/a: %+v", gj)
	}

	// The compose-only project's gate is passed over for the next gate of an npm project.
	jobs = []plan.Job{mkJob(t, r, "ci", run(`cd "$X" && npm run build`), scanStep, bScan)}
	qs = newGateModel(projects, jobs, []string{ciFile}).questions()
	if i := slices.IndexFunc(qs, func(q plan.Question) bool { return q.ID == bp }); i < 0 || qs[i].Default != "apps/b" {
		t.Fatalf("questions: %+v", qs)
	}

	// A build in a directory no npm project contains is asked about too: the compose-only
	// project at the root does not take it by prefix.
	jobs = []plan.Job{mkJob(t, r, "image", run("docker build -f docker/Dockerfile -t x:1 docker"))}
	if got := questionIDs(newGateModel(projects, jobs, []string{ciFile}).questions()); !slices.Contains(got, bp) {
		t.Fatalf("questions: %v", got)
	}
}

// In a repository without an npm or Rust project, CI builds belong to no project, and image and
// binary builds get BUILD_NOT_GATED; the compose-only project's gate goes at the start of the
// chosen job, after checkout and before the push.
func TestGatesComposeOnlyRepoWithBuild(t *testing.T) {
	r := repo(t, map[string]string{"Dockerfile": "FROM x\n", "compose.yml": composeImage})
	projects := []plan.Project{composeOnly("compose.yml")}
	jobs := []plan.Job{mkJob(t, r, "deploy", fstep{uses: "actions/checkout@v5"}, run("docker build -t ghcr.io/acme/app:1 ."),
		run("cargo build --release"), run("npm run build"), run("docker push ghcr.io/acme/app:1"))}
	qs := newGateModel(projects, jobs, []string{ciFile}).questions()
	if len(qs) != 1 || qs[0].ID != "gate-job:." || qs[0].Prompt != "Which job should check the compose images of repo?" || qs[0].UnlessChosen != nil {
		t.Fatalf("questions: %+v", qs)
	}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	if len(gates) != 1 {
		t.Fatalf("gates: %+v", gates)
	}
	if g := gates[0]; g.Job != jobs[0].ID || g.At.Step != 0 || g.At.Command != -1 || g.Before || len(g.Builds) != 0 || len(g.Images) != 0 || g.Targets != nil {
		t.Fatalf("gate: %s", dumpGate(g))
	}
	if got := planned["."]; len(got) != 1 || got[0].Name != "compose" || got[0].Compose != "compose.yml" || pl.projects["."].source != "" {
		t.Fatalf("planned: %+v", got)
	}
	// The image and the binary build get BUILD_NOT_GATED; the npm build (other) does not.
	got := noteCodes(pl.notes, "BUILD_NOT_GATED")
	want := []plan.Note{
		{Code: "BUILD_NOT_GATED", File: ciFile, Line: 200, Message: "this build belongs to no npm or Rust project, so the gate does not scan what it builds; if a compose service runs this image, add the service to its compose target's exclude list"},
		{Code: "BUILD_NOT_GATED", File: ciFile, Line: 300, Message: "this build belongs to no npm or Rust project, so the gate does not scan what it builds"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("BUILD_NOT_GATED: %+v", got)
	}
	if n := noteOf(t, pl.notes, "NO_BUILD_FOUND"); n.File != ciFile || n.Message != "No build belongs to repo, so the gate in ci.yml: deploy checks its compose images." {
		t.Fatalf("NO_BUILD_FOUND: %+v", n)
	}
	if len(pl.attributed["."]) != 0 {
		t.Fatalf("attributed: %v", pl.attributed)
	}
}

// A compose-only project's gate-job defaults to the first job that pushes or deploys, so the
// deploy is gated; an existing gate of the project still decides first (re-runs keep it). Other
// projects keep the first job.
func TestGatesComposeGateJobDefault(t *testing.T) {
	r := repo(t, map[string]string{"compose.yml": composeImage})
	checkout := fstep{uses: "actions/checkout@v5"}
	projects := []plan.Project{composeOnly("compose.yml"), npmProject("apps/a", "a")}
	jobs := []plan.Job{
		mkJob(t, r, "test", checkout, run("docker compose up -d")),
		mkJob(t, r, "deploy", checkout, run("docker stack deploy -c compose.yml app")),
	}
	qs := newGateModel(projects, jobs, []string{ciFile}).questions()
	if got := questionIDs(qs); !slices.Equal(got, []string{"projects", "gate-job:.", "gate-job:apps/a"}) {
		t.Fatalf("questions: %v", got)
	}
	if qs[1].Default != jobs[1].ID || qs[2].Default != jobs[0].ID {
		t.Fatalf("defaults: %s, %s", qs[1].Default, qs[2].Default)
	}
	gates, _, _ := planGates(t, r, projects[:1], jobs, nil)
	if len(gates) != 1 || gates[0].Job != jobs[1].ID || gates[0].At.Step != 0 || gates[0].Before {
		t.Fatalf("gates: %+v", gates)
	}

	// An existing gate of the compose-only project in test keeps it there.
	jobs[0] = mkJob(t, r, "test", checkout, scanStep, run("docker compose up -d"))
	qs = newGateModel(projects[:1], jobs, []string{ciFile}).questions()
	if len(qs) != 1 || qs[0].Default != jobs[0].ID {
		t.Fatalf("existing gate: %+v", qs)
	}
	if gates, _, _ = planGates(t, r, projects[:1], jobs, nil); len(gates) != 1 || gates[0].Job != jobs[0].ID || gates[0].Existing == nil {
		t.Fatalf("existing gate: %+v", gates)
	}
}

// NO_DEPLOY_SEEN for a project with compose targets when no CI job pushes or deploys.
func TestComputeNoDeploySeen(t *testing.T) {
	ci := &fakeCI{files: []string{fakeCIFile}}
	root := writeTree(t, map[string]string{"compose.yml": composeImage, fakeCIFile: "test: docker compose up -d\n"})
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	n := noteOf(t, p.Notes, "NO_DEPLOY_SEEN")
	if n.File != "shearbolt.yaml" || countCode(p.Notes, "NO_DEPLOY_SEEN") != 1 ||
		n.Message != "Setup saw no push or deploy step in the CI jobs it could read. If a platform deploys this project on its own (for example on every push), a failed shearbolt check does not stop it: require the check before merging to the deployed branch, or trigger the deploy from CI after the gate." {
		t.Fatalf("NO_DEPLOY_SEEN: %+v", n)
	}

	// A stack deploy is a deploy, in any job: a later job without a push does not hide it.
	for _, src := range []string{
		"test: docker compose up -d\ndeploy: docker stack deploy -c compose.yml app\n",
		"deploy: docker stack deploy -c compose.yml app\ntest: docker compose up -d\n",
	} {
		root = writeTree(t, map[string]string{"compose.yml": composeImage, fakeCIFile: src})
		if p = computeWith(t, root, ci, nil); p.Error != nil || hasCode(p.Notes, "NO_DEPLOY_SEEN") {
			t.Fatalf("stack deploy %q: %+v, notes %v", src, p.Error, codes(p.Notes))
		}
	}

	// A config setup cannot edit: the planned compose targets decide.
	root = writeTree(t, map[string]string{"compose.yml": composeImage, "shearbolt.yaml": "version: 1\ntargets: []\n", fakeCIFile: "test: docker compose up -d\n"})
	if p = computeWith(t, root, ci, nil); p.Error != nil || !hasCode(p.Notes, "UNSUPPORTED_EDIT") || countCode(p.Notes, "NO_DEPLOY_SEEN") != 1 {
		t.Fatalf("unsupported config: %+v, notes %v", p.Error, codes(p.Notes))
	}

	// An npm project with a compose file gets it too; one without gets nothing.
	root = writeTree(t, npmTree(map[string]string{"compose.yml": composeImage}))
	if p = computeWith(t, root, ci, nil); p.Error != nil || countCode(p.Notes, "NO_DEPLOY_SEEN") != 1 {
		t.Fatalf("npm with compose: %+v, notes %v", p.Error, codes(p.Notes))
	}
	root = writeTree(t, npmTree(nil))
	if p = computeWith(t, root, ci, nil); p.Error != nil || hasCode(p.Notes, "NO_DEPLOY_SEEN") {
		t.Fatalf("npm: %+v, notes %v", p.Error, codes(p.Notes))
	}

	// Compose targets are written only into a new config: an existing one without a compose
	// target scans no compose file, so there is no deploy to warn about.
	root = writeTree(t, npmTree(map[string]string{"compose.yml": composeImage, "shearbolt.yaml": "version: 1\ntargets:\n  - name: source\n    source: .\n", fakeCIFile: "build: npm ci; npm run build\n"}))
	if p = computeWith(t, root, ci, nil); p.Error != nil || !hasCode(p.Notes, "COMPOSE_NOT_GATED") || hasCode(p.Notes, "NO_DEPLOY_SEEN") {
		t.Fatalf("existing config: %+v, notes %v", p.Error, codes(p.Notes))
	}
}

// A compose target is named after the file's path without its extension, with "/" as "-";
// collisions are numbered. Compose targets come after file targets and before source, and a
// compose-only project has no source target.
func TestGatesComposeTargetNames(t *testing.T) {
	pp := &projectPlan{project: composeOnly("docker-compose.yml", "test/compose.yml", "compose.yml", "compose.yaml")}
	pp.plan()
	var got []string
	for _, tg := range pp.targets {
		got = append(got, tg.Name+"="+tg.Compose)
	}
	want := []string{"docker-compose=docker-compose.yml", "test-compose=test/compose.yml", "compose=compose.yml", "compose-2=compose.yaml"}
	if !slices.Equal(got, want) || pp.source != "" {
		t.Fatalf("targets %v (source %q), want %v", got, pp.source, want)
	}

	p := npmProject(".", "web")
	p.Compose = []string{"compose.yml"}
	pp = &projectPlan{project: p}
	pp.plan()
	if got := targetNames(pp.targets); !slices.Equal(got, []string{"compose", "source"}) || pp.source != "source" {
		t.Fatalf("npm targets: %v", got)
	}
}

// Compose targets are scanned by every gate of their project, as source is, also by gates that
// scan a subset; a compose target the config does not have is never named.
func TestGatesComposeTargetsInSubsets(t *testing.T) {
	r := demoRepo(t, map[string]string{"Dockerfile": "FROM x\n", "compose.yml": composeImage})
	projects := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: "web", Binaries: []string{"demo"}, Compose: []string{"compose.yml"}}}
	jobs := []plan.Job{
		mkJob(t, r, "x", run("cargo build --release --locked")),
		mkJob(t, r, "y", run("docker build -t ghcr.io/acme/web:1 .")),
	}
	gates, planned, pl := planGates(t, r, projects, jobs, nil)
	if got := targetNames(planned["."]); !slices.Equal(got, []string{"web", "demo", "compose", "source"}) {
		t.Fatalf("planned: %v", got)
	}
	if !slices.Equal(gates[0].Targets, []string{"compose", "demo", "source"}) || !slices.Equal(gates[1].Targets, []string{"compose", "source", "web"}) {
		t.Fatalf("targets: %v, %v", gates[0].Targets, gates[1].Targets)
	}

	pp := pl.projects["."]
	merged := slices.DeleteFunc(slices.Clone(pp.targets), func(tg config.Target) bool { return tg.Compose != "" })
	names := map[string]string{}
	for _, tg := range pp.targets {
		names[tg.Name] = tg.Name
	}
	gates = pp.finish(merged, names, map[string]string{"web": "SHEARBOLT_IMAGE"}, func(string) string { return commitExprs["github"] }, map[string]map[string]bool{})
	if !slices.Equal(gates[0].Targets, []string{"demo", "source"}) || !slices.Equal(gates[1].Targets, []string{"source", "web"}) {
		t.Fatalf("without the compose target: %v, %v", gates[0].Targets, gates[1].Targets)
	}
}

// A compose-only project has no build: the by-hand note says where the gate goes instead.
func TestGatesNoEditableJobComposeOnly(t *testing.T) {
	m := newGateModel([]plan.Project{composeOnly("compose.yml")}, nil, []string{ciFile})
	pl := m.place(resolve(t, m.questions(), nil), "1.2.3", fakeSnippet)
	n := noteOf(t, pl.notes, "NO_GATE_PLACED")
	want := "no gate could be placed automatically for "
	if !strings.HasPrefix(n.Message, want) || !strings.Contains(n.Message, "; add this by hand to a job that runs before any deploy:\n"+ciFile+":\nSNIPPET("+ciFile+",.)") ||
		strings.Contains(n.Message, "builds it") {
		t.Fatalf("note: %q", n.Message)
	}
}
