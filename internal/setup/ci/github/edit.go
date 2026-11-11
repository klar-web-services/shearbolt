package github

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/shelledit"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

// This file applies gates and preps to a workflow (R4 rule 5, R5, R8). Each job is edited on its
// own: its edits are queued, the result is checked, and a job whose edit fails is rolled back
// completely, reported in Edited.Failed and given UNSUPPORTED_EDIT with the gate snippet.
//
// The step is the unit on GitHub. Setup's added lines (docker tag, echo) go at the end of a
// build's pipeline. A step is split right after its last gated build when another command follows
// that build in the gate's step (P1), or when a gated build uses --push; the remainder becomes a
// new step after the gate, with the shell state it needs copied in front.

// failAt returns why a job cannot be edited, located at a file line.
func failAt(line int, format string, args ...any) error {
	return shelledit.Errorf(line, format, args...)
}

// Edit implements plan.CI.
func (p Plugin) Edit(r plan.Repo, file string, src []byte, jobs []plan.Job, gates []plan.Gate, preps map[string][]plan.Prep) (plan.Edited, error) {
	wf, err := parseWorkflow(file, src)
	if err != nil {
		return plan.Edited{}, err
	}
	d := wf.doc
	out := plan.Edited{Src: src}
	var tags []string
	attest := false
	for _, pj := range jobs {
		var jg []plan.Gate
		for _, g := range gates {
			if g.Job == pj.ID {
				jg = append(jg, g)
			}
		}
		jp := preps[pj.ID]
		if len(jg) == 0 && len(jp) == 0 {
			continue
		}
		j := wf.job(pj.ID)
		e := &jobEditor{d: d, wf: wf, j: j, pj: pj, gates: jg, preps: jp, unit: yamledit.IndentUnit(d)}
		cp := d.Checkpoint()
		err := fmt.Errorf("the job is not in the workflow")
		if j != nil && len(j.steps) == len(pj.Steps) {
			err = e.apply()
		}
		if err == nil {
			if _, rerr := d.Result(); rerr != nil {
				err = failAt(0, "the edited file would not be valid YAML (%v)", rerr)
			}
		}
		if err != nil {
			d.Rollback(cp)
			out.Failed = append(out.Failed, pj.ID)
			out.Notes = append(out.Notes, p.failNote(r, file, pj, j, jg, err))
			continue
		}
		out.Notes = append(out.Notes, e.notes...)
		tags = append(tags, e.tags...)
		attest = attest || e.attest
	}
	res, err := d.Result()
	if err != nil {
		return plan.Edited{}, err
	}
	if !bytes.Equal(res, src) {
		out.Src = res
	}
	if len(tags) > 0 {
		slices.Sort(tags)
		out.Notes = append(out.Notes, plan.Note{
			Code: "TAG_ADDED",
			Message: fmt.Sprintf("setup adds the local tag %s to the gated image builds and the gate scans it; it is never pushed, so a scan that falls back to the registry fails instead of reading an older image",
				strings.Join(slices.Compact(tags), ", ")),
			File: file,
		})
	}
	if attest {
		out.Notes = append(out.Notes, plan.Note{
			Code:    "ATTESTATIONS_DROPPED",
			Message: "the --push image builds now load the image instead, and docker push sends their original tags after the gate; pushing with docker push drops the provenance and SBOM attestations BuildKit attaches when it pushes directly",
			File:    file,
		})
	}
	return out, nil
}

func (wf *workflow) job(id string) *job {
	for _, j := range wf.jobs {
		if j.id == id {
			return j
		}
	}
	return nil
}

// failNote is UNSUPPORTED_EDIT for a job that was left unchanged, with the snippet of each of its gates.
func (p Plugin) failNote(r plan.Repo, file string, pj plan.Job, j *job, gates []plan.Gate, err error) plan.Note {
	key, line := pj.ID, 0
	if j != nil {
		key, line = j.key, j.line
	}
	if ee, ok := err.(*shelledit.Error); ok && ee.Line > 0 {
		line = ee.Line
	}
	msg := fmt.Sprintf("%s: job %s was left unchanged: %s.", file, key, err.Error())
	if len(gates) > 0 {
		msg += " Add the gate by hand after the build and before any push or deploy (and tag each image with its SHEARBOLT_IMAGE value):"
		for _, g := range gates {
			msg += "\n" + p.Snippet(r, file, g)
		}
	} else {
		msg += " Its cargo builds were not rewritten to cargo auditable build, so their binaries carry no crate list; the source target still checks Cargo.lock."
	}
	return plan.Note{Code: "UNSUPPORTED_EDIT", Message: msg, File: file, Line: line}
}

// jobEditor queues one job's edits.
type jobEditor struct {
	d     *yamledit.Doc
	wf    *workflow
	j     *job
	pj    plan.Job
	gates []plan.Gate
	preps []plan.Prep
	unit  int

	steps   map[int]*stepPlan
	ids     map[string]bool   // step ids used in the job
	outputs map[[2]int]string // (step, command) of a run-step image build -> its output name
	notes   []plan.Note
	tags    []string
	attest  bool
}

// stepPlan is what happens to one step.
type stepPlan struct {
	s          int
	st         *step
	gate       int // index of the gate whose builds are in this step; -1 for none
	multi      bool
	builds     []plan.Build // gated builds of the step (CI-file and Via), by command
	owners     []int        // per build: the index of the gate it belongs to
	edits      []shelledit.Edit
	shellEdits bool // edits that need a POSIX shell (all but the Rust rewrite)
	needsID    bool
	id         string
	hasPush    bool
	pushes     []string // docker push lines for its --push builds
	split      bool
	cut, rest  int             // part 1 is script[:cut], the remainder script[rest:]
	last       int             // the command part 1 ends with: the last gated build, or setup's lines after it
	marker     bool            // the step can stop before the split point: part 1 ends with setup's marker line (design §3.2)
	state      []string        // shell state copied into the remainder
	dropped    []droppedAssign // assignments left out of the remainder: the split fails when it reads them
	remainder  string
	pushStep   []string // a converted build-push-action's push step, after the gate
}

func (e *jobEditor) stepPlanFor(s int) *stepPlan {
	if sp, ok := e.steps[s]; ok {
		return sp
	}
	sp := &stepPlan{s: s, st: e.j.steps[s], gate: -1}
	e.steps[s] = sp
	return sp
}

func (e *jobEditor) sortedPlans() []*stepPlan {
	var out []*stepPlan
	for _, sp := range e.steps {
		out = append(out, sp)
	}
	slices.SortFunc(out, func(a, b *stepPlan) int { return a.s - b.s })
	return out
}

func samePos(a, b plan.Pos) bool {
	return a.File == b.File && a.Job == b.Job && a.Step == b.Step && a.Command == b.Command
}

func buildAt(builds []plan.Build, pos plan.Pos) (plan.Build, bool) {
	for _, b := range builds {
		if samePos(b.Pos, pos) {
			return b, true
		}
	}
	return plan.Build{}, false
}

func imageVarOf(g plan.Gate, pos plan.Pos) *plan.ImageVar {
	for i := range g.Images {
		if samePos(g.Images[i].Build.Pos, pos) {
			return &g.Images[i]
		}
	}
	return nil
}

func (e *jobEditor) apply() error {
	e.steps = map[int]*stepPlan{}
	e.ids = map[string]bool{}
	for _, st := range e.j.steps {
		if st.id != "" {
			e.ids[st.id] = true
		}
	}
	n := len(e.j.steps)
	inRange := func(p plan.Pos) bool { return p.Step >= 0 && p.Step < n && p.Command < len(e.j.steps[p.Step].cmds) }

	// 1. The gated builds of each step.
	for gi, g := range e.gates {
		if g.Existing != nil && !inRange(g.Existing.Pos) || g.Existing == nil && !inRange(g.At) {
			return failAt(0, "the gate position is not in the job")
		}
		for _, b := range g.Builds {
			if !inRange(b.Pos) {
				return failAt(0, "a gated build is not in the job")
			}
			sp := e.stepPlanFor(b.Pos.Step)
			if sp.gate >= 0 && sp.gate != gi {
				sp.multi = true
			}
			sp.gate = gi
			sp.builds = append(sp.builds, b)
			sp.owners = append(sp.owners, gi)
		}
		for _, iv := range g.Images {
			if _, ok := buildAt(g.Builds, iv.Build.Pos); !ok {
				return failAt(0, "an image to scan is not among the gate's builds")
			}
		}
	}
	// Each scanned image needs its own local tag, or one image would be scanned twice.
	tagAt := map[string]plan.Pos{}
	for _, g := range e.gates {
		for _, iv := range g.Images {
			v := shelledit.ShellValue(iv.ScanTag)
			if p, ok := tagAt[v]; ok && !samePos(p, iv.Build.Pos) {
				return failAt(iv.Build.Pos.Line, "two image builds in this job share the scan tag %s, so the gate cannot tell their images apart; give each build its own shearbolt- tag or remove them and run setup again", v)
			}
			tagAt[v] = iv.Build.Pos
		}
	}
	// docker push --all-tags of a scanned repository would push its scan tag.
	var scanTags []string
	for _, g := range e.gates {
		for _, iv := range g.Images {
			scanTags = append(scanTags, iv.ScanTag)
		}
	}
	for _, st := range e.j.steps {
		if err := shelledit.AllTagsPushes(st.cmds, scanTags, st.cmdLine); err != nil {
			return err
		}
	}
	for _, sp := range e.steps {
		idx := make([]int, len(sp.builds))
		for i := range idx {
			idx[i] = i
		}
		slices.SortStableFunc(idx, func(a, b int) int { return sp.builds[a].Pos.Command - sp.builds[b].Pos.Command })
		builds, owners := make([]plan.Build, len(idx)), make([]int, len(idx))
		for k, i := range idx {
			builds[k], owners[k] = sp.builds[i], sp.owners[i]
		}
		sp.builds, sp.owners = builds, owners
	}

	// 2. Output names: the n-th scanned image build of a step writes shearbolt-image-<n>.
	e.outputs = map[[2]int]string{}
	perStep := map[int][]int{}
	for _, g := range e.gates {
		for _, iv := range g.Images {
			if p := iv.Build.Pos; p.Command >= 0 && iv.Build.Via == "" {
				perStep[p.Step] = append(perStep[p.Step], p.Command)
			}
		}
	}
	for s, cmds := range perStep {
		slices.Sort(cmds)
		for k, c := range slices.Compact(cmds) {
			name := "shearbolt-image"
			if k > 0 {
				name = fmt.Sprintf("shearbolt-image-%d", k+1)
			}
			e.outputs[[2]int{s, c}] = name
		}
	}

	// Setup's output lines already in a step must each follow the build they belong to.
	for _, sp := range e.sortedPlans() {
		if err := e.checkOutputLines(sp); err != nil {
			return err
		}
	}

	// 3. Image builds: scan tag, --load, output lines, --push.
	for _, sp := range e.sortedPlans() {
		for i, b := range sp.builds {
			if b.Pos.Command < 0 {
				if err := e.actionEdits(sp, e.gates[sp.owners[i]], b); err != nil {
					return err
				}
				continue
			}
			if b.Via == "" && b.Kind == plan.BuildImage && b.Image != nil {
				if err := e.imageEdits(sp, e.gates[sp.owners[i]], b); err != nil {
					return err
				}
			}
		}
	}

	// 4. Rust rewrites in the workflow's own scripts.
	for _, pr := range e.preps {
		for _, in := range pr.Inserts {
			if in.Via != "" {
				continue
			}
			if !inRange(in.Pos) || e.j.steps[in.Pos.Step].run == nil || in.Offset < 0 || in.Offset > len(e.j.steps[in.Pos.Step].script) {
				return failAt(0, "a cargo build to rewrite is not in the job")
			}
			sp := e.stepPlanFor(in.Pos.Step)
			sp.edits = append(sp.edits, shelledit.Edit{At: in.Offset, End: in.Offset, Text: in.Text})
		}
	}

	// 5. Splits.
	for _, sp := range e.sortedPlans() {
		if err := e.decideSplit(sp); err != nil {
			return err
		}
	}

	// 6. Changed steps: ids and scripts.
	for _, sp := range e.sortedPlans() {
		if err := e.renderStep(sp); err != nil {
			return err
		}
	}

	// 7. The install, before the first build that needs it.
	if err := e.install(); err != nil {
		return err
	}

	// 8. Gates, then the remainders that follow them.
	for gi, g := range e.gates {
		if err := e.placeGate(gi, g); err != nil {
			return err
		}
	}

	// 9. Push steps after a gate must not run when it fails.
	return e.wrapPushIfs()
}

// imageEdits plans the changes to gated image build b in step sp (R2, R5): the scan tag (or a
// docker tag line for railpack), the output line that passes it to the gate, --load for buildx,
// and for --push the conversion to --load with docker push lines for the original tags.
func (e *jobEditor) imageEdits(sp *stepPlan, g plan.Gate, b plan.Build) error {
	st := sp.st
	c := b.Pos.Command
	cmd := st.cmds[c]
	line := st.cmdLine(cmd)
	if cmd.Start != b.Cmd.Start {
		return failAt(line, "the build command could not be located in its step")
	}
	img := b.Image
	before := len(sp.edits)
	var added []string
	if iv := imageVarOf(g, b.Pos); iv != nil {
		switch img.Tool {
		case "docker", "buildx":
			ed, needed, err := shelledit.TagEdit(img, iv.ScanTag, len(st.script), line)
			if err != nil {
				return err
			}
			if needed {
				sp.edits = append(sp.edits, ed)
				e.tags = append(e.tags, shelledit.ShellValue(iv.ScanTag))
			}
		case "railpack":
			if len(img.Tags) == 0 {
				return failAt(line, "this railpack build has no --name to tag")
			}
			if !shelledit.RailpackTagged(st.cmds, c, img) {
				added = append(added, "docker tag "+img.Tags[0]+" "+iv.ScanTag)
				e.tags = append(e.tags, shelledit.ShellValue(iv.ScanTag))
			}
		default:
			return failAt(line, "setup cannot add a scan tag to a %s build", img.Tool)
		}
		if line := e.outputLine(sp.s, c); !slices.Contains(ownLinesAfter(st, c), line) {
			added = append(added, line)
		}
		sp.needsID = true
	}
	if img.Push && (img.Tool == "docker" || img.Tool == "buildx") {
		edits, pushes, err := shelledit.PushEdits(cmd, img, line)
		if err != nil {
			return err
		}
		if err := e.pushBuildRefusals(st, cmd.ArgWords(), line); err != nil {
			return err
		}
		sp.edits = append(sp.edits, edits...)
		sp.pushes = append(sp.pushes, pushes...)
		sp.hasPush = true
		e.attest = true
	} else if ed, ok := shelledit.LoadEdit(img); ok {
		if err := shelledit.AttestationFlags(cmd.ArgWords(), line); err != nil {
			return err
		}
		sp.edits = append(sp.edits, ed)
	}
	if len(added) > 0 {
		if err := shelledit.Insertable(cmd, line, "add lines after it"); err != nil {
			return err
		}
		at, text := shelledit.InsertPoint(st.script, cmd, added)
		sp.edits = append(sp.edits, shelledit.Edit{At: at, End: at, Text: text})
	}
	if len(sp.edits) > before || sp.needsID {
		sp.shellEdits = true
		if !st.posix {
			return failAt(line, "this step does not run in bash or sh (shell: or a Windows runner), so setup cannot edit its script")
		}
	}
	return nil
}

// imageVarAt returns the image variable of the build at pos in any of the job's gates.
func (e *jobEditor) imageVarAt(pos plan.Pos) *plan.ImageVar {
	for _, g := range e.gates {
		if iv := imageVarOf(g, pos); iv != nil {
			return iv
		}
	}
	return nil
}

// outputLine is the line setup adds after scanned image build c of step s, exactly as written.
func (e *jobEditor) outputLine(s, c int) string {
	iv := e.imageVarAt(plan.Pos{File: e.wf.file, Job: e.j.id, Step: s, Command: c})
	return "echo " + e.outputs[[2]int{s, c}] + "=" + iv.ScanTag + ` >> "$GITHUB_OUTPUT"`
}

// ownLinesAfter returns the text of setup's own lines directly after command c.
func ownLinesAfter(st *step, c int) []string {
	var out []string
	for k := c + 1; k < len(st.cmds) && shelledit.OwnLine(st.cmds[k]); k++ {
		out = append(out, st.script[st.cmds[k].Start:st.cmds[k].End])
	}
	return out
}

// checkOutputLines fails the job when a step that holds gated builds has a shearbolt-image
// output line that is not setup's line for the build it directly follows (R8): after builds are
// added or moved, a line matched by its name alone would hand the gate another build's image.
func (e *jobEditor) checkOutputLines(sp *stepPlan) error {
	st := sp.st
	if st.run == nil || !strings.Contains(st.script, "shearbolt-image") {
		return nil
	}
	builds := map[int]bool{}
	for _, b := range sp.builds {
		builds[b.Pos.Command] = true
	}
	for k, cmd := range st.cmds {
		text := st.script[cmd.Start:cmd.End]
		if builds[k] || !strings.Contains(text, "shearbolt-image") {
			continue
		}
		j := k - 1
		for j >= 0 && shelledit.OwnLine(st.cmds[j]) {
			j--
		}
		b := plan.Pos{File: e.wf.file, Job: e.j.id, Step: sp.s, Command: j}
		if j < 0 || !builds[j] || e.imageVarAt(b) == nil || e.outputs[[2]int{sp.s, j}] == "" || e.outputLine(sp.s, j) != text {
			return failAt(st.cmdLine(cmd), "%s is not setup's output line for the build before it; remove setup's output lines from this step and run setup again", shelledit.CommandText(cmd))
		}
	}
	return nil
}

// editable reports whether a step's own text can be changed: not an alias, no anchors or aliases
// inside it (they would change other steps too), and not flow style.
func editable(st *step) bool {
	return st.m != nil && st.item.Kind != yaml.AliasNode && !hasAnchor(st.item) && !hasAlias(st.m) && !flow(st.m)
}

func hasAlias(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.AliasNode {
		return true
	}
	return slices.ContainsFunc(n.Content, hasAlias)
}

// renderStep queues the step's id and its new script (part 1 when it is split).
func (e *jobEditor) renderStep(sp *stepPlan) error {
	st := sp.st
	if st.run == nil {
		return nil
	}
	line := st.line
	newScript := st.script
	if sp.split {
		e.markerEdit(sp)
		var first, second []shelledit.Edit
		for _, ed := range sp.edits {
			switch {
			case ed.At <= sp.cut && ed.End <= sp.cut:
				first = append(first, ed)
			case ed.At >= sp.rest:
				second = append(second, ed)
			default:
				return failAt(line, "an edit falls on the split point")
			}
		}
		part1, err := shelledit.Apply(st.script[:sp.cut], 0, first)
		if err != nil {
			return failAt(line, "%v", err)
		}
		rest, err := shelledit.Apply(st.script[sp.rest:], sp.rest, second)
		if err != nil {
			return failAt(line, "%v", err)
		}
		if strings.Contains(part1, "\n") && !strings.HasSuffix(part1, "\n") {
			part1 += "\n"
		}
		rem := strings.Join(append(slices.Clone(sp.state), sp.pushes...), "\n")
		if rest == "" && len(sp.pushes) == 0 {
			rem = "" // nothing left to run after the gate
		}
		if rest != "" {
			if rem != "" {
				rem += "\n"
			}
			rem += rest
		}
		if strings.Contains(rem, "\n") && !strings.HasSuffix(rem, "\n") {
			rem += "\n"
		}
		for _, d := range sp.dropped {
			if assignRead(d.names, rem, st.script, st.cmds[:d.cmd], slices.Concat(e.wf.env, e.j.env, st.env)) {
				return failAt(d.line, d.reason, d.text)
			}
		}
		if st.id != "" && strings.Contains(rem, "GITHUB_OUTPUT") {
			return failAt(line, "the step has id %s and the part after the gated build writes $GITHUB_OUTPUT, which a split step would not pass on", st.id)
		}
		sp.remainder = rem
		newScript = part1
	} else {
		var err error
		if newScript, err = shelledit.Apply(st.script, 0, sp.edits); err != nil {
			return failAt(line, "%v", err)
		}
	}
	if sp.needsID {
		sp.id = st.id
	}
	newID := sp.needsID && st.id == ""
	if newScript == st.script && !newID {
		return nil
	}
	if !editable(st) || st.run.Kind != yaml.ScalarNode || st.run.Tag == "!!binary" {
		return failAt(line, "this step uses YAML anchors, aliases or flow style, so setup cannot change it in place")
	}
	var sFirst, sLast int
	var sLines []string
	if newScript != st.script {
		var err error
		if sFirst, sLast, sLines, err = e.d.ScriptLines(st.run, newScript); err != nil {
			return failAt(line, "its script cannot be rewritten (%v)", err)
		}
	}
	if newID {
		sp.id = e.newID()
		if err := e.insertID(st, sp.id, sFirst, &sLines); err != nil {
			return failAt(line, "setup cannot add an id to this step (%v)", err)
		}
	}
	if sLines != nil {
		if err := e.d.ReplaceLines(sFirst, sLast, sLines); err != nil {
			return failAt(line, "its script cannot be rewritten (%v)", err)
		}
	}
	return nil
}

// markerEdit queues setup's marker line as the last command of a split step's part 1, after the
// output line and joined like it: with the command's own && or ;, so a failed build in an &&
// list still fails the step, or on a new line. A marker line already there is kept.
func (e *jobEditor) markerEdit(sp *stepPlan) {
	st := sp.st
	if !sp.marker || slices.Contains(ownLinesAfter(st, sp.builds[len(sp.builds)-1].Pos.Command), shelledit.ReachedLine) {
		return
	}
	at, text := shelledit.InsertPoint(st.script, st.cmds[sp.last], []string{shelledit.ReachedLine})
	// A line added before it at the end of the script already ends the script with a newline.
	if at == len(st.script) && slices.ContainsFunc(sp.edits, func(ed shelledit.Edit) bool { return ed.At == at && strings.HasSuffix(ed.Text, "\n") }) {
		text = strings.TrimPrefix(text, "\n")
	}
	sp.edits = append(sp.edits, shelledit.Edit{At: at, End: at, Text: text})
}

// holdsMarker reports whether a run step's script has setup's marker line, matched by its exact
// text as markerEdit matches it: OwnLine alone also accepts the image output line.
func holdsMarker(st *step) bool {
	return slices.ContainsFunc(st.cmds, func(c shell.Command) bool {
		return c.End <= len(st.script) && st.script[c.Start:c.End] == shelledit.ReachedLine
	})
}

func (e *jobEditor) newID() string {
	id := "shearbolt-build"
	for n := 2; e.ids[id]; n++ {
		id = fmt.Sprintf("shearbolt-build-%d", n)
	}
	e.ids[id] = true
	return id
}

// insertID queues `id: <id>` after the step's name, or as its first key. When the first key
// shares the dash line with the script being replaced (scriptFirst), the id is folded into
// those lines instead.
func (e *jobEditor) insertID(st *step, id string, scriptFirst int, scriptLines *[]string) error {
	m := st.m
	if slices.Contains(keys(m), "name") {
		return e.d.SetKey(m, "id", yamledit.Scalar(id), "name")
	}
	k0 := m.Content[0]
	src := e.d.Line(k0.Line)
	off := byteOffset(src, k0.Column-1)
	prefix, kc := src[:off], k0.Column-1
	pad := strings.Repeat(" ", kc)
	if !strings.Contains(prefix, "-") {
		return e.d.InsertBefore(k0.Line, []string{pad + "id: " + yamledit.Scalar(id)})
	}
	idLine := prefix + "id: " + yamledit.Scalar(id)
	if *scriptLines != nil && scriptFirst == k0.Line {
		first := (*scriptLines)[0]
		if len(first) < off || first[:off] != prefix {
			return fmt.Errorf("the script's first line moved")
		}
		*scriptLines = append([]string{idLine, pad + first[off:]}, (*scriptLines)[1:]...)
		return nil
	}
	return e.d.ReplaceLines(k0.Line, k0.Line, []string{idLine, pad + src[off:]})
}

// byteOffset returns the byte offset of rune column col in s.
func byteOffset(s string, col int) int {
	for i := range s {
		if col == 0 {
			return i
		}
		col--
	}
	return len(s)
}

// install queues the first Install of the job's preps as a step before the first build that
// needs it, unless an earlier step, or the script before that build, already mentions its Marker.
func (e *jobEditor) install() error {
	var first *plan.Prep
	for i := range e.preps {
		pr := &e.preps[i]
		if pr.Install == nil {
			continue
		}
		if first == nil || pr.Pos.Step < first.Pos.Step || pr.Pos.Step == first.Pos.Step && pr.Pos.Command < first.Pos.Command {
			first = pr
		}
	}
	if first == nil {
		return nil
	}
	s := first.Pos.Step
	if s < 0 || s >= len(e.j.steps) {
		return failAt(0, "the build that needs %s is not in the job", first.Install.Name)
	}
	in := first.Install
	if in.Marker != "" {
		for i := 0; i < s; i++ {
			if stepMentions(e.j.steps[i], len(e.j.steps[i].cmds), in.Marker) {
				return nil
			}
		}
		if st := e.j.steps[s]; first.Pos.Command >= 0 && first.Pos.Command < len(st.cmds) && commandsMention(st.cmds[:first.Pos.Command], in.Marker) {
			return nil
		}
	}
	pad := strings.Repeat(" ", e.unit)
	lines := []string{"name: " + yamledit.Scalar(in.Name)}
	if in.Uses != "" {
		lines = append(lines, "uses: "+yamledit.Scalar(in.Uses))
		if len(in.With) > 0 {
			lines = append(lines, "with:")
			for _, k := range sortedKeys(in.With) {
				lines = append(lines, pad+k+": "+yamledit.Scalar(in.With[k]))
			}
		}
	} else {
		lines = append(lines, "run: "+yamledit.Scalar(in.Run))
	}
	if err := e.d.InsertItem(e.j.seq, s-1, lines); err != nil {
		return failAt(e.j.steps[s].line, "setup cannot add the %s step (%v)", in.Name, err)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// stepMentions reports whether a step's values (other than its name), or the words of its first
// n commands, contain marker. Comments do not count.
func stepMentions(st *step, n int, marker string) bool {
	if st.m == nil {
		return false
	}
	var walk func(x *yaml.Node) bool
	walk = func(x *yaml.Node) bool {
		x = resolveAlias(x)
		if x == nil {
			return false
		}
		if x.Kind == yaml.ScalarNode {
			return strings.Contains(x.Value, marker)
		}
		return slices.ContainsFunc(x.Content, walk)
	}
	for i := 0; i+1 < len(st.m.Content); i += 2 {
		if k := st.m.Content[i].Value; k != "name" && k != "run" && walk(st.m.Content[i+1]) {
			return true
		}
	}
	return commandsMention(st.cmds[:n], marker)
}

// commandsMention reports whether a word of cmds contains marker.
func commandsMention(cmds []shell.Command, marker string) bool {
	return slices.ContainsFunc(cmds, func(c shell.Command) bool {
		return slices.ContainsFunc(c.Words, func(w shell.Word) bool { return strings.Contains(w.Value, marker) })
	})
}
