package setup

import (
	"bytes"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// This file follows CI commands into the repository files they run (R2 "Called files"):
// Makefiles (make, gmake, $(MAKE)), justfiles (just) and shell scripts (a relative path with a
// slash, x.sh, bash|sh|zsh x, source x, . x). A command that is recognizably such a call but
// cannot be followed counts as a push, so the gate goes before it and checks the source.

// maxCallDepth is how deep calls are followed: the CI command's file is depth 1.
const maxCallDepth = 3

// call is one command that runs an in-repo file.
type call struct {
	kind  string   // "make", "just" or "script"
	file  string   // repo-relative
	dir   string   // where the file's commands run
	goals []string // make targets, or just recipes and their arguments; nil = the default
	env   []string // variables the call puts in the commands' environment (make VAR=x)
}

func (t *call) key() string {
	return t.kind + "\x00" + t.file + "\x00" + t.dir + "\x00" + strings.Join(t.goals, "\x00")
}

// callOf returns the file call cmd makes. isCall also reports a recognizable call that cannot be
// resolved (t == nil): an unknown directory, a variable or outside path, a missing file,
// Makefile or justfile. Callers count such a call as a push. Not calls: programs installed
// outside the repository (absolute, home or tool-home paths without .sh), programs the job
// built (a missing path under target, node_modules, dist, build or out), scripts read from stdin
// or given inline (-c), and sourced files outside the repository (~/.cargo/env,
// "$NVM_DIR/nvm.sh").
func callOf(c *classifier, cmd shell.Command, ctx plan.Context) (t *call, isCall bool) {
	args := cmd.ArgWords()
	if len(args) == 0 {
		return nil, false
	}
	self := cmd.Before.Self
	prog := args[0].Value
	switch base := path.Base(prog); {
	case base == "make" || base == "gmake" || prog == "$(MAKE)" || prog == "${MAKE}":
		return makeCall(c.r, args[1:], ctx.Dir, self)
	case base == "just":
		return justCall(c, args[1:], ctx.Dir, self)
	case slices.Contains(shells, base):
		return shellCall(args[1:], ctx.Dir, self, false)
	case prog == "source" || prog == ".":
		return shellCall(args[1:], ctx.Dir, self, true)
	case strings.HasSuffix(prog, ".sh") || strings.Contains(prog, "/"):
		sh := strings.HasSuffix(prog, ".sh")
		if outsideRepo(prog) && !sh {
			return nil, false
		}
		f, ok := callPath(ctx.Dir, self, prog)
		if !sh && builtProgram(c.r, prog, f, ok) {
			return nil, false
		}
		if !ok {
			return nil, true
		}
		return &call{kind: "script", file: f, dir: ctx.Dir}, true
	}
	return nil, false
}

// toolHomes are variables that name directories outside the repository where tools and their
// environment scripts live.
var toolHomes = []string{"HOME", "NVM_DIR", "CARGO_HOME", "VIRTUAL_ENV", "CONDA_PREFIX"}

// outsideRepo reports a path that names something outside the repository: absolute, ~, or
// below a tool home such as $HOME or "${CARGO_HOME}".
func outsideRepo(p string) bool {
	if path.IsAbs(p) || strings.HasPrefix(p, "~") {
		return true
	}
	for _, h := range toolHomes {
		for _, v := range []string{"$" + h, "${" + h + "}"} {
			if p == v || strings.HasPrefix(p, v+"/") {
				return true
			}
		}
	}
	return false
}

// outputDirs are the directories builds write to. A missing program there was built by the job.
var outputDirs = []string{"target", "node_modules", "dist", "build", "out"}

// builtProgram reports a program the job built and then runs (./target/release/demo --version,
// ./node_modules/.bin/x): a path that does not exist in the repository with an output directory
// among its components. Files committed in such a directory are followed as usual.
func builtProgram(r plan.Repo, prog, resolved string, ok bool) bool {
	if ok && r.Exists(resolved) {
		return false
	}
	var parts []string
	if !variable(prog) {
		parts = strings.Split(path.Clean(prog), "/")
	}
	if ok {
		parts = append(parts, strings.Split(resolved, "/")...)
	}
	return slices.ContainsFunc(parts, func(p string) bool { return slices.Contains(outputDirs, p) })
}

// callPath resolves the path of a called file or directory: relative to dir; relative to the
// script's own directory self for $(dirname "$0")/x; or relative to the repository root after a
// clone-directory variable ($GITHUB_WORKSPACE/x). It fails for anything that is not a certain
// path inside the repository.
func callPath(dir, self, p string) (string, bool) {
	if d, ok := shell.ScriptDir(p, self); ok {
		return d, true
	}
	for _, v := range cloneDirs {
		if rest, ok := strings.CutPrefix(p, v+"/"); ok {
			return relPath(".", rest)
		}
	}
	return relPath(dir, p)
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*\s*(::?|:::|\?|\+|!)?=`)

func makeCall(r plan.Repo, args []shell.Word, dir, self string) (*call, bool) {
	t := &call{kind: "make", dir: dir}
	file := ""
	chdir := func(d string) bool {
		var ok bool
		t.dir, ok = callPath(t.dir, self, d)
		return ok
	}
	value := func(i *int) string {
		if *i+1 < len(args) {
			*i++
			return args[*i].Value
		}
		return ""
	}
	for i := 0; i < len(args); i++ {
		v := args[i].Value
		switch {
		case slices.Contains([]string{"-v", "--version", "-h", "--help"}, v):
			return nil, false
		case v == "-C" || v == "--directory":
			if !chdir(value(&i)) {
				return nil, true
			}
		case strings.HasPrefix(v, "--directory="):
			if !chdir(strings.TrimPrefix(v, "--directory=")) {
				return nil, true
			}
		case strings.HasPrefix(v, "-C"):
			if !chdir(v[2:]) {
				return nil, true
			}
		case v == "-f" || v == "--file" || v == "--makefile":
			file = value(&i)
		case strings.HasPrefix(v, "--file=") || strings.HasPrefix(v, "--makefile="):
			_, file, _ = strings.Cut(v, "=")
		case strings.HasPrefix(v, "-f") && !strings.HasPrefix(v, "--"):
			file = v[2:]
		case slices.Contains([]string{"-I", "-o", "-W", "-E", "--include-dir", "--old-file", "--assume-old", "--new-file", "--assume-new", "--what-if", "--eval"}, v):
			i++
		case slices.Contains([]string{"-j", "-l", "--jobs", "--load-average", "--max-load"}, v):
			if i+1 < len(args) && strings.Trim(args[i+1].Value, "0123456789.") == "" {
				i++
			}
		case strings.HasPrefix(v, "-"):
		case assignment.MatchString(v):
			t.env = append(t.env, strings.TrimRight(strings.SplitN(v, "=", 2)[0], " :?+!"))
		default:
			t.goals = append(t.goals, v)
		}
	}
	if file != "" {
		f, ok := callPath(t.dir, self, file)
		if !ok {
			return nil, true
		}
		t.file = f
		return t, true
	}
	if t.dir == "" {
		return nil, true
	}
	for _, n := range []string{"GNUmakefile", "makefile", "Makefile"} {
		if f := path.Join(t.dir, n); r.Exists(f) {
			t.file = f
			return t, true
		}
	}
	return nil, true
}

func justCall(c *classifier, args []shell.Word, dir, self string) (*call, bool) {
	t := &call{kind: "just"}
	file, wd := "", ""
	withValue := []string{"--shell", "--shell-arg", "--dotenv-filename", "--dotenv-path", "--color", "--command-color",
		"--list-heading", "--list-prefix", "--chooser", "--timestamp-format", "--tempdir"}
	noRecipes := []string{"-c", "--command", "-l", "--list", "--summary", "--dump", "--evaluate", "-s", "--show",
		"--choose", "--init", "--edit", "--fmt", "--variables", "--completions", "-h", "--help", "-V", "--version", "--groups", "--usage"}
	for i := 0; i < len(args); i++ {
		v := args[i].Value
		name, val, eq := strings.Cut(v, "=")
		next := func() string {
			if eq {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i].Value
			}
			return ""
		}
		switch {
		case name == "-f" || name == "--justfile":
			file = next()
		case name == "-d" || name == "--working-directory":
			wd = next()
		case name == "--set":
			i += 2
		case slices.Contains(withValue, name):
			next()
		case slices.Contains(noRecipes, name):
			return nil, false
		case strings.HasPrefix(v, "-"):
		case assignment.MatchString(v):
		default:
			t.goals = append(t.goals, v)
		}
	}
	switch {
	case file != "":
		f, ok := callPath(dir, self, file)
		if !ok {
			return nil, true
		}
		t.file, t.dir = f, path.Dir(f)
	case dir == "":
		return nil, true
	default:
		for d := dir; t.file == ""; d = path.Dir(d) {
			names, _ := c.r.ReadDir(d)
			for _, n := range names {
				if strings.EqualFold(n, "justfile") || strings.EqualFold(n, ".justfile") {
					t.file, t.dir = path.Join(d, n), d
					break
				}
			}
			if (d == "." || d == "/" || escapes(d)) && t.file == "" {
				return nil, true
			}
		}
	}
	if wd != "" {
		d, ok := callPath(dir, self, wd)
		if !ok {
			return nil, true
		}
		t.dir = d
	}
	return t, true
}

// shellCall resolves `bash|sh|zsh [options] x` (source is false) or `source x` / `. x`. A shell
// with -c (an inline script, see inlineScript), -s or no script reads no file and is not a call;
// neither is a sourced file outside the repository, such as ~/.cargo/env.
func shellCall(args []shell.Word, dir, self string, source bool) (*call, bool) {
	i := 0
	for !source && i < len(args) {
		v := args[i].Value
		if v == "--" {
			i++
			break
		}
		if v == "--rcfile" || v == "--init-file" {
			i += 2
			continue
		}
		if strings.HasPrefix(v, "--") {
			i++
			continue
		}
		if len(v) < 2 || v[0] != '-' && v[0] != '+' {
			break
		}
		if strings.ContainsAny(v[1:], "cs") {
			return nil, false
		}
		if strings.HasSuffix(v, "o") || strings.HasSuffix(v, "O") {
			i++
		}
		i++
	}
	if i >= len(args) {
		return nil, false
	}
	p := args[i].Value
	if source && outsideRepo(p) {
		return nil, false
	}
	f, ok := callPath(dir, self, p)
	if !ok {
		return nil, true
	}
	return &call{kind: "script", file: f, dir: dir}, true
}

// runMode is how follow treats a recipe.
type runMode int

const (
	unreached    runMode = iota // only its release cargo builds count, RewriteOnly
	reached                     // everything counts
	maybeReached                // a Make rule whose target is a variable that does not resolve, or a prerequisite of one (literal or resolved): its pushes and unfollowable calls count, its builds stay RewriteOnly
)

// follow classifies the commands of the file t runs. Only reachable recipes (the invoked targets
// and, transitively, their prerequisites, or the default goal) produce gateable builds and
// pushes; ecosystem builds in other recipes come back RewriteOnly, with no notes. A Make rule
// whose target is a variable that does not resolve may run under any name: its pushes count,
// but its builds stay RewriteOnly, and the same holds for the rules of its literal or resolved
// prerequisites. Builds get Via set to the file they are in, and their notes are
// located there. The caller's depth is added to every command, so builds behind a conditional
// call count as conditional.
//
// What cannot be followed counts as a push: the depth limit, a file that cannot be read or is
// not a shell script (binary, or a #! naming another interpreter), a named goal or recipe
// without a rule, a prerequisite that is a variable it cannot resolve. A call already being
// followed (a cycle) adds nothing.
func (c *classifier) follow(t *call, caller shell.Command, ctx plan.Context, level int, stack []string) ([]plan.Build, bool) {
	key := t.key()
	if slices.Contains(stack, key) {
		return nil, false
	}
	if level > maxCallDepth {
		return nil, true
	}
	stack = append(slices.Clone(stack), key)
	src := c.read(t.file)
	if src == nil || !utf8.Valid(src) || bytes.IndexByte(src, 0) >= 0 {
		return nil, true
	}
	text := string(src)
	env := slices.Sorted(maps.Keys(ctx.Env))
	env = append(env, t.env...)

	var builds []plan.Build
	var vars map[string]string // Makefile variables, for programs written as $(NAME)
	self := ""                 // the script's directory, for $(dirname "$0"); "" in recipes
	push := false
	run := func(sp span, m runMode) {
		// The shell reads `\` before CR LF as an escaped CR; make, just and the shell in CI join
		// those lines. The replacement keeps every byte offset.
		script := strings.ReplaceAll(text[sp.start:sp.end], "\\\r\n", " \\\n")
		cmds, _ := shell.Parse(script, shell.State{Dir: t.dir, Env: env, Self: self})
		for _, cmd := range cmds {
			cmd = shift(cmd, sp.start)
			cmd.Depth += caller.Depth
			nctx := contextOf(cmd)
			if m == unreached {
				if b := c.recognize(cmd, nctx); b != nil {
					b.Via, b.RewriteOnly, b.Notes = t.file, true, nil
					builds = append(builds, *b)
				}
				continue
			}
			bs, p := c.nested(cmd, nctx, t.file, level, stack, vars)
			if m == maybeReached {
				for i := range bs {
					bs[i].RewriteOnly, bs[i].Notes = true, nil
				}
			}
			builds, push = append(builds, bs...), push || p
		}
	}

	switch t.kind {
	case "script":
		if !shellScript(text, false) {
			return nil, true
		}
		self = path.Dir(t.file)
		run(span{0, len(text)}, reached)
	case "make":
		mf := parseMakefile(text)
		env, vars = append(env, mf.exports...), mf.vars
		reach, maybe, unknown := mf.reachable(t.goals)
		push = unknown
		for _, rl := range mf.rules {
			m := unreached
			switch {
			case reach[rl]:
				m = reached
			case maybe[rl]:
				m = maybeReached
			}
			for _, sp := range rl.recipes {
				run(sp, m)
			}
		}
	case "just":
		jf := parseJustfile(text)
		env = append(env, jf.exports...)
		reach, unknown := jf.reachable(t.goals)
		push = unknown
		for _, rc := range jf.recipes {
			if rc.shebang != "" && !shellScript(rc.shebang, true) {
				push = push || reach[rc]
				continue
			}
			m := unreached
			if reach[rc] {
				m = reached
			}
			for _, sp := range rc.lines {
				run(sp, m)
			}
		}
	}
	return builds, push
}

// nested classifies a reachable command of the file at depth level (or of an inline script):
// a pushing image build counts as a push (other image builds in called files are not
// recognized), an ecosystem build gets Via = file and its notes located there, a file call is
// followed one level deeper, and a push, or an inline script that pushes, is a push.
func (c *classifier) nested(cmd shell.Command, ctx plan.Context, file string, level int, stack []string, vars map[string]string) ([]plan.Build, bool) {
	args := cmd.ArgWords()
	if ib := imageBuild(cmd, args, ctx, vars); ib != nil {
		return nil, ib.Image != nil && (ib.Image.Push || ib.Image.OutputPush)
	}
	if b := c.recognize(cmd, ctx); b != nil {
		b.Via = file
		for i := range b.Notes {
			if b.Notes[i].File == "" && file != "" {
				b.Notes[i].File, b.Notes[i].Line = file, c.line(file, cmd.Start)
			}
		}
		return []plan.Build{*b}, false
	}
	if n, isCall := callOf(c, cmd, ctx); isCall {
		if n == nil {
			return nil, true
		}
		return c.follow(n, cmd, ctx, level+1, stack)
	}
	return nil, isPush(args, vars) || c.inlinePush(cmd, args, level, stack, vars)
}

// shift moves a command parsed from a slice of a file to the file's offsets.
func shift(c shell.Command, by int) shell.Command {
	c.Start += by
	c.End += by
	c.Words = slices.Clone(c.Words)
	for i := range c.Words {
		c.Words[i].Start += by
	}
	c.Redirs = slices.Clone(c.Redirs)
	for i := range c.Redirs {
		c.Redirs[i].Start += by
		if c.Redirs[i].Target.Raw != "" {
			c.Redirs[i].Target.Start += by
		}
	}
	return c
}

// shellScript reports whether text is a shell script: a #! line naming a shell, or no #! line
// at all when one is not required.
func shellScript(text string, required bool) bool {
	first, _, _ := strings.Cut(text, "\n")
	first = strings.TrimSpace(strings.TrimPrefix(first, "\xef\xbb\xbf"))
	if !strings.HasPrefix(first, "#!") {
		return !required
	}
	fields := strings.Fields(first[2:])
	for len(fields) > 0 && path.Base(fields[0]) == "env" {
		fields = fields[1:]
		for len(fields) > 0 && (strings.HasPrefix(fields[0], "-") || strings.Contains(fields[0], "=")) {
			fields = fields[1:]
		}
	}
	return len(fields) > 0 && slices.Contains([]string{"sh", "bash", "dash", "zsh", "ksh", "ash", "mksh"}, path.Base(fields[0]))
}
