package setup

import (
	"bytes"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// stops reports whether cmd can end the shell it runs in early and successfully. exit $? or a
// bare exit right after || does not count: $? is then the failed command's non-zero status.
// Heredoc bodies are parsed as commands, so an exit in one (ssh host <<EOF ... exit ... EOF) is a
// false positive.
// depth is the level of the file a source here would read: 1 for a CI command.
func (c *classifier) stops(cmd shell.Command, ctx plan.Context, depth int) bool {
	args := cmd.ArgWords()
	if leadingWord(cmd) == "exec" {
		return slices.ContainsFunc(args, func(w shell.Word) bool { return !redirectWord(w) })
	}
	if len(args) == 0 {
		return false
	}
	switch args[0].Value {
	case "exit":
		v := "$?" // a bare exit exits with the last command's status
		if len(args) > 1 {
			v = args[1].Value
		}
		if cmd.OpBefore == "||" && (v == "$?" || v == "${?}") {
			return false
		}
		return successStatus(v)
	case "source", ".":
		return c.sourceStops(cmd, ctx, depth)
	}
	return false
}

var fdVariable = regexp.MustCompile(`^\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// redirectWord reports a word the lexer returns as an argument that belongs to a redirect: a
// process substitution, >(…) or <(…), as in exec > >(tee log), or an fd variable, as in
// exec {fd}>log. An exec with only such words is a bare exec.
func redirectWord(w shell.Word) bool {
	return strings.HasPrefix(w.Raw, ">(") || strings.HasPrefix(w.Raw, "<(") || fdVariable.MatchString(w.Raw)
}

var shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^\]]*\])?\+?=`)

func leadingWord(cmd shell.Command) string {
	for _, w := range cmd.Words {
		if !shellAssignment.MatchString(w.Raw) {
			return w.Value
		}
	}
	return ""
}

// successStatus reports whether exit v may succeed: v is not a literal number (a variable such as
// "$code"), or it is one whose status, modulo 256, is 0.
func successStatus(v string) bool {
	n, err := strconv.Atoi(v)
	return err != nil || n%256 == 0
}

func (c *classifier) sourceStops(cmd shell.Command, ctx plan.Context, depth int) bool {
	t, isCall := callOf(c, cmd, ctx)
	switch {
	case !isCall:
		return false // outside the repository, or nothing to read
	case t == nil || depth > maxCallDepth:
		return true
	}
	src := c.read(t.file)
	if src == nil || !utf8.Valid(src) || bytes.IndexByte(src, 0) >= 0 {
		return true
	}
	env := slices.Sorted(maps.Keys(ctx.Env))
	cmds, _ := shell.Parse(string(src), shell.State{Dir: t.dir, Env: env, Self: path.Dir(t.file)})
	return slices.ContainsFunc(cmds, func(sub shell.Command) bool { return c.stops(sub, contextOf(sub), depth+1) })
}
