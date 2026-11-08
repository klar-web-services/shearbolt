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

// gateEnv returns the image variables gate g passes: a run-step build's output, or the scan tag
// itself for an action build.
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

// placeGate inserts gate g, or updates the existing gate it names (R8), and puts the remainders
// of its split steps after it.
func (e *jobEditor) placeGate(gi int, g plan.Gate) error {
	env, err := e.gateEnv(g)
	if err != nil {
		return err
	}
	anchor := g.At.Step
	switch {
	case g.Existing != nil:
		anchor = g.Existing.Pos.Step
		if err := e.updateExisting(g, env); err != nil {
			return err
		}
	case g.Before:
		if err := e.d.InsertItem(e.j.seq, g.At.Step-1, gateLines(g, "", env, e.unit)); err != nil {
			return failAt(e.j.steps[g.At.Step].line, "setup cannot add the gate step (%v)", err)
		}
	default:
		st := e.j.steps[g.At.Step]
		ifCond, err := e.gateIf(g)
		if err != nil {
			return err
		}
		if err := e.d.InsertItem(e.j.seq, g.At.Step, gateLines(g, ifCond, env, e.unit)); err != nil {
			return failAt(st.line, "setup cannot add the gate step (%v)", err)
		}
	}
	for _, sp := range e.sortedPlans() {
		if sp.gate != gi {
			continue
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

// gateIf returns the if: a new gate copies: the condition shared by every step that holds one of
// its builds (--push steps included), "" when they have none. Builds under different conditions
// fail the job: a gate that follows one of them would be skipped while another build is pushed
// unscanned, or run where an image was never built.
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

// updateExisting brings an existing gate up to date (R8): its image variables (set, added, or
// removed when no longer needed) and its targets input. A shearbolt scan command in a run step
// gets the variables in its step's env; its arguments are the user's.
func (e *jobEditor) updateExisting(g plan.Gate, env [][2]string) error {
	ex := g.Existing
	st := e.j.steps[ex.Pos.Step]
	if st.m == nil {
		return failAt(ex.Pos.Line, "the existing gate step is not a mapping")
	}
	// A gate step that continues on error cannot stop the job (Batch 5 re-review).
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
	return e.setTargets(st, g.Targets)
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

// errexitShell reports whether a shell: setting stops at a failing command: GitHub's bash and sh
// (which it runs with -e), or a custom shell with -e (alone or in a group such as -eo) or -o errexit.
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
