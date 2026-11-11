package github

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

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
	state, dropped, err := carriedState(st.script, st.cmds[:last], st)
	if err != nil {
		return err
	}
	sp.split, sp.state, sp.dropped, sp.shellEdits, sp.last = true, state, dropped, true, last
	sp.cut, sp.rest = shelledit.SplitPoints(st.script, cmd)
	// A stop before the split point ends part 1 successfully, and the remainder would run
	// although the script never got there (design §3.2): part 1 then ends with setup's marker
	// line, which the remainder checks through the step's id.
	sp.marker = slices.ContainsFunc(e.pj.Steps[sp.s].Stops, func(k int) bool {
		return k >= 0 && k < len(st.cmds) && st.cmds[k].End <= sp.cut
	})
	if sp.marker {
		sp.needsID = true
	}
	return nil
}

// carried are the commands whose state the remainder re-creates by running them again.
var carried = []string{"cd", "export", "unset", "set", "shopt", "umask", "ulimit", "declare", "typeset", "readonly"}

// uncarried change shell state in ways a new step cannot re-create safely (R4 rule 5 (d)).
var uncarried = []string{"source", ".", "pushd", "popd", "alias", "unalias", "eval", "trap", "read", "mapfile", "readarray", "local", "hash", "enable"}

// Why carriedState cannot re-create a command's state; %s is the command.
const (
	condMsg  = "%s changes the shell state inside a conditional or loop, which a split step cannot re-create"
	underMsg = "%s runs only under a condition or in a pipeline, so a split step cannot re-create its state"
	substMsg = "%s uses command substitution, so a split step cannot re-create it"
)

// droppedAssign is an assignment before the split point that the split step cannot re-create
// and leaves out (design §4). renderStep fails the split with reason when the continued step
// may read one of its names (assignRead), so only shell-local names, like live, are ever left
// out: a name with no lowercase letter, or one in an env:, may be an inherited variable.
type droppedAssign struct {
	names  []string
	line   int
	text   string // the command, as messages show it
	reason string // one of condMsg, underMsg and substMsg
	cmd    int    // the command's index in the step
}

// carriedState returns the commands before the split point that set shell state, to be copied
// in front of the remainder: cd, set, export and bare assignments without command substitution,
// and the like. Anything that cannot be carried, or a state change inside a conditional or
// loop, fails the split, except a command made up only of assignments that is not in a pipeline
// or in the background: it is returned in dropped, and fails the split only when the continued
// step may read one of its names (renderStep, assignRead), which includes every name that may
// be inherited from the environment.
func carriedState(script string, cmds []shell.Command, st *step) (state []string, dropped []droppedAssign, err error) {
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
			return nil, nil, failAt(line, "the step defines a shell function (%s), which a split step would not have", shelledit.CommandText(c))
		}
		if !bare && !slices.Contains(carried, name) && !slices.Contains(uncarried, name) && !(name == "exec" && len(argv) == 1) {
			continue
		}
		subst := strings.Contains(text, "$(") || strings.Contains(text, "`")
		after := ""
		if i > 0 {
			after = cmds[i-1].Op
		}
		piped := after == "|" || c.Op == "|" || c.Op == "&"
		reason := ""
		switch {
		case c.Depth > 0:
			reason = condMsg
		case after == "&&" || after == "||" || piped:
			reason = underMsg
		case slices.Contains(uncarried, name), name == "exec":
			return nil, nil, failAt(line, "%s changes the shell state in a way a split step cannot re-create", shelledit.CommandText(c))
		case subst:
			reason = substMsg
		case name == "cd" && len(argv) > 1 && argv[1] == "-":
			return nil, nil, failAt(line, "%s depends on the previous directory, which a split step does not have", shelledit.CommandText(c))
		case name == "export" && slices.Contains(argv[1:], "-f"):
			return nil, nil, failAt(line, "%s exports a shell function, which a split step would not have", shelledit.CommandText(c))
		}
		switch {
		case reason == "":
			state = append(state, text)
		case len(c.Words) > 0 && len(c.Prefix()) == len(c.Words) && !piped:
			dropped = append(dropped, droppedAssign{names: c.Prefix(), line: line, text: shelledit.CommandText(c), reason: reason, cmd: i})
		default:
			return nil, nil, failAt(line, reason, shelledit.CommandText(c))
		}
	}
	return state, dropped, nil
}

// assignRead reports whether a split step's continued text rem may read one of names, which an
// assignment that the split step leaves out sets (design §4). A name counts as read when it may
// be an inherited environment variable, which every child process of rem reads without naming
// it: when it is one of env, the env: keys of the workflow, the job and the step, or has no
// lowercase letter, since the runner, $GITHUB_ENV from an earlier step or the shell (PATH, IFS)
// can set such a name and setup cannot tell. It also counts as read when it is a whole word
// anywhere in rem, which covers arithmetic, [[ -v ]] and comments; and every name does when rem
// uses indirect or prefix expansion, or runs eval, source, ., a nameref or a command that
// prints variables (also inside a substitution), when script exports the name or makes it
// readonly, or when allexport is set by one of before, the commands ahead of the assignment. A
// false match only fails the split.
func assignRead(names []string, rem, script string, before []shell.Command, env []string) bool {
	for _, n := range names {
		if slices.Contains(env, n) || !strings.ContainsFunc(n, unicode.IsLower) {
			return true
		}
		if regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(n) + `([^A-Za-z0-9_]|$)`).MatchString(rem) {
			return true
		}
	}
	if strings.Contains(rem, "${!") || slices.ContainsFunc(commandsIn(rem, 0), readsAny) {
		return true
	}
	cmds, _ := shell.Parse(script, shell.State{})
	if slices.ContainsFunc(cmds, func(c shell.Command) bool { return exportsName(c, names) }) {
		return true
	}
	return slices.ContainsFunc(before, allexport)
}

// commandsIn parses text into commands, adding the commands of the command and process
// substitutions in their words, to a few levels. The text after an opening $(, ` or <( is
// parsed to the word's end, so what follows the substitution can only add commands.
func commandsIn(text string, depth int) []shell.Command {
	cmds, _ := shell.Parse(text, shell.State{})
	out := slices.Clone(cmds)
	if depth >= 4 {
		return out
	}
	for _, c := range cmds {
		words := slices.Clone(c.Words)
		for _, r := range c.Redirs {
			words = append(words, r.Target)
		}
		for _, w := range words {
			for i := 0; i < len(w.Raw); i++ {
				rest := w.Raw[i:]
				switch {
				case strings.HasPrefix(rest, "$(("):
					i += 2 // arithmetic, not a command
				case strings.HasPrefix(rest, "$("), strings.HasPrefix(rest, "<("), strings.HasPrefix(rest, ">("):
					out = append(out, commandsIn(rest[2:], depth+1)...)
				case rest[0] == '`':
					out = append(out, commandsIn(rest[1:], depth+1)...)
				}
			}
		}
	}
	return out
}

// builtinArgv is a command's Argv without a leading builtin.
func builtinArgv(c shell.Command) []string {
	argv := c.Argv()
	if len(argv) > 0 && argv[0] == "builtin" {
		argv = argv[1:]
	}
	return argv
}

// options splits args into the letters of their leading options (-x or +x clusters, up to --)
// and the arguments after them.
func options(args []string) (letters string, rest []string) {
	for i, a := range args {
		if a == "--" {
			return letters, args[i+1:]
		}
		if len(a) < 2 || a[0] != '-' && a[0] != '+' {
			return letters, args[i:]
		}
		letters += a[1:]
	}
	return letters, nil
}

// readsAny reports a command that can read a variable it does not name: eval, source and ., a
// nameref, or a command that prints variables.
func readsAny(c shell.Command) bool {
	argv := builtinArgv(c)
	if len(argv) == 0 {
		return false
	}
	opts, args := options(argv[1:])
	switch argv[0] {
	case "eval", "source", ".":
		return true
	case "set":
		return len(argv) == 1
	case "declare", "typeset", "local":
		return len(args) == 0 || strings.ContainsAny(opts, "np")
	case "export":
		return len(args) == 0 || strings.Contains(opts, "p")
	case "compgen":
		for i, a := range argv[1:] {
			if a == "variable" && argv[i] == "-A" || a == "-Avariable" || len(a) > 1 && a[0] == '-' && a[1] != 'A' && strings.Contains(a, "v") {
				return true
			}
		}
	}
	return false
}

// exportsName reports a command that exports one of names or makes it readonly: export,
// readonly, or declare, typeset or local with -x or -r.
func exportsName(c shell.Command, names []string) bool {
	argv := builtinArgv(c)
	if len(argv) == 0 {
		return false
	}
	opts, args := options(argv[1:])
	switch argv[0] {
	case "export", "readonly":
	case "declare", "typeset", "local":
		if !strings.ContainsAny(opts, "xr") {
			return false
		}
	default:
		return false
	}
	return slices.ContainsFunc(args, func(a string) bool { return slices.Contains(names, varName(a)) })
}

// varName returns NAME for an argument written NAME, NAME=…, NAME+=… or NAME[i]…, else "".
func varName(a string) string {
	i := 0
	for i < len(a) && (a[i] == '_' || a[i] >= 'a' && a[i] <= 'z' || a[i] >= 'A' && a[i] <= 'Z' || i > 0 && a[i] >= '0' && a[i] <= '9') {
		i++
	}
	if rest := a[i:]; i > 0 && (rest == "" || rest[0] == '=' || rest[0] == '[' || strings.HasPrefix(rest, "+=")) {
		return a[:i]
	}
	return ""
}

// allexport reports set -a, set -o allexport or shopt -o allexport, after which every
// assignment exports its name.
func allexport(c shell.Command) bool {
	argv := builtinArgv(c)
	if len(argv) == 0 {
		return false
	}
	switch argv[0] {
	case "set":
		return slices.ContainsFunc(argv[1:], func(a string) bool {
			return a == "allexport" || len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Contains(a, "a")
		})
	case "shopt":
		return slices.Contains(argv[1:], "allexport")
	}
	return false
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
// When the step can stop early, its if: also checks setup's marker (continuedIf), and it gets
// one, right after its name, even when the step had none.
func (e *jobEditor) remainderLines(sp *stepPlan) ([]string, error) {
	st := sp.st
	name := "Continue after shearbolt"
	if st.name != "" {
		name = st.name + " (continued)"
	}
	out := []string{"name: " + yamledit.Scalar(name)}
	ifLine := ""
	if st.ifNode != nil || sp.marker {
		copied, at := "", st.line
		if st.ifNode != nil {
			copied, at = st.ifNode.Value, st.ifNode.Line
		}
		var cond string
		var err error
		if sp.marker {
			if cond, err = continuedIf(copied, sp.id); err != nil {
				return nil, failAt(at, "the continued step must run only when the build ran and the gate passed, and %v", err)
			}
		} else if cond, err = safeIf(copied); err != nil {
			return nil, failAt(at, "the continued step would run after a failed gate, and %v", err)
		}
		ifLine = "if: " + yamledit.Scalar(cond)
	}
	if st.ifNode == nil && ifLine != "" {
		out = append(out, ifLine)
	}
	for _, k := range keys(st.m) {
		switch k {
		case "if":
			if st.ifNode != nil {
				out = append(out, ifLine)
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
