package setup

import (
	"bytes"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// This file finds the commands that can end the shell they run in early and successfully (design
// §3.2, §3.5, P6). Plugins use them to tell when a gate placed after such a command can be
// skipped.

// stops reports whether cmd can end the shell it runs in early and successfully (design §3.2,
// §3.5): exit without an argument, with 0 or with an argument that is not a literal number (a
// literal non-zero exit fails the shell and does not count); exec with a command (bare exec
// only redirects); or source/. of a file in the repository that
// contains such a command anywhere (followed recursively up to maxCallDepth) or that cannot be
// read or followed. A sourced file outside the repository (~/.cargo/env) does not count, the
// same rule callOf uses. bash/sh x runs a child shell and never counts.
//
// The exit status is taken modulo 256, as the shell does, so exit 256 counts too. exit $? or a
// bare exit right after || does not count: $? is then the failed command's non-zero status, and
// a bare exit exits with it. Heredoc bodies are parsed as commands, so an exit in one
// (ssh host <<EOF ... exit ... EOF) is a false positive.
// depth is the level of the file a source here would read: 1 for a CI command.
func (c *classifier) stops(cmd shell.Command, ctx plan.Context, depth int) bool {
	if leadingWord(cmd) == "exec" {
		return len(cmd.ArgWords()) > 0
	}
	args := cmd.ArgWords()
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
		return len(args) == 1 || successStatus(v)
	case "source", ".":
		return c.sourceStops(cmd, ctx, depth)
	}
	return false
}

// shellAssignment matches a word that assigns a shell variable: NAME=…, NAME+=… or NAME[i]=….
var shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^\]]*\])?\+?=`)

// leadingWord returns the value of cmd's first word after its VAR=x assignments, or "".
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

// sourceStops reports whether the file cmd (source x or . x) reads can stop the shell: it cannot
// be resolved, read or followed (the depth limit, a binary file), or one of its commands stops.
// The file's commands run in the caller's directory.
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
