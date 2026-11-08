package github

import (
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/shelledit"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

// decideSplit splits the step after its last gated build when another command follows it in the
// gate's step (P1), or when one of its gated builds uses --push. A step that cannot be split
// fails the job (R4 rule 5 (a)-(d)).
func (e *jobEditor) decideSplit(sp *stepPlan) error {
	if len(sp.builds) == 0 || sp.st.run == nil {
		return nil
	}
	g := e.gates[sp.gate]
	st := sp.st
	last := sp.builds[len(sp.builds)-1].Pos.Command
	// P1, for each gate that follows a build in this step: a command after its build that is not
	// another gated build of the step needs a split.
	gated := map[int]bool{}
	for _, b := range sp.builds {
		gated[b.Pos.Command] = true
	}
	need := sp.hasPush
	for _, og := range e.gates {
		if og.Existing == nil && !og.Before && og.At.Step == sp.s && og.At.Command >= 0 && shelledit.Follows(st.cmds, og.At.Command, gated) {
			need = true
		}
	}
	if !need {
		return nil
	}
	// Setup's own lines right after the build stay with it, so the output line is written by
	// the step whose id the gate reads.
	for last+1 < len(st.cmds) && shelledit.OwnLine(st.cmds[last+1]) {
		last++
	}
	cmd := st.cmds[last]
	line := st.cmdLine(cmd)
	switch {
	case sp.multi:
		return failAt(line, "builds of two projects share this step, which would need two splits")
	case !st.posix:
		return failAt(line, "this step does not run in bash or sh, so setup cannot split it after %s", shelledit.CommandText(cmd))
	case g.Existing != nil && g.Existing.Pos.Command >= 0 && g.Existing.Pos.Step == sp.s:
		return failAt(line, "the existing gate runs in the same step as the --push build, so the push cannot move after it")
	}
	if err := shelledit.Insertable(cmd, line, "split the step after it"); err != nil {
		return err
	}
	state, err := carriedState(st.script, st.cmds[:last], st)
	if err != nil {
		return err
	}
	sp.split, sp.state, sp.shellEdits = true, state, true
	sp.cut, sp.rest = shelledit.SplitPoints(st.script, cmd)
	return nil
}

// carried are the commands whose state the remainder re-creates by running them again.
var carried = []string{"cd", "export", "unset", "set", "shopt", "umask", "ulimit", "declare", "typeset", "readonly"}

// uncarried change shell state in ways a new step cannot re-create safely (R4 rule 5 (d)).
var uncarried = []string{"source", ".", "pushd", "popd", "alias", "unalias", "eval", "trap", "read", "mapfile", "readarray", "local", "hash", "enable"}

// carriedState returns the commands before the split point that set shell state, to be copied
// in front of the remainder: cd, set, export and bare assignments without command substitution,
// and the like. Anything that cannot be carried, or a state change inside a conditional or
// loop, fails the split.
func carriedState(script string, cmds []shell.Command, st *step) ([]string, error) {
	var out []string
	for i, c := range cmds {
		line := st.cmdLine(c)
		text := script[c.Start:c.End]
		argv := c.Argv()
		bare := len(argv) == 0 && len(c.Words) > 0
		name := ""
		if len(argv) > 0 {
			name = argv[0]
		}
		if isFunction(c) {
			return nil, failAt(line, "the step defines a shell function (%s), which a split step would not have", shelledit.CommandText(c))
		}
		if !bare && !slices.Contains(carried, name) && !slices.Contains(uncarried, name) && !(name == "exec" && len(argv) == 1) {
			continue
		}
		subst := strings.Contains(text, "$(") || strings.Contains(text, "`")
		after := ""
		if i > 0 {
			after = cmds[i-1].Op
		}
		switch {
		case c.Depth > 0:
			return nil, failAt(line, "%s changes the shell state inside a conditional or loop, which a split step cannot re-create", shelledit.CommandText(c))
		case after == "&&" || after == "||" || after == "|" || c.Op == "|" || c.Op == "&":
			return nil, failAt(line, "%s runs only under a condition or in a pipeline, so a split step cannot re-create its state", shelledit.CommandText(c))
		case slices.Contains(uncarried, name), name == "exec":
			return nil, failAt(line, "%s changes the shell state in a way a split step cannot re-create", shelledit.CommandText(c))
		case subst:
			return nil, failAt(line, "%s uses command substitution, so a split step cannot re-create it", shelledit.CommandText(c))
		case name == "cd" && len(argv) > 1 && argv[1] == "-":
			return nil, failAt(line, "%s depends on the previous directory, which a split step does not have", shelledit.CommandText(c))
		case name == "export" && slices.Contains(argv[1:], "-f"):
			return nil, failAt(line, "%s exports a shell function, which a split step would not have", shelledit.CommandText(c))
		}
		out = append(out, text)
	}
	return out, nil
}

// isFunction reports a function definition: `name () ...` or `function name ...`.
func isFunction(c shell.Command) bool {
	if len(c.Words) == 0 {
		return false
	}
	if c.Words[0].Raw == "function" {
		return true
	}
	return strings.HasSuffix(c.Words[0].Raw, "()") || len(c.Words) > 1 && strings.HasPrefix(c.Words[1].Raw, "()")
}

// remainderLines renders the new step that holds a split step's remainder: its name, the copied
// if (made safe), env, shell, working-directory, continue-on-error and timeout-minutes, and run.
func (e *jobEditor) remainderLines(sp *stepPlan) ([]string, error) {
	st := sp.st
	name := "Continue after shearbolt"
	if st.name != "" {
		name = st.name + " (continued)"
	}
	out := []string{"name: " + yamledit.Scalar(name)}
	for _, k := range keys(st.m) {
		switch k {
		case "if":
			if st.ifNode != nil {
				cond, err := safeIf(st.ifNode.Value)
				if err != nil {
					return nil, failAt(st.ifNode.Line, "the continued step would run after a failed gate, and %v", err)
				}
				out = append(out, "if: "+yamledit.Scalar(cond))
			}
		case "env", "shell", "working-directory", "continue-on-error", "timeout-minutes":
			lines, err := e.entryLines(st.m, k)
			if err != nil {
				return nil, failAt(st.line, "its %s cannot be copied to the continued step (%v)", k, err)
			}
			out = append(out, lines...)
		}
	}
	run := yamledit.RenderScript(sp.remainder, 0, 0, e.unit)
	out = append(out, "run: "+run[0])
	return append(out, run[1:]...), nil
}

// entryLines returns key's entry of mapping m as written, relative to m's key column.
func (e *jobEditor) entryLines(m *yaml.Node, key string) ([]string, error) {
	i := slices.Index(keys(m), key)
	k, v := m.Content[2*i], m.Content[2*i+1]
	if hasAnchor(k) || hasAnchor(v) {
		return nil, fmt.Errorf("it defines a YAML anchor")
	}
	last, err := e.d.EntryEnd(m, key)
	if err != nil {
		return nil, err
	}
	kc := k.Column - 1
	src := e.d.Line(k.Line)
	out := []string{src[byteOffset(src, kc):]}
	for l := k.Line + 1; l <= last; l++ {
		t := e.d.Line(l)
		strip := min(kc, len(t)-len(strings.TrimLeft(t, " ")))
		out = append(out, t[strip:])
	}
	return out, nil
}
