package github

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/shelledit"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

func (e *jobEditor) gateEnv(g plan.Gate) ([][2]string, error) {
	var env [][2]string
	for _, iv := range g.Images {
		b, _ := buildAt(g.Builds, iv.Build.Pos)
		if b.Pos.Command < 0 {
			env = append(env, [2]string{iv.Var, iv.ScanTag})
			continue
		}
		sp := e.steps[b.Pos.Step]
		name := e.outputs[[2]int{b.Pos.Step, b.Pos.Command}]
		if sp == nil || sp.id == "" || name == "" {
			return nil, failAt(0, "the step that builds the image has no id to read its tag from")
		}
		env = append(env, [2]string{iv.Var, fmt.Sprintf("${{ steps.%s.outputs.%s }}", sp.id, name)})
	}
	return env, nil
}

func (e *jobEditor) placeGate(gi int, g plan.Gate) error {
	env, err := e.gateEnv(g)
	if err != nil {
		return err
	}
	anchor := g.At.Step
	decided := -1 // the split step whose early stops decide the gate's marker term
	switch {
	case g.Existing != nil:
		anchor = g.Existing.Pos.Step
		if g.Existing.Pos.Command < 0 {
			decided = g.Existing.Pos.Step - 1
		}
		if err := e.updateExisting(gi, g, env); err != nil {
			return err
		}
	case g.Before:
		if err := e.d.InsertItem(e.j.seq, g.At.Step-1, gateLines(g, "", env, e.unit)); err != nil {
			return failAt(e.j.steps[g.At.Step].line, "setup cannot add the gate step (%v)", err)
		}
		e.zosia = e.zosia || g.Zosia
	default:
		st := e.j.steps[g.At.Step]
		ifCond, err := e.gateIf(g)
		if err != nil {
			return err
		}
		if sp := e.markerSplit(gi, g.At.Step); sp != nil {
			decided = sp.s
			skip, reason := e.gateMaySkip(g, sp.s)
			if skip && templated(ifCond) {
				skip, reason = false, reasonTemplate
			}
			if skip {
				ifCond = withMarker(ifCond, sp.id)
			}
			e.notes = append(e.notes, e.earlyStopNote(g, sp.s, skip, reason))
		}
		if err := e.d.InsertItem(e.j.seq, g.At.Step, gateLines(g, ifCond, env, e.unit)); err != nil {
			return failAt(st.line, "setup cannot add the gate step (%v)", err)
		}
		e.zosia = e.zosia || g.Zosia
	}
	for _, sp := range e.sortedPlans() {
		if sp.gate != gi {
			continue
		}
		// Every new split with the marker gets EARLY_STOP. A split the gate does not directly
		// follow cannot carry the gate's term, and neither can a gate in a run step.
		if sp.split && sp.marker && sp.s != decided {
			reason := reasonNotNext
			if g.Existing != nil && g.Existing.Pos.Command >= 0 {
				reason = reasonRunStep
			}
			if skip, why := e.gateMaySkip(g, sp.s); !skip {
				reason = why
			}
			e.notes = append(e.notes, e.earlyStopNote(g, sp.s, false, reason))
		}
		if sp.pushStep != nil {
			if err := e.d.InsertItem(e.j.seq, anchor, sp.pushStep); err != nil {
				return failAt(sp.st.line, "setup cannot add the push step (%v)", err)
			}
		}
		if !sp.split || sp.remainder == "" {
			continue
		}
		lines, err := e.remainderLines(sp)
		if err != nil {
			return err
		}
		if err := e.d.InsertItem(e.j.seq, anchor, lines); err != nil {
			return failAt(sp.st.line, "setup cannot add the continued step (%v)", err)
		}
	}
	return nil
}

// Builds under different conditions fail the job: a gate that follows one of them would be
// skipped while another build is pushed unscanned, or run where an image was never built.
func (e *jobEditor) gateIf(g plan.Gate) (string, error) {
	cond, first := "", true
	for _, b := range g.Builds {
		c := ""
		if n := e.j.steps[b.Pos.Step].ifNode; n != nil {
			c = n.Value
		}
		if first {
			cond, first = c, false
			continue
		}
		if bareExpr(c) != bareExpr(cond) {
			return "", failAt(e.j.steps[b.Pos.Step].line, "the gated builds run under different if: conditions, so one gate cannot follow them all")
		}
	}
	if st := e.j.steps[g.At.Step]; st.ifNode != nil && !first {
		return st.ifNode.Value, nil
	}
	return cond, nil
}

func (e *jobEditor) markerSplit(gi, s int) *stepPlan {
	if sp := e.steps[s]; sp != nil && sp.gate == gi && sp.split && sp.marker {
		return sp
	}
	return nil
}

// A running gate fails on early stops only when it scans an image or a file built in step s;
// otherwise it passes, and the note says what it checks, without advice.
func (e *jobEditor) earlyStopNote(g plan.Gate, s int, skip bool, reason string) plan.Note {
	st := e.j.steps[s]
	where, line := "", st.line
	for _, k := range e.pj.Steps[s].Stops {
		if k >= 0 && k < len(st.cmds) {
			line = st.cmdLine(st.cmds[k])
			name := shelledit.StopName(st.cmds[k])
			if name == "" {
				name = "exit"
			}
			where = fmt.Sprintf(" (%s at line %d)", name, line)
			break
		}
	}
	lead := "this step can stop before its build" + where + "; "
	var msg string
	switch missing := builtIn(g, s); {
	case skip:
		msg = lead + "the rest of the step and the shearbolt gate run only when the build ran. If you add a target to shearbolt.yaml by hand, run setup again, so the gate stops skipping."
	case missing != "":
		msg = lead + "the rest of the step runs only when the build ran, but the shearbolt gate still runs on early stops and fails when " + missing + " was not built, because " + reason + ". " + advice(reason)
	default:
		checks := "every target in shearbolt.yaml"
		if g.Scans != nil {
			checks = strings.Join(g.Scans, ", ")
		}
		msg = lead + "the rest of the step runs only when the build ran, and the shearbolt gate still runs on early stops and checks " + checks + "."
	}
	return plan.Note{Code: "EARLY_STOP", Message: msg, File: e.wf.file, Line: line}
}

func builtIn(g plan.Gate, s int) string {
	if slices.ContainsFunc(g.Images, func(iv plan.ImageVar) bool { return iv.Build.Pos.Step == s }) {
		return "the image"
	}
	if slices.ContainsFunc(g.Files, func(f plan.FileTarget) bool { return f.Build.Pos.Step == s }) {
		return "its artifact"
	}
	return ""
}

func advice(reason string) string {
	switch reason {
	case reasonLaterPush, reasonOtherSteps:
		return "Move the early stop into the job's if: to skip the gate as well."
	case reasonNotNext:
		return "Move the shearbolt gate directly after this step and run setup again."
	case reasonRunStep:
		return "Use the Shearbolt action for the gate (uses: " + gateUses + ") and run setup again."
	case reasonTemplate:
		return "Write the gate's if: without ${{ }} inside it and run setup again."
	}
	return "Remove targets that do not ship from shearbolt.yaml and run setup again, or move the early stop into the job's if:."
}

// A shearbolt scan command in a run step gets the variables in its step's env; its arguments
// and its if: are the user's.
func (e *jobEditor) updateExisting(gi int, g plan.Gate, env [][2]string) error {
	ex := g.Existing
	st := e.j.steps[ex.Pos.Step]
	if st.m == nil {
		return failAt(ex.Pos.Line, "the existing gate step is not a mapping")
	}
	if v := e.d.Get(st.m, "continue-on-error"); v != nil && (v.Kind != yaml.ScalarNode || !strings.EqualFold(strings.TrimSpace(v.Value), "false")) {
		return failAt(ex.Pos.Line, "the existing gate step has continue-on-error: %s, so a failing scan would not stop the job; remove it, or remove the gate and run setup again", value(v))
	}
	if ex.Pos.Command >= 0 {
		if ex.Pos.Command >= len(st.cmds) {
			return failAt(ex.Pos.Line, "the existing scan could not be located in its step")
		}
		if err := shelledit.UsableScan(st.cmds, ex.Pos.Command, ex.Pos.Line); err != nil {
			return err
		}
		if off := shelledit.ErrexitOff(st.cmds[:ex.Pos.Command]); off != nil {
			return failAt(ex.Pos.Line, "%s turns off errexit (set +e) before the existing scan in its step, so a failing scan would not stop the job; turn it back on with set -e before the scan", shelledit.CommandText(*off))
		}
		if st.shellSet && !errexitShell(st.shell) && shelledit.Follows(st.cmds, ex.Pos.Command, nil) {
			return failAt(ex.Pos.Line, "the existing scan's step runs in shell: %s, which has no errexit (-e), and commands follow the scan, so a failing scan would not stop the job; add -e to the shell or make the scan the step's last command", st.shell)
		}
		for _, iv := range g.Images {
			if iv.Build.Pos.Step == ex.Pos.Step {
				return failAt(ex.Pos.Line, "the existing gate runs in the step that builds the image, so it cannot read the image's tag")
			}
		}
		return e.setEnv(st, env)
	}
	if err := e.setEnv(st, env); err != nil {
		return err
	}
	if g.Zosia {
		if err := e.setZosia(st, g.Targets); err != nil {
			return err
		}
	} else if err := e.setTargets(st, g.Targets); err != nil {
		return err
	}
	return e.gateTerm(gi, g, st)
}

// gateTerm keeps the marker term in the if: of existing action gate g right. Any term in
// setup's form is stripped first, whatever its step id, and the decision is made on the rest of
// the condition:
//   - When the step directly before the gate is a split step with the marker (markerBefore), the
//     term is that step's, and it is there exactly when the gate may be skipped (gateMaySkip) and
//     its own condition can be combined with it.
//   - Otherwise a term the gate had is removed, since nothing justifies it any more: kept, a stale
//     one would skip the gate while a push runs, or never let it run.
//
// Conditions are compared normalized, so a term the user reformatted stays as written, and an
// unchanged re-run says nothing.
func (e *jobEditor) gateTerm(gi int, g plan.Gate, st *step) error {
	s := g.Existing.Pos.Step - 1
	id, split := e.markerBefore(gi, s)
	if e.d.Get(st.m, "if") != nil && st.ifNode == nil {
		// An if: that is not a scalar cannot be read as a condition, so it is left alone.
		if split {
			reason := reasonTemplate
			if skip, why := e.gateMaySkip(g, s); !skip {
				reason = why
			}
			e.notes = append(e.notes, e.earlyStopNote(g, s, false, reason))
		}
		return nil
	}
	cur := ""
	if st.ifNode != nil {
		cur = st.ifNode.Value
	}
	// Strip every term in setup's form: an older run may have left one inside another.
	next, oldID, had := cur, "", false
	for {
		rest, termID, ok := stripAnyMarker(next)
		if !ok {
			break
		}
		if !had {
			oldID = termID
		}
		next, had = rest, true
	}
	if id == "" {
		if !had {
			return nil
		}
		if err := e.setIf(st, next); err != nil {
			return err
		}
		e.notes = append(e.notes, plan.Note{
			Code:    "EARLY_STOP",
			Message: fmt.Sprintf("setup removed the early-stop condition from this shearbolt gate, because it no longer directly follows a step that sets steps.%s.outputs.%s; the gate now runs on every run.", oldID, shelledit.ReachedOutput),
			File:    e.wf.file,
			Line:    st.line,
		})
		return nil
	}
	skip, reason := e.gateMaySkip(g, s)
	if skip && templated(next) {
		skip, reason = false, reasonTemplate
	}
	if skip {
		next = withMarker(next, id)
	}
	changed := normCond(next) != normCond(cur)
	if changed {
		if err := e.setIf(st, next); err != nil {
			return err
		}
	}
	if changed || split {
		e.notes = append(e.notes, e.earlyStopNote(g, s, skip, reason))
	}
	return nil
}

// markerBefore returns the id of step s when it is a split step with the marker, "" when it is
// not. Its id is the one renderStep gave it, which a step whose id the user deleted gets back in
// the same run, or else its own.
func (e *jobEditor) markerBefore(gi, s int) (id string, split bool) {
	if s < 0 {
		return "", false
	}
	if sp := e.markerSplit(gi, s); sp != nil {
		return sp.id, true
	}
	prev := e.j.steps[s]
	if prev.run == nil || !holdsMarker(prev) {
		return "", false
	}
	if sp := e.steps[s]; sp != nil && sp.id != "" {
		return sp.id, false
	}
	return prev.id, false
}

// An empty cond removes the if:, or, when it shares its line with the item's dash and cannot be
// deleted, makes it success(), which means the same as no if: and which later runs leave alone.
func (e *jobEditor) setIf(st *step, cond string) error {
	fail := func(err error) error {
		return failAt(st.line, "the existing gate's if: cannot be updated (%v)", err)
	}
	if !editable(st) {
		return fail(fmt.Errorf("the step uses YAML anchors, aliases or flow style"))
	}
	if cond == "" {
		if e.d.DeleteEntry(st.m, "if") == nil {
			return nil
		}
		cond = "success()"
	}
	after := ""
	if slices.Contains(keys(st.m), "name") {
		after = "name"
	}
	return wrapErr(fail, e.d.SetKey(st.m, "if", yamledit.Scalar(cond), after))
}

// imageVarName matches the variables setup manages in a gate's env.
var imageVarName = regexp.MustCompile(`^SHEARBOLT_IMAGE(_[A-Z0-9_]+)?$`)

func (e *jobEditor) setEnv(st *step, desired [][2]string) error {
	m := st.m
	envNode := e.d.Get(m, "env")
	want := map[string]string{}
	for _, kv := range desired {
		want[kv[0]] = kv[1]
	}
	var del []string
	var set [][2]string
	present := map[string]bool{}
	if envNode != nil && envNode.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(envNode.Content); i += 2 {
			k, v := envNode.Content[i].Value, envNode.Content[i+1]
			present[k] = true
			if w, ok := want[k]; ok {
				if v.Kind != yaml.ScalarNode || v.Value != w {
					set = append(set, [2]string{k, w})
				}
			} else if imageVarName.MatchString(k) {
				del = append(del, k)
			}
		}
	}
	for _, kv := range desired {
		if !present[kv[0]] {
			set = append(set, kv)
		}
	}
	if len(del) == 0 && len(set) == 0 {
		return nil
	}
	fail := func(err error) error {
		return failAt(st.line, "the existing gate's env cannot be updated (%v)", err)
	}
	if !editable(st) {
		return fail(fmt.Errorf("the step uses YAML anchors, aliases or flow style"))
	}
	pad := strings.Repeat(" ", e.unit)
	render := func(kvs [][2]string) string {
		var b strings.Builder
		for _, kv := range kvs {
			b.WriteString("\n" + pad + kv[0] + ": " + yamledit.Scalar(kv[1]))
		}
		return b.String()
	}
	if envNode == nil {
		after := ""
		if slices.Contains(keys(m), "uses") {
			after = "uses"
		}
		if err := e.d.SetKey(m, "env", render(desired), after); err != nil {
			return fail(err)
		}
		return nil
	}
	if envNode.Kind != yaml.MappingNode {
		return fail(fmt.Errorf("env is not a mapping"))
	}
	if len(keys(envNode))-len(del) == 0 {
		if len(set) == 0 {
			return wrapErr(fail, e.d.DeleteEntry(m, "env"))
		}
		return wrapErr(fail, e.d.SetKey(m, "env", render(desired), ""))
	}
	for _, kv := range set {
		if err := e.d.SetKey(envNode, kv[0], yamledit.Scalar(kv[1]), ""); err != nil {
			return fail(err)
		}
	}
	for _, k := range del {
		if err := e.d.DeleteEntry(envNode, k); err != nil {
			return fail(err)
		}
	}
	return nil
}

func wrapErr(fail func(error) error, err error) error {
	if err != nil {
		return fail(err)
	}
	return nil
}

func (e *jobEditor) setTargets(st *step, targets []string) error {
	m := st.m
	with := e.d.Get(m, "with")
	has := with != nil && slices.Contains(keys(with), "targets")
	want := strings.Join(targets, " ")
	fail := func(err error) error {
		return failAt(st.line, "the existing gate's targets cannot be updated (%v)", err)
	}
	if targets == nil {
		if !has {
			return nil
		}
		if !editable(st) {
			return fail(fmt.Errorf("the step uses YAML anchors, aliases or flow style"))
		}
		if len(keys(with)) == 1 {
			return wrapErr(fail, e.d.DeleteEntry(m, "with"))
		}
		return wrapErr(fail, e.d.DeleteEntry(with, "targets"))
	}
	if has && value(e.d.Get(with, "targets")) == want {
		return nil
	}
	if !editable(st) {
		return fail(fmt.Errorf("the step uses YAML anchors, aliases or flow style"))
	}
	if with == nil {
		return wrapErr(fail, e.d.SetKey(m, "with", "\n"+strings.Repeat(" ", e.unit)+"targets: "+yamledit.Scalar(want), ""))
	}
	return wrapErr(fail, e.d.SetKey(with, "targets", yamledit.Scalar(want), ""))
}

// setZosia sets the existing gate's targets and adds the zosia-secret input when the step has
// none. Both edits can touch the same with: block, so they are queued together.
func (e *jobEditor) setZosia(st *step, targets []string) error {
	m := st.m
	with := e.d.Get(m, "with")
	if slices.Contains(keys(with), zosiaInput) {
		return e.setTargets(st, targets)
	}
	fail := func(err error) error {
		return failAt(st.line, "the existing gate cannot get the zosia-secret input (%v)", err)
	}
	if !editable(st) {
		return fail(fmt.Errorf("the step uses YAML anchors, aliases or flow style"))
	}
	pad := strings.Repeat(" ", e.unit)
	switch {
	case with == nil && targets != nil:
		// setTargets adds the with: block after the step's last entry; the line follows it there.
		if err := e.setTargets(st, targets); err != nil {
			return err
		}
		kc, err := e.d.KeyColumn(m)
		if err != nil {
			return fail(err)
		}
		ks := keys(m)
		end, err := e.d.EntryEnd(m, ks[len(ks)-1])
		if err != nil {
			return fail(err)
		}
		e.zosia = true
		return wrapErr(fail, e.d.InsertAfter(end, []string{strings.Repeat(" ", kc) + pad + zosiaLine}))
	case targets == nil && slices.Equal(keys(with), []string{"targets"}):
		// setTargets would remove the whole with: block; the block stays and its only line changes.
		e.zosia = true
		return wrapErr(fail, e.d.SetKey(m, "with", "\n"+pad+zosiaLine, ""))
	}
	if err := e.setTargets(st, targets); err != nil {
		return err
	}
	return e.addZosia(st)
}

// addZosia adds the zosia-secret input to a gate step that has none, leaving its other inputs alone.
func (e *jobEditor) addZosia(st *step) error {
	with := e.d.Get(st.m, "with")
	if slices.Contains(keys(with), zosiaInput) {
		return nil
	}
	fail := func(err error) error {
		return failAt(st.line, "the existing gate cannot get the zosia-secret input (%v)", err)
	}
	if !editable(st) {
		return fail(fmt.Errorf("the step uses YAML anchors, aliases or flow style"))
	}
	e.zosia = true
	if with == nil {
		return wrapErr(fail, e.d.SetKey(st.m, "with", "\n"+strings.Repeat(" ", e.unit)+zosiaLine, ""))
	}
	return wrapErr(fail, e.d.SetKey(with, zosiaInput, strings.TrimPrefix(zosiaLine, zosiaInput+": "), ""))
}

func (e *jobEditor) removeZosia(st *step) error {
	with := e.d.Get(st.m, "with")
	if !slices.Contains(keys(with), zosiaInput) {
		return nil
	}
	fail := func(err error) error {
		return failAt(st.line, "the gate's zosia-secret input cannot be removed (%v)", err)
	}
	if !editable(st) {
		return fail(fmt.Errorf("the step uses YAML anchors, aliases or flow style"))
	}
	if len(keys(with)) == 1 {
		return wrapErr(fail, e.d.DeleteEntry(st.m, "with"))
	}
	return wrapErr(fail, e.d.DeleteEntry(with, zosiaInput))
}

// SetZosia implements plan.ZosiaEditor.
func (p Plugin) SetZosia(file string, src []byte, gates []plan.GateCmd, on bool) ([]byte, map[int]error, error) {
	wf, err := parseWorkflow(file, src)
	if err != nil {
		return nil, nil, err
	}
	d := wf.doc
	failed := map[int]error{}
	for i, g := range gates {
		cp := d.Checkpoint()
		err := fmt.Errorf("the step is not in the workflow")
		if j := wf.job(g.Pos.Job); j != nil && g.Pos.Step < len(j.steps) && j.steps[g.Pos.Step].m != nil {
			e := &jobEditor{d: d, wf: wf, j: j, unit: yamledit.IndentUnit(d)}
			if on {
				err = e.addZosia(j.steps[g.Pos.Step])
			} else {
				err = e.removeZosia(j.steps[g.Pos.Step])
			}
		}
		if err == nil {
			if _, rerr := d.Result(); rerr != nil {
				err = rerr
			}
		}
		if err != nil {
			d.Rollback(cp)
			failed[i] = err
		}
	}
	res, err := d.Result()
	return res, failed, err
}

// errexitShell reports whether a shell: setting stops at a failing command: GitHub's bash and sh
// (which it runs with -e), or a custom shell with -e or -o errexit.
func errexitShell(name string) bool {
	f := strings.Fields(name)
	if len(f) == 1 && (f[0] == "bash" || f[0] == "sh") {
		return true
	}
	for i, w := range f {
		switch {
		case w == "-o" && i+1 < len(f) && f[i+1] == "errexit":
			return true
		case strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "e"):
			return true
		}
	}
	return false
}
