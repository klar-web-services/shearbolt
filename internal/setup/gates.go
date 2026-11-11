package setup

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// This file decides where gates go: R3 (attribution, questions, gate jobs), R4 (placement), R5
// (image slots, variables and scan tags) and R4 rule 8 (the targets each gate scans). Everything
// here is a pure function of the projects, the editable jobs and the answers; the CI plugins
// turn the resulting plan.Gate values into YAML. The steps run in this order:
//
//	m := newGateModel(projects, jobs, ciFiles)
//	qs := m.questions()                        // R3, before the answers are resolved
//	pl := m.place(values, version, snippet)    // R3 attribution, R4 rules 1-4 and 7
//	pp := pl.projects[p.Dir]                   // per selected project: pp.targets are planned
//	change, merged, names, vars, _ := configChange(r, p, pp.targets)
//	gates := pp.finish(merged, names, vars, commit, used) // R4 rule 8, R5; used is shared by all projects

// commitExprs are the CI systems' commit expressions, keyed by plan.CI.Name, for scan tags.
var commitExprs = map[string]string{
	"github":    "${{ github.sha }}",
	"gitlab":    "$CI_COMMIT_SHA",
	"bitbucket": "$BITBUCKET_COMMIT",
}

// located is one build and where it is in the jobs.
type located struct {
	plan.Build
	i         int // index in gateModel.builds: the build's identity
	job, step int
}

// at is a position in a job: a step and a command in it (-1 = the step itself).
type at struct{ step, cmd int }

func (a at) less(b at) bool { return a.step < b.step || a.step == b.step && a.cmd < b.cmd }

func (l located) at() at { return at{l.step, l.Pos.Command} }

// gateModel holds the projects, the editable jobs and their builds with their attribution.
type gateModel struct {
	projects []plan.Project
	owners   []plan.Project // the projects that can own builds: npm and Rust; without them, the root project
	rootOwns bool           // the root project (ecosystem docker) is the owner: it owns only image builds
	jobs     []plan.Job
	ciFiles  []string
	builds   []located // every build in job order; a duplicate Via build of one command is left out
	owner    []string  // per build: its owner when attributed without a question, else ""
	question []string  // per build: its build-project question ID, else ""
}

// newGateModel attributes every build of jobs (R3) among the owners, the npm and Rust projects.
// Without them, the root project (ecosystem docker) is the one owner (rootOwns), and it owns only
// image builds: every other build belongs to no project. Where npm or Rust projects exist, the
// root project never owns builds. A build belongs to the owner whose Dir is the longest prefix of
// its directory; with one owner, every build belongs to it. A build whose directory is unknown,
// or that matches no prefix, gets a build-project question when there is more than one owner.
// RewriteOnly builds are attributed by prefix only and never get a question. A Via build found
// twice in one command (a file reached by two calls) counts once.
func newGateModel(projects []plan.Project, jobs []plan.Job, ciFiles []string) *gateModel {
	m := &gateModel{projects: projects, jobs: jobs, ciFiles: ciFiles}
	for _, p := range projects {
		if p.Ecosystem != plan.EcosystemDocker {
			m.owners = append(m.owners, p)
		}
	}
	if i := slices.IndexFunc(projects, func(p plan.Project) bool { return p.Ecosystem == plan.EcosystemDocker }); len(m.owners) == 0 && i >= 0 {
		m.owners, m.rootOwns = []plan.Project{projects[i]}, true
	}
	seen := map[string]bool{}
	for j, job := range jobs {
		for s, st := range job.Steps {
			for _, b := range st.Builds {
				if b.Via != "" {
					key := fmt.Sprint(j, s, b.Pos.Command, b.Via, b.Cmd.Start, b.RewriteOnly)
					if seen[key] {
						continue
					}
					seen[key] = true
				}
				owner := m.prefixOwner(b.Dir)
				if !b.RewriteOnly && len(m.owners) == 1 {
					owner = m.owners[0].Dir
				}
				if m.rootOwns && b.Kind != plan.BuildImage {
					owner = ""
				}
				m.builds = append(m.builds, located{Build: b, i: len(m.builds), job: j, step: s})
				m.owner = append(m.owner, owner)
				m.question = append(m.question, "")
			}
		}
	}
	// build-project questions: one per unattributable build, by file and line, with :n when
	// builds share a line.
	var open []int
	for i, b := range m.builds {
		if !b.RewriteOnly && m.owner[i] == "" && len(m.owners) > 1 {
			open = append(open, i)
		}
	}
	slices.SortStableFunc(open, func(a, b int) int {
		pa, pb := m.builds[a].Pos, m.builds[b].Pos
		if c := strings.Compare(pa.File, pb.File); c != 0 {
			return c
		}
		return pa.Line - pb.Line
	})
	for k := 0; k < len(open); {
		e := k
		p := m.builds[open[k]].Pos
		for e < len(open) && m.builds[open[e]].Pos.File == p.File && m.builds[open[e]].Pos.Line == p.Line {
			e++
		}
		for n := k; n < e; n++ {
			id := fmt.Sprintf("build-project:%s:%d", p.File, p.Line)
			if e-k > 1 {
				id += fmt.Sprintf(":%d", n-k+1)
			}
			m.question[open[n]] = id
		}
		k = e
	}
	return m
}

// prefixOwner returns the owner whose Dir is the longest prefix of dir, or "" when dir is unknown
// or no owner contains it.
func (m *gateModel) prefixOwner(dir string) string {
	if dir == "" {
		return ""
	}
	best := ""
	for _, p := range m.owners {
		if (p.Dir == "." || dir == p.Dir || strings.HasPrefix(dir, p.Dir+"/")) && (best == "" || len(p.Dir) > len(best) || best == ".") {
			best = p.Dir
		}
	}
	return best
}

// questions returns R3's questions: projects (with more than one detected project, compose-only
// included), then build-project by file and line (options: the owners and none; default: the
// owner whose existing gate follows the build, else the first owner), then gate-job by project.
// A gate-job question is asked for each project without an attributed build when there is an
// editable job; its default is the first job that already holds the project's gate, otherwise
// the first job. A compose-only project's gate-job asks which job checks its compose images; its
// default without an existing gate is the first job that pushes or deploys (then the first job),
// so the deploy is gated. No build-project answer makes it inapplicable: no build can be chosen
// for that project.
func (m *gateModel) questions() []plan.Question {
	var qs []plan.Question
	if len(m.projects) > 1 {
		q := plan.Question{ID: "projects", Prompt: "Which projects should Shearbolt protect?", Kind: "multi"}
		var dirs []string
		for _, p := range m.projects {
			q.Options = append(q.Options, plan.Option{Value: p.Dir, Label: projectOption(p)})
			dirs = append(dirs, p.Dir)
		}
		q.Default = strings.Join(dirs, ",")
		qs = append(qs, q)
	}
	var bp []string
	byID := map[string]int{}
	for i, id := range m.question {
		if id != "" {
			byID[id] = i
			bp = append(bp, id)
		}
	}
	slices.SortStableFunc(bp, func(a, b string) int {
		pa, pb := m.builds[byID[a]].Pos, m.builds[byID[b]].Pos
		if c := strings.Compare(pa.File, pb.File); c != 0 {
			return c
		}
		if pa.Line != pb.Line {
			return pa.Line - pb.Line
		}
		return byID[a] - byID[b]
	})
	for _, id := range bp {
		b := m.builds[byID[id]]
		q := plan.Question{
			ID:     id,
			Prompt: fmt.Sprintf("Which project does the build at %s:%d (%s) belong to?", b.Pos.File, b.Pos.Line, commandText(b.Build)),
			Kind:   "choice",
		}
		for _, p := range m.owners {
			q.Options = append(q.Options, plan.Option{Value: p.Dir, Label: projectOption(p)})
		}
		q.Options = append(q.Options, plan.Option{Value: "none", Label: "none (do not gate this build)"})
		q.Default = q.Options[0].Value
		if p := m.gateAfter(b); p != "" {
			q.Default = p // a re-run with the defaults keeps the earlier answer (R8)
		}
		qs = append(qs, q)
	}
	if len(m.jobs) == 0 {
		return qs
	}
	for _, p := range m.projects {
		if m.hasBuild(p.Dir) {
			continue
		}
		q := plan.Question{
			ID:           "gate-job:" + p.Dir,
			Prompt:       fmt.Sprintf("No build step was found for %s. Which job should check its lockfiles?", projectLabel(p)),
			Kind:         "choice",
			Default:      m.jobs[0].ID,
			Project:      p.Dir,
			UnlessChosen: bp,
		}
		if p.Ecosystem == plan.EcosystemDocker {
			q.Prompt = fmt.Sprintf("Which job should check the compose images of %s?", projectLabel(p))
			q.UnlessChosen = nil
		}
		def := -1
		for j, job := range m.jobs {
			q.Options = append(q.Options, plan.Option{Value: job.ID, Label: job.Label})
			if g, _ := existingGate(job, p.Dir, m.firstPush(j)); def < 0 && g != nil {
				def = j
			}
		}
		if def < 0 && p.Ecosystem == plan.EcosystemDocker {
			// Compose images ship with the deploy: the first job that pushes or deploys.
			for j := range m.jobs {
				if m.firstPush(j) != never {
					def = j
					break
				}
			}
		}
		if def >= 0 {
			q.Default = m.jobs[def].ID
		}
		qs = append(qs, q)
	}
	return qs
}

// gateAfter returns the owner of the existing scan gate that follows build b most closely in its
// job, among the gates of owners; "" when there is none. Gates of a compose-only project are
// passed over, so the default is always one of the build-project options (plan.Resolve does not
// check defaults).
func (m *gateModel) gateAfter(b located) string {
	best, bestAt := "", never
	for s, st := range m.jobs[b.job].Steps {
		for _, g := range st.Gates {
			a := at{s, g.Pos.Command}
			if g.Install || !b.at().less(a) || !a.less(bestAt) {
				continue
			}
			if slices.ContainsFunc(m.owners, func(p plan.Project) bool { return p.Dir == g.Project }) {
				best, bestAt = g.Project, a
			}
		}
	}
	return best
}

// hasBuild reports whether project dir has a build attributed without a question (RewriteOnly
// builds do not count).
func (m *gateModel) hasBuild(dir string) bool {
	for i, b := range m.builds {
		if !b.RewriteOnly && m.owner[i] == dir {
			return true
		}
	}
	return false
}

func projectOption(p plan.Project) string {
	kind := p.Ecosystem
	if p.Framework == "nextjs" {
		kind += ", Next.js"
	}
	return fmt.Sprintf("%s (%s)", p.Dir, kind)
}

// projectLabel names a project in messages: its directory, or its name for the root.
func projectLabel(p plan.Project) string {
	if p.Dir == "." && p.Name != "" {
		return p.Name
	}
	return p.Dir
}

// commandText is a short form of a build's command for prompts.
func commandText(b plan.Build) string {
	var words []string
	for _, w := range b.Cmd.Words {
		words = append(words, w.Raw)
	}
	s := strings.Join(words, " ")
	if b.Via != "" {
		s += " in " + b.Via
	}
	if r := []rune(s); len(r) > 60 {
		s = string(r[:57]) + "..."
	}
	return s
}

// never is a position after every step: the limit when nothing is pushed.
var never = at{step: 1 << 30}

// existingGate returns the last existing scan gate for project dir in job strictly before limit,
// or nil. late is the first existing scan gate of dir at or after limit (nil when none): it
// runs after a push, so it is not reused.
func existingGate(job plan.Job, dir string, limit at) (found, late *plan.GateCmd) {
	for _, st := range job.Steps {
		for i := range st.Gates {
			g := st.Gates[i]
			if g.Install || g.Project != dir {
				continue
			}
			if (at{g.Pos.Step, g.Pos.Command}).less(limit) {
				found = &g
			} else if late == nil {
				late = &g
			}
		}
	}
	return found, late
}

// lateGateNote is GATE_AFTER_PUSH for an existing gate of p that runs after a push.
func lateGateNote(p plan.Project, late *plan.GateCmd) plan.Note {
	return plan.Note{
		Code: "GATE_AFTER_PUSH",
		Message: fmt.Sprintf("the existing Shearbolt gate for %s at %s:%d runs after a push or deploy in its job, so it cannot stop what is pushed; setup adds a gate before the push and leaves this one alone. Remove it once the new gate is in place.",
			projectLabel(p), late.Pos.File, late.Pos.Line),
		File: late.Pos.File,
		Line: late.Pos.Line,
	}
}

// gatePlace is one gate before its targets are known.
type gatePlace struct {
	gate     plan.Gate
	file     string       // the CI file
	images   []plan.Build // gated image builds: slot k is images[k-1]
	binaries []plan.Build // gated binary builds
	drop     bool         // a root project's gate with nothing to scan: it is not placed
}

// placement is the result of attribution and placement for one set of answers. The gates are
// in projects; only projectPlan.finish turns them into plan.Gate values.
type placement struct {
	projects map[string]*projectPlan // per selected project dir
	// attributed holds, per selected project dir, every build attributed to it in job order,
	// duplicates removed: gated or not (after a push, other kinds, Via builds), plus RewriteOnly
	// builds attributed by prefix. It is the list Prepare is called for. Each build's Pos.Job
	// (with Via) says where its Inserts and Install go, so grouping the non-RewriteOnly builds'
	// Preps by Pos.Job gives CI.Edit its per-job preps.
	attributed map[string][]plan.Build
	notes      []plan.Note
}

// projectPlan is one selected project's gates and planned targets.
type projectPlan struct {
	project plan.Project
	gates   []*gatePlace
	targets []config.Target   // planned (R6 order): image slots, file targets, compose targets, source
	slots   []string          // planned name of image slot k at index k-1
	files   map[string]string // binary path -> planned name
	source  string            // planned name of the source target; "" for the root project (docker)
}

// place resolves attribution with the answers in values (from plan.Resolve) and places one
// gate per gate job of each selected project (R3, R4 rules 1-4 and 7). Gates get version.
// Builds attributed to selected projects contribute their notes, located at their CI command
// when they carry no location. Without an npm or Rust project, every non-RewriteOnly binary
// build, which no project owns, gets BUILD_NOT_GATED. A root project's gate with nothing to scan
// is not placed (placeIn), so all of its gates can be gone. With no editable job at all, no gate
// is placed and each selected project gets NO_GATE_PLACED with snippet's text for every CI file.
func (m *gateModel) place(values map[string]string, version string, snippet func(file string, g plan.Gate) string) *placement {
	pl := &placement{projects: map[string]*projectPlan{}, attributed: map[string][]plan.Build{}}
	selected := map[string]bool{}
	if v, ok := values["projects"]; ok {
		for _, d := range strings.Split(v, ",") {
			selected[strings.TrimSpace(d)] = true
		}
	} else {
		for _, p := range m.projects {
			selected[p.Dir] = true
		}
	}

	// Attribution, after the answers.
	owner := make([]string, len(m.builds))
	for i, b := range m.builds {
		o := m.owner[i]
		if id := m.question[i]; id != "" {
			if o = values[id]; o == "none" {
				o = ""
			}
		}
		if !selected[o] {
			continue
		}
		owner[i] = o
		pl.attributed[o] = append(pl.attributed[o], b.Build)
		for _, n := range b.Notes {
			if n.File == "" {
				n.File, n.Line = b.Pos.File, b.Pos.Line
			}
			pl.notes = append(pl.notes, n)
		}
	}
	if m.rootOwns || len(m.owners) == 0 { // no npm or Rust project
		for i, b := range m.builds {
			if b.RewriteOnly || b.Kind != plan.BuildBinary || m.owner[i] != "" {
				continue
			}
			pl.notes = append(pl.notes, plan.Note{Code: "BUILD_NOT_GATED", Message: "this build belongs to no npm or Rust project, so the gate does not scan what it builds",
				File: b.Pos.File, Line: b.Pos.Line})
		}
	}

	for _, p := range m.projects {
		if !selected[p.Dir] {
			continue
		}
		pp := &projectPlan{project: p}
		pl.projects[p.Dir] = pp
		if len(m.jobs) == 0 {
			g := plan.Gate{Project: p, Config: configFile(p), Version: version}
			where := "the job that builds it, after the build and before any push"
			if p.Ecosystem == plan.EcosystemDocker {
				where = "a job that runs before any deploy"
			}
			msg := fmt.Sprintf("no gate could be placed automatically for %s; add this by hand to %s:", projectLabel(p), where)
			for _, f := range m.ciFiles {
				msg += "\n" + f + ":\n" + snippet(f, g)
			}
			pl.notes = append(pl.notes, plan.Note{Code: "NO_GATE_PLACED", Message: msg})
			pp.plan()
			continue
		}
		// Gate jobs: those with its image or binary builds, plus those with only its other
		// builds when a push follows one of them; without image or binary builds, those with its
		// other builds; else the gate-job answer. kinds[j] says which builds are gated in job j.
		artifact := []plan.BuildKind{plan.BuildImage, plan.BuildBinary}
		other := []plan.BuildKind{plan.BuildOther}
		mine := func(b located, kinds []plan.BuildKind) bool {
			return owner[b.i] == p.Dir && !b.RewriteOnly && slices.Contains(kinds, b.Kind)
		}
		kinds := map[int][]plan.BuildKind{}
		for _, b := range m.builds {
			if mine(b, artifact) {
				kinds[b.job] = artifact
			}
		}
		withArtifacts := len(kinds) > 0
		firstOther := map[int]at{}
		for _, b := range m.builds {
			if _, seen := firstOther[b.job]; mine(b, other) && !seen {
				firstOther[b.job] = b.at()
			}
		}
		for j, first := range firstOther {
			if _, ok := kinds[j]; ok {
				continue
			}
			if !withArtifacts || m.firstPushFrom(j, first).less(never) {
				kinds[j] = other
			}
		}
		gateJobs := slices.Sorted(maps.Keys(kinds))
		if len(gateJobs) == 0 {
			// The answer is a valid job ID after plan.Resolve.
			if j := slices.IndexFunc(m.jobs, func(job plan.Job) bool { return job.ID == values["gate-job:"+p.Dir] }); j >= 0 {
				gp, notes := m.placeAtStart(p, j, version)
				pp.gates = append(pp.gates, gp)
				msg := fmt.Sprintf("No build step was found for %s, so the gate in %s checks its lockfiles.", projectLabel(p), m.jobs[j].Label)
				if p.Ecosystem == plan.EcosystemDocker {
					msg = fmt.Sprintf("No build belongs to %s, so the gate in %s checks its compose images.", projectLabel(p), m.jobs[j].Label)
				}
				pl.notes = append(pl.notes, plan.Note{Code: "NO_BUILD_FOUND", Message: msg, File: gp.file})
				pl.notes = append(pl.notes, notes...)
			}
		}
		for _, j := range gateJobs {
			var g []located
			for _, b := range m.builds {
				if b.job == j && mine(b, kinds[j]) {
					g = append(g, b)
				}
			}
			gp, notes := m.placeIn(p, j, g, version)
			if !gp.drop {
				pp.gates = append(pp.gates, gp)
			}
			pl.notes = append(pl.notes, notes...)
		}
		pp.plan()
	}
	return pl
}

// jobFile returns the CI file of job j.
func (m *gateModel) jobFile(j int) string {
	id, best := m.jobs[j].ID, ""
	for _, f := range m.ciFiles {
		if strings.HasPrefix(id, f+"#") && len(f) > len(best) {
			best = f
		}
	}
	if best == "" {
		best, _, _ = strings.Cut(id, "#")
	}
	return best
}

// pushes returns the positions in job j where something is pushed or deployed: the steps' Push
// entries, and image builds that push themselves (Classified.Push leaves those out). A gateable
// push-in-build of the project being placed (own) does not count: its push moves after the gate.
func (m *gateModel) pushes(j int, own func(located) bool) []at {
	var out []at
	for s, st := range m.jobs[j].Steps {
		for _, c := range st.Push {
			out = append(out, at{s, c})
		}
	}
	for _, b := range m.builds {
		if b.job != j || b.Image == nil || !b.Image.Push && !b.Image.OutputPush {
			continue
		}
		if b.Image.Gateable() && !b.Image.OutputPush && own(b) {
			continue
		}
		out = append(out, b.at())
	}
	return out
}

// firstPush returns the position of the first push in job j, or never.
func (m *gateModel) firstPush(j int) at { return m.firstPushFrom(j, at{-1, -1}) }

// firstPushFrom returns the position of the first push in job j at or after from, or never.
func (m *gateModel) firstPushFrom(j int, from at) at {
	first := never
	for _, a := range m.pushes(j, func(located) bool { return false }) {
		if !a.less(from) && a.less(first) {
			first = a
		}
	}
	return first
}

// placeAtStart places a gate for a job without builds of the project (R4 rule 4): after the
// actions/checkout step when nothing is pushed at or before it, otherwise before the first step.
// An existing gate of the project before the job's first push is updated instead; one after it
// is left alone and gets GATE_AFTER_PUSH.
func (m *gateModel) placeAtStart(p plan.Project, j int, version string) (*gatePlace, []plan.Note) {
	job := m.jobs[j]
	file := m.jobFile(j)
	g := plan.Gate{Project: p, Config: configFile(p), Job: job.ID, Version: version,
		At: plan.Pos{File: file, Job: job.ID, Step: 0, Command: -1}, Before: true}
	first := m.firstPush(j)
	if c := slices.IndexFunc(job.Steps, func(st plan.Step) bool { return isCheckout(st.Uses) }); c >= 0 && first.step > c {
		g.At.Step, g.Before = c, false
	}
	var notes []plan.Note
	switch e, late := existingGate(job, p.Dir, first); {
	case e != nil:
		g.Existing, g.At, g.Before = e, e.Pos, false
	case late != nil:
		notes = append(notes, lateGateNote(p, late))
	}
	return &gatePlace{gate: g, file: file}, notes
}

func isCheckout(uses string) bool {
	name, _, _ := strings.Cut(uses, "@")
	return name == "actions/checkout"
}

// placeIn places project p's gate in job j, whose gated builds are g, in order (R4 rules 1-3
// and 7). firstPush is the first push at or after the first build in g; a Via build in a
// command that pushes counts as after it. B is the gateable builds of g before firstPush. The
// gate goes after the last build in B, or, when B is empty, before the first build in g and
// checks the source only. An existing gate of p strictly before firstPush is updated in place
// instead (the last one); builds of B after it get BUILD_AFTER_GATE. An existing gate after
// firstPush cannot stop the push: it is left alone, gets GATE_AFTER_PUSH, and a new gate is
// placed. Builds of g that are not gated get MULTI_PLATFORM or PUSH_IN_BUILD (image builds that
// cannot be gated), or BUILD_AFTER_PUSH.
//
// The root project has no source to check. Its gate is dropped (gatePlace.drop) when B is empty,
// it has no compose file and there is no existing gate: its empty target list would mean every
// target, so it would scan an image this job never built. A dropped gate returns only the notes
// of its builds, which say that no gate is placed; the notes about where the gate runs
// (GATE_AFTER_PUSH, orderNotes) are left out.
func (m *gateModel) placeIn(p plan.Project, j int, g []located, version string) (*gatePlace, []plan.Note) {
	file := m.jobFile(j)
	inG := func(b located) bool { return slices.ContainsFunc(g, func(x located) bool { return x.i == b.i }) }
	first := g[0].at()
	firstPush := never
	for _, a := range m.pushes(j, inG) {
		if !a.less(first) && a.less(firstPush) {
			firstPush = a
		}
	}
	gateable := func(b located) bool { return b.Image == nil || b.Image.Gateable() }
	var bset []located
	for _, b := range g {
		if gateable(b) && b.at().less(firstPush) {
			bset = append(bset, b)
		}
	}
	gate := plan.Gate{Project: p, Config: configFile(p), Job: m.jobs[j].ID, Version: version}
	var notes []plan.Note
	note := func(b located, code, msg string) {
		notes = append(notes, plan.Note{Code: code, Message: msg, File: b.Pos.File, Line: b.Pos.Line})
	}
	where := func(b located) string {
		if b.Via != "" {
			return fmt.Sprintf("this build (in %s)", b.Via)
		}
		return "this build"
	}
	if len(bset) > 0 {
		gate.At = bset[len(bset)-1].Pos
	} else {
		gate.At, gate.Before = g[0].Pos, true
	}
	e, late := existingGate(m.jobs[j], p.Dir, firstPush)
	root := p.Ecosystem == plan.EcosystemDocker
	drop := root && len(bset) == 0 && len(p.Compose) == 0 && e == nil
	if e == nil && late != nil && !drop {
		notes = append(notes, lateGateNote(p, late))
	}
	if e != nil {
		gate.Existing, gate.At, gate.Before = e, e.Pos, false
		ePos := at{e.Pos.Step, e.Pos.Command}
		kept := bset[:0:0]
		for _, b := range bset {
			if ePos.less(b.at()) {
				note(b, "BUILD_AFTER_GATE", where(b)+" runs after the existing Shearbolt gate in this job, so that gate does not scan it; move the gate after the build, or remove it and run setup again")
				continue
			}
			kept = append(kept, b)
		}
		bset = kept
	}
	// gateBefore reports whether the gate runs before build b.
	gateBefore := func(b located) bool {
		a := at{gate.At.Step, gate.At.Command}
		if gate.Before {
			return !b.at().less(a)
		}
		return a.less(b.at())
	}
	for _, b := range g {
		if slices.ContainsFunc(bset, func(x located) bool { return x.i == b.i }) {
			continue
		}
		order, still := "the gate runs before it and checks the source lockfiles", "; the source lockfiles are still checked"
		switch {
		case drop:
			order = "no gate is placed for it in this job"
		case !gateBefore(b):
			order = "the gate in this job runs after it, so it does not stop this build's push"
		case root && len(p.Compose) > 0:
			order = "the gate runs before it and checks its compose images"
		case root:
			order = "the gate runs before it and checks only the images built before it"
		}
		if root {
			still = ""
			if len(p.Compose) > 0 {
				still = "; its compose images are still checked"
			}
		}
		switch {
		case !gateable(b) && b.Image.OutputPush:
			note(b, "PUSH_IN_BUILD", "this image build pushes through its output, so no gate can run between the build and the push and its image is not scanned; "+order+". Build with --load (or load: true) and push in a later step to scan the image")
		case !gateable(b) && gateBefore(b):
			note(b, "MULTI_PLATFORM", "this image build targets more than one platform, so it cannot be loaded into the local Docker daemon and scanned before it is pushed; "+order)
		case !gateable(b):
			note(b, "MULTI_PLATFORM", "this image build targets more than one platform, so it cannot be loaded into the local Docker daemon, and the gate, which runs after it, does not scan its image"+still)
		case !b.at().less(firstPush) && !slices.ContainsFunc(b.Notes, func(n plan.Note) bool { return n.Code == "BUILD_AFTER_PUSH" }):
			unscanned := "so the gate, which runs before that push, does not scan it"
			if drop {
				unscanned = "so no gate can run between it and that push and its image is not scanned"
			}
			note(b, "BUILD_AFTER_PUSH", where(b)+" runs after a push or deploy in the same job, "+unscanned+"; build before you push, or in a separate job, to scan it")
		}
	}
	if !drop {
		notes = append(notes, m.orderNotes(p, j, g, bset, gate, inG)...)
	}
	gp := &gatePlace{file: file, drop: drop}
	for _, b := range bset {
		gate.Builds = append(gate.Builds, b.Build)
		switch b.Kind {
		case plan.BuildImage:
			gp.images = append(gp.images, b.Build)
		case plan.BuildBinary:
			gp.binaries = append(gp.binaries, b.Build)
		}
	}
	gp.gate = gate
	return gp, notes
}

// orderNotes reports what runs before the gate but is not checked by it: PUSH_BEFORE_GATE for
// pushes ahead of the first build of g and of the gate, and COMMANDS_BEFORE_GATE for commands and
// steps between the gated builds bset (and, for an existing gate, up to that gate). Builds,
// gates, shell state and setup actions are not listed.
func (m *gateModel) orderNotes(p plan.Project, j int, g, bset []located, gate plan.Gate, inG func(located) bool) []plan.Note {
	var notes []plan.Note
	job := m.jobs[j]
	gateAt := at{gate.At.Step, gate.At.Command}
	beforeGate := func(a at) bool { return a.less(gateAt) || !gate.Before && a == gateAt }
	var early []at
	for _, a := range m.pushes(j, inG) {
		if a.less(g[0].at()) && beforeGate(a) && !slices.Contains(early, a) {
			early = append(early, a)
		}
	}
	if len(early) > 0 {
		slices.SortFunc(early, func(a, b at) int {
			if a.less(b) {
				return -1
			}
			return 1
		})
		var items []string
		for _, a := range early {
			items = append(items, m.itemText(j, a))
		}
		notes = append(notes, plan.Note{
			Code: "PUSH_BEFORE_GATE",
			Message: fmt.Sprintf("this job pushes or deploys before the first build of %s, ahead of the gate, so that runs unchecked: %s; move it after the build and the gate",
				projectLabel(p), strings.Join(items, "; ")),
			File: g[0].Pos.File,
			Line: g[0].Pos.Line,
		})
	}

	if len(bset) == 0 || len(bset) == 1 && gate.Existing == nil {
		return notes
	}
	lo, hi := bset[0].at(), bset[len(bset)-1].at()
	if gate.Existing != nil {
		hi = gateAt
	}
	skip := map[at]bool{}
	for _, b := range m.builds {
		if b.job == j && !b.RewriteOnly {
			skip[b.at()] = true
		}
	}
	for s, st := range job.Steps {
		for _, gc := range st.Gates {
			skip[at{s, gc.Pos.Command}] = true
		}
	}
	var items []string
	var firstItem *at
	for s := lo.step; s <= hi.step && s < len(job.Steps); s++ {
		st := job.Steps[s]
		cands := []at{{s, -1}}
		if len(st.Commands) > 0 {
			cands = nil
			for c := range st.Commands {
				cands = append(cands, at{s, c})
			}
		}
		for _, a := range cands {
			if !lo.less(a) || !a.less(hi) || skip[a] || m.harmless(j, a) {
				continue
			}
			if firstItem == nil {
				firstItem = &a
			}
			items = append(items, m.itemText(j, a))
		}
	}
	if len(items) == 0 {
		return notes
	}
	next := bset[len(bset)-1]
	for _, b := range bset {
		if firstItem.less(b.at()) {
			next = b
			break
		}
	}
	return append(notes, plan.Note{
		Code: "COMMANDS_BEFORE_GATE",
		Message: fmt.Sprintf("these commands or steps run between gated builds of %s in this job, before the gate, which follows the last gated build: %s; if one of them deploys or publishes, move it after the gate",
			projectLabel(p), strings.Join(items, "; ")),
		File: next.Pos.File,
		Line: next.Pos.Line,
	})
}

// stateCommands only change the shell's state or print, so they cannot deploy.
var stateCommands = []string{"cd", "pushd", "popd", "export", "set", "unset", "echo", "printf", "true", "false", ":", "shift", "local", "readonly", "declare", "umask",
	"fi", "done", "esac", "}", ")"}

// setupActions prepare a job and cannot deploy; names ending in "-" are prefixes.
var setupActions = []string{"actions/checkout", "actions/cache", "actions/setup-", "docker/setup-", "docker/login-action", "docker/metadata-action",
	"taiki-e/install-action", "dtolnay/rust-toolchain", "Swatinem/rust-cache"}

// harmless reports a command or step that cannot deploy: shell state, setup's own install, or a
// setup action.
func (m *gateModel) harmless(j int, a at) bool {
	st := m.jobs[j].Steps[a.step]
	if a.cmd < 0 || a.cmd >= len(st.Commands) {
		name, _, _ := strings.Cut(st.Uses, "@")
		return st.Uses != "" && slices.ContainsFunc(setupActions, func(p string) bool {
			return name == p || strings.HasSuffix(p, "-") && strings.HasPrefix(name, p)
		})
	}
	cmd := st.Commands[a.cmd]
	argv := cmd.Argv()
	if len(argv) == 0 || slices.Contains(stateCommands, argv[0]) {
		return true
	}
	return slices.ContainsFunc(cmd.Words, func(w shell.Word) bool { return strings.Contains(w.Raw, "cargo-auditable") })
}

// itemText describes the command or step at a for messages.
func (m *gateModel) itemText(j int, a at) string {
	st := m.jobs[j].Steps[a.step]
	if a.cmd < 0 || a.cmd >= len(st.Commands) {
		switch {
		case st.Name != "":
			return st.Name
		case st.Uses != "":
			return st.Uses
		}
		return fmt.Sprintf("step %d", a.step+1)
	}
	var words []string
	for _, w := range st.Commands[a.cmd].Words {
		words = append(words, w.Raw)
	}
	s := strings.Join(words, " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:57]) + "..."
	}
	return s
}

// plan computes the project's planned targets from its gates (R5, R6): one image target per
// slot (as many as the most gated image builds in one job), one file target per binary path of
// its gated binary builds (only for Rust projects, whose paths are relative to the project's
// Cargo.lock root), one compose target per compose file of the project (composeName), and
// source with the project's excludes, except for the root project (docker), which has no
// lockfiles. Slot 1 is named after the project, slot k >= 2 after the last path component of
// its scan tag's repository in the first job that fills it (image-<k> when that is not usable).
func (pp *projectPlan) plan() {
	p := pp.project
	taken := map[string]bool{"source": true}
	pp.source = ""
	pp.files = map[string]string{}
	var images, files []config.Target
	usedVars := map[string]bool{}
	for k := 1; ; k++ {
		gi := slices.IndexFunc(pp.gates, func(g *gatePlace) bool { return len(g.images) >= k })
		if gi < 0 {
			break
		}
		name, v := "", "SHEARBOLT_IMAGE"
		if k == 1 {
			name = uniqueName(shortName(p), taken)
		} else {
			base := fmt.Sprintf("image-%d", k)
			if repo, ok := buildRepository(*pp.gates[gi].images[k-1].Image); ok {
				if c := strings.Trim(sanitizeName(path.Base(repo)), ".-_"); c != "" && !strings.ContainsAny(path.Base(repo), "$`") {
					base = c
				}
			}
			name = uniqueName(base, taken)
			v = "SHEARBOLT_IMAGE_" + envName(name)
		}
		for n, base := 2, v; usedVars[v]; n++ {
			v = fmt.Sprintf("%s_%d", base, n)
		}
		usedVars[v] = true
		pp.slots = append(pp.slots, name)
		images = append(images, config.Target{Name: name, Image: fmt.Sprintf("${%s:-%s:local}", v, dockerRepoName(name))})
	}
	if p.Ecosystem == "rust" {
		byBin := map[string]bool{}
		for _, g := range pp.gates {
			for _, b := range g.binaries {
				for _, f := range b.Paths {
					if _, ok := pp.files[f]; ok {
						continue
					}
					bin := strings.Trim(sanitizeName(path.Base(f)), ".-_")
					if bin == "" {
						bin = "bin"
					}
					name := bin
					if byBin[bin] || taken[bin] {
						parts := strings.Split(f, "/")
						if t := len(parts) - 3; t >= 1 && strings.Contains(parts[t], "-") && !taken[bin+"-"+sanitizeName(parts[t])] {
							name = bin + "-" + sanitizeName(parts[t])
						}
					}
					byBin[bin] = true
					name = uniqueName(name, taken)
					pp.files[f] = name
					files = append(files, config.Target{Name: name, File: f, LockfileFallback: "Cargo.lock"})
				}
			}
		}
	}
	pp.targets = append(images, files...)
	for _, f := range p.Compose {
		pp.targets = append(pp.targets, config.Target{Name: uniqueName(composeName(f), taken), Compose: f})
	}
	if p.Ecosystem != plan.EcosystemDocker {
		pp.source = "source"
		pp.targets = append(pp.targets, config.Target{Name: pp.source, Source: ".", Exclude: slices.Clone(p.Excludes)})
	}
}

// composeName is the target name of compose file f, a path relative to the project: f without
// its extension, "/" replaced by "-", sanitized, with leading and trailing separators trimmed;
// "compose" when nothing is left. So docker-compose.yml is docker-compose, test/compose.yml is
// test-compose.
func composeName(f string) string {
	n := strings.ReplaceAll(strings.TrimSuffix(f, path.Ext(f)), "/", "-")
	if n = strings.Trim(sanitizeName(n), ".-_"); n != "" {
		return n
	}
	return "compose"
}

// finish completes the project's gates against its merged config (R4 rule 8, R5). names maps
// planned names to the names in the file and vars gives each image target's variable (from
// configChange); merged is nil when the config could not be edited, and the gates then scan
// every target it has. commit returns the CI's commit expression for a CI file (commitExprs).
// used holds the scan tags already given out per job; it is shared by every project's finish, so
// scan tags are unique within a job.
//
// A gate scans the targets of its gated builds, source, every compose target, and every target
// setup did not plan; Targets is nil when that is every target of the merged config, and
// otherwise lists only targets the merged config has (a planned target can be missing from it).
// Each gated image build whose target has a variable gets an ImageVar with the scan tag the
// plugin adds to the build.
func (pp *projectPlan) finish(merged []config.Target, names, vars map[string]string, commit func(file string) string, used map[string]map[string]bool) []plan.Gate {
	name := func(planned string) string {
		if n, ok := names[planned]; ok {
			return n
		}
		return planned
	}
	plannedNames := map[string]bool{}
	always := map[string]bool{} // scanned by every gate: source and the compose targets
	if pp.source != "" {
		always[name(pp.source)] = true
	}
	for _, t := range pp.targets {
		plannedNames[name(t.Name)] = true
		if t.Kind() == "compose" {
			always[name(t.Name)] = true
		}
	}
	inMerged := map[string]bool{}
	for _, t := range merged {
		inMerged[t.Name] = true
	}
	var out []plan.Gate
	for _, gp := range pp.gates {
		g := gp.gate
		set := maps.Clone(always)
		if used[g.Job] == nil {
			used[g.Job] = map[string]bool{}
		}
		usedTags := used[g.Job]
		for k, b := range gp.images {
			n := name(pp.slots[k])
			set[n] = true
			if v := vars[n]; v != "" {
				tag := scanTag(*b.Image, commit(gp.file), dockerRepoName(pp.slots[k]))
				// The same repository as an earlier image of the job: a shared tag would scan one
				// image twice, so this one gets a number.
				for i := len(usedTags) + 1; usedTags[tagValue(*b.Image, tag)]; i++ {
					next := scanTag(*b.Image, fmt.Sprintf("%d-%s", i, commit(gp.file)), dockerRepoName(pp.slots[k]))
					if next == tag {
						break // a tag already in the build is kept; the CI plugin refuses the duplicate
					}
					tag = next
				}
				usedTags[tagValue(*b.Image, tag)] = true
				g.Images = append(g.Images, plan.ImageVar{Var: v, Build: b, ScanTag: tag})
			}
		}
		for _, b := range gp.binaries {
			for _, f := range b.Paths {
				if n, ok := pp.files[f]; ok {
					set[name(n)] = true
				}
			}
		}
		all := merged != nil
		for _, t := range merged {
			if !plannedNames[t.Name] {
				set[t.Name] = true
			}
		}
		for _, t := range merged {
			all = all && set[t.Name]
		}
		if !all && merged != nil {
			for n := range set {
				if inMerged[n] {
					g.Targets = append(g.Targets, n)
				}
			}
			slices.Sort(g.Targets)
		}
		out = append(out, g)
	}
	return out
}
