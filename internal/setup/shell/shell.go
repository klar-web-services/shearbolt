// Package shell splits shell text into commands, words and byte offsets. It is not a shell
// interpreter: it only tells commands apart and tracks the little state setup needs, which is the
// working directory, the variables set so far, and how deeply each command is nested.
//
// Compound commands are flattened. The words that open or continue one (if, then, elif, else,
// while, until, do, {, (, !) are not commands themselves; they only raise Depth. The words that
// close one (fi, done, esac, }, )) are one-word commands at the depth outside the construct. A
// for, select or case header is a command that starts with its keyword, and a function definition
// is a command whose words are `name ()` or `function name`. Case patterns are skipped.
package shell

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// Word is one shell word.
type Word struct {
	Raw   string // as written, quotes kept; always script[Start:Start+len(Raw)]
	Value string // quotes and backslash escapes removed, variables and substitutions not expanded
	Start int    // byte offset in the script
}

// State is the shell state that carries from one command to the next.
type State struct {
	Dir string   // effective directory, repo-relative ("" = unknown, for example after cd $X)
	Env []string // names set by `export X=…` or a bare `X=…`, in order
	// Self is the repo-relative directory of the script file being parsed, which
	// $(dirname "$0") and $(dirname "${BASH_SOURCE[0]}") name; "" when the text is not a script file.
	Self string
}

// scriptDir matches a leading $(dirname "$0") or $(dirname "${BASH_SOURCE[0]}"), quoted or not.
var scriptDir = regexp.MustCompile(`^\$\(dirname\s+(?:--\s+)?"?(?:\$0|\$\{0\}|\$\{BASH_SOURCE(?:\[0\])?\}|\$BASH_SOURCE)"?\s*\)`)

// ScriptDir resolves a path value that starts with the running script's directory
// ($(dirname "$0") or $(dirname "${BASH_SOURCE[0]}")) against self, the script's repo-relative
// directory. It reports false for any other value, when self is "", or when the rest holds a
// substitution or a glob or leaves the repository. In a sourced file $0 names the caller; it is
// taken as the sourced file here.
func ScriptDir(v, self string) (string, bool) {
	m := scriptDir.FindStringIndex(v)
	if self == "" || m == nil {
		return "", false
	}
	rest := v[m[1]:]
	if rest != "" && rest[0] != '/' || strings.ContainsAny(rest, "$`*?[") {
		return "", false
	}
	d := path.Join(self, rest)
	if d == ".." || strings.HasPrefix(d, "../") {
		return "", false
	}
	return d, true
}

// Redir is one redirection. For a heredoc, Target is the delimiter word.
type Redir struct {
	Op     string // as written, with any file descriptor: ">", ">>", "2>&", "&>", "<<", "<<-"
	Start  int    // byte offset of Op
	Target Word   // zero when missing
}

// Command is one simple command.
type Command struct {
	Words      []Word // leading VAR=x assignments included; redirections are in Redirs
	Redirs     []Redir
	Start, End int    // byte offsets in the script; End is just after the last word or redirection
	Op         string // operator after it: "&&", "||", ";", ";;", "|", "&", "\n", or "" at the end or before a closing ")"
	Before     State  // directory and variables in effect before the command
	// Depth is the nesting in if/for/while/until/select/case, {}, (), a function body or a negated
	// pipeline. Everything from a heredoc operator to the end of its body is one level deeper, and
	// so is everything from a construct that never closes to the end of the script. 0 = top level.
	Depth     int
	ContAfter bool // a `\`-newline lies between End and the next command (`a && \` + newline + `b`)
	Script    int  // set by the caller: which script text of the step
}

// Parse splits script into commands. start is the state before the first command; the returned
// state is the one after the last, so consecutive script items can be parsed in sequence.
//
// The directory follows a literal `cd`, `pushd` and `popd`. It becomes "" (unknown) after a `cd`
// to a variable, `-`, an absolute path or a path outside the repository; after a `cd` that runs
// only if the previous command failed (`||`); and after an if, loop, case or function body that
// changed it. A subshell, and a command in a pipeline or in the background, changes nothing.
// Heredoc bodies are parsed as commands too (one level deeper), so a push inside
// `bash <<EOF` is still seen; their state does not leak out.
//
// A quote, substitution or expression that never closes is read as its opening characters only,
// so the commands after it stay visible, and the command holding it and every command after it
// get Depth+1: the parser cannot be sure where they start, so they must not be edited.
func Parse(script string, start State) ([]Command, State) {
	return parse(script, start, 0)
}

func parse(src string, start State, base int) ([]Command, State) {
	toks, conts, unsafe := lex(src)
	p := &parser{src: src, toks: toks, base: base, state: State{Dir: start.Dir, Env: slices.Clone(start.Env), Self: start.Self}}
	p.run()
	p.contAfter(conts)
	if unsafe >= 0 {
		for i := range p.cmds {
			if p.cmds[i].End > unsafe {
				p.cmds[i].Depth++
			}
		}
	}
	return p.cmds, p.state
}

// Argv returns the command's word values without leading VAR=x assignments and without the
// wrappers that run the next word as the program, with their options: sudo, env, time, nohup,
// nice, exec, command (except `command -v`/`-V`, a lookup) and timeout (and its duration).
func (c Command) Argv() []string {
	args := c.ArgWords()
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	for i, w := range args {
		out[i] = w.Value
	}
	return out
}

// ArgWords is Argv as words, so callers can edit at their offsets.
func (c Command) ArgWords() []Word {
	_, args := c.split()
	return args
}

// Prefix returns the names assigned by leading VAR=x words, including those given to a wrapper
// such as sudo or env.
func (c Command) Prefix() []string {
	prefix, _ := c.split()
	return prefix
}

var (
	sudoArgs    = []string{"-u", "--user", "-g", "--group", "-C", "--close-from", "-D", "--chdir", "-h", "--host", "-p", "--prompt", "-r", "--role", "-t", "--type", "-T", "--command-timeout", "-U", "--other-user", "-R", "--chroot"}
	envArgs     = []string{"-u", "--unset", "-C", "--chdir", "-S", "--split-string"}
	niceArgs    = []string{"-n", "--adjustment"}
	execArgs    = []string{"-a"}
	timeoutArgs = []string{"-s", "--signal", "-k", "--kill-after"}
)

func (c Command) split() (prefix []string, args []Word) {
	w := c.Words
	i := 0
	for ; i < len(w); i++ {
		n, ok := assignName(w[i].Raw)
		if !ok {
			break
		}
		prefix = append(prefix, n)
	}
	for i < len(w) {
		switch w[i].Value {
		case "sudo":
			i = skipOptions(w, i+1, sudoArgs, &prefix)
		case "env":
			i = skipOptions(w, i+1, envArgs, &prefix)
		case "time", "nohup":
			i = skipOptions(w, i+1, nil, &prefix)
		case "nice":
			i = skipOptions(w, i+1, niceArgs, &prefix)
		case "exec":
			i = skipOptions(w, i+1, execArgs, &prefix)
		case "command":
			if i+1 < len(w) && (w[i+1].Value == "-v" || w[i+1].Value == "-V") {
				return prefix, w[i:] // a lookup, not a run
			}
			i = skipOptions(w, i+1, nil, &prefix)
		case "timeout":
			i = skipOptions(w, i+1, timeoutArgs, &prefix) + 1 // and the duration
		default:
			return prefix, w[i:]
		}
	}
	return prefix, nil
}

func skipOptions(w []Word, i int, withArg []string, prefix *[]string) int {
	opts := true
	for i < len(w) {
		v := w[i].Value
		if n, ok := assignName(w[i].Raw); ok {
			*prefix = append(*prefix, n)
			i++
			continue
		}
		if opts && v == "--" {
			opts = false
			i++
			continue
		}
		if opts && strings.HasPrefix(v, "-") {
			if slices.Contains(withArg, v) {
				i += 2
			} else {
				i++
			}
			continue
		}
		return i
	}
	return i
}

// assignName returns NAME for a word written NAME=…, NAME+=… or NAME[i]=….
func assignName(raw string) (string, bool) {
	i := 0
	for i < len(raw) && (raw[i] == '_' || isAlpha(raw[i]) || i > 0 && isDigit(raw[i])) {
		i++
	}
	if i == 0 {
		return "", false
	}
	name := raw[:i]
	if i < len(raw) && raw[i] == '[' {
		j := strings.IndexByte(raw[i:], ']')
		if j < 0 {
			return "", false
		}
		i += j + 1
	}
	if i < len(raw) && raw[i] == '+' {
		i++
	}
	if i < len(raw) && raw[i] == '=' {
		return name, true
	}
	return "", false
}

// identifier returns the variable name in an export/declare argument (NAME, NAME=…, NAME[i]=…).
func identifier(arg string) (string, bool) {
	if n, ok := assignName(arg); ok {
		return n, true
	}
	if arg == "" || isDigit(arg[0]) {
		return "", false
	}
	for i := 0; i < len(arg); i++ {
		if arg[i] != '_' && !isAlpha(arg[i]) && !isDigit(arg[i]) {
			return "", false
		}
	}
	return arg, true
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// ---- parser ----

type frameKind int

const (
	fIf frameKind = iota
	fLoop
	fCase
	fBrace
	fParen
)

type frame struct {
	kind     frameKind
	dir      string // directory when the construct opened
	body     bool   // a function body: defined here, not run
	patterns bool   // case: the next word starts a pattern
}

type parser struct {
	src       string
	toks      []token
	base      int
	state     State
	stack     []frame
	cmds      []Command
	own       []int // indexes of the commands parsed here, not from heredoc bodies
	cur       *Command
	curCase   bool // cur is a case header
	curCaseIn bool // ...and has read its `in`
	negate    bool // the current pipeline started with !
	heredoc   bool // a heredoc operator was seen on the current line
	prevOp    string
	funcHdr   bool     // the last finished command was a function header
	dirs      []string // pushd stack
}

func (p *parser) run() {
	for i := 0; i < len(p.toks); i++ {
		t := p.toks[i]
		patternStart := t.kind == tWord && t.word.Raw != "esac" || t.kind == tOp && t.op == "("
		if p.cur != nil && p.curCaseIn && patternStart {
			p.finish("")
		}
		if f := p.top(); p.cur == nil && patternStart && f != nil && f.kind == fCase && f.patterns {
			i = p.skipPattern(i)
			f.patterns = false
			continue
		}
		switch t.kind {
		case tWord:
			p.word(t)
		case tRedir:
			p.redir(t)
		case tOp:
			i = p.op(i)
		}
	}
	if p.cur != nil {
		p.finish("")
	}
}

// skipPattern skips a case pattern, `(a|b)` or `a|b)`, and returns the index of its `)`.
func (p *parser) skipPattern(i int) int {
	for ; i < len(p.toks); i++ {
		if t := p.toks[i]; t.kind == tOp && t.op == ")" {
			return i
		}
	}
	return len(p.toks) - 1
}

func (p *parser) word(t token) {
	w := t.word
	if p.cur != nil {
		if w.Raw == "{" && isFuncHeader(p.cur.Words) {
			p.finish("")
			p.push(fBrace, true)
			return
		}
		p.add(w)
		if p.curCase && len(p.cur.Words) == 3 && w.Raw == "in" {
			p.curCaseIn = true
			if f := p.top(); f != nil && f.kind == fCase {
				f.patterns = true
			}
		}
		return
	}
	switch w.Raw {
	case "if":
		p.push(fIf, false)
		return
	case "while", "until":
		p.push(fLoop, false)
		return
	case "then", "else", "elif", "do":
		return
	case "{":
		p.push(fBrace, p.funcHdr)
		return
	case "!":
		p.negate = true
		return
	case "fi":
		p.pop(fIf)
	case "done":
		p.pop(fLoop)
	case "esac":
		p.pop(fCase)
	case "}":
		p.pop(fBrace)
	case "for", "select":
		p.push(fLoop, false)
	case "case":
		p.push(fCase, false)
		p.begin(t.start)
		p.curCase = true
		p.add(w)
		return
	}
	p.begin(t.start)
	p.add(w)
}

func (p *parser) redir(t token) {
	if p.cur == nil {
		p.begin(t.start)
	}
	p.cur.Redirs = append(p.cur.Redirs, Redir{Op: t.op, Start: t.start, Target: t.word})
	p.cur.End = t.end
	if op := strings.TrimLeft(t.op, "0123456789"); op == "<<" || op == "<<-" {
		if !p.heredoc {
			p.cur.Depth++
		}
		p.heredoc = true
	}
}

func (p *parser) op(i int) int {
	t := p.toks[i]
	switch t.op {
	case "(":
		if p.cur != nil && isFuncName(p.cur.Words) && i+1 < len(p.toks) && p.toks[i+1].kind == tOp && p.toks[i+1].op == ")" {
			end := p.toks[i+1].end
			p.add(Word{Raw: p.src[t.start:end], Value: "()", Start: t.start})
			return i + 1
		}
		if p.cur != nil {
			p.finish("")
		}
		p.push(fParen, p.funcHdr)
	case ")":
		if p.cur != nil {
			p.finish("")
		}
		p.pop(fParen)
		p.begin(t.start)
		p.add(Word{Raw: ")", Value: ")", Start: t.start})
	default:
		op := t.op
		switch op {
		case "|&":
			op = "|"
		case ";&", ";;&":
			op = ";;"
		}
		if p.cur != nil {
			p.finish(op)
		}
		if op != "|" {
			p.negate = false
		}
		if f := p.top(); op == ";;" && f != nil && f.kind == fCase {
			f.patterns = true
		}
		if op == "\n" {
			p.heredoc = false
			for _, b := range t.heredocs {
				p.body(b)
			}
		}
	}
	return i
}

func (p *parser) begin(start int) {
	d := p.base + len(p.stack)
	if p.negate {
		d++
	}
	if p.heredoc {
		d++
	}
	p.cur = &Command{Start: start, End: start, Depth: d, Before: State{Dir: p.state.Dir, Env: slices.Clone(p.state.Env), Self: p.state.Self}}
	p.curCase, p.curCaseIn = false, false
	p.funcHdr = false
}

func (p *parser) add(w Word) {
	p.cur.Words = append(p.cur.Words, w)
	p.cur.End = w.Start + len(w.Raw)
}

func (p *parser) finish(op string) {
	c := p.cur
	p.cur = nil
	c.Op = op
	p.apply(c)
	p.prevOp = op
	p.funcHdr = isFuncHeader(c.Words)
	p.own = append(p.own, len(p.cmds))
	p.cmds = append(p.cmds, *c)
}

func (p *parser) top() *frame {
	if len(p.stack) == 0 {
		return nil
	}
	return &p.stack[len(p.stack)-1]
}

func (p *parser) push(k frameKind, body bool) {
	p.stack = append(p.stack, frame{kind: k, dir: p.state.Dir, body: body})
	p.funcHdr = false
}

// pop closes the innermost construct if it is of kind k. A stray closer changes nothing, so a
// script the parser does not understand never looks less nested than it is.
func (p *parser) pop(k frameKind) {
	if len(p.stack) == 0 || p.stack[len(p.stack)-1].kind != k {
		return
	}
	f := p.stack[len(p.stack)-1]
	p.stack = p.stack[:len(p.stack)-1]
	switch {
	case k == fParen:
		p.state.Dir = f.dir
	case k == fBrace && !f.body:
	default:
		if p.state.Dir != f.dir {
			p.state.Dir = ""
		}
	}
}

// body parses a heredoc body one level deeper, starting from the current state, and discards
// the state it ends with.
func (p *parser) body(b [2]int) {
	sub, _ := parse(p.src[b[0]:b[1]], p.state, p.base+len(p.stack)+1)
	for _, c := range sub {
		c.Start += b[0]
		c.End += b[0]
		for k := range c.Words {
			c.Words[k].Start += b[0]
		}
		for k := range c.Redirs {
			c.Redirs[k].Start += b[0]
			if c.Redirs[k].Target.Raw != "" {
				c.Redirs[k].Target.Start += b[0]
			}
		}
		p.cmds = append(p.cmds, c)
	}
}

// apply updates the state after c.
func (p *parser) apply(c *Command) {
	if c.Op == "|" || c.Op == "&" || p.prevOp == "|" {
		return // a subshell
	}
	args := c.ArgWords()
	if len(args) == 0 {
		for _, w := range c.Words {
			if _, ok := assignName(w.Raw); !ok {
				return
			}
		}
		for _, w := range c.Words {
			n, _ := assignName(w.Raw)
			p.setVar(n)
		}
		return
	}
	conditional := p.prevOp == "||"
	switch args[0].Value {
	case "cd":
		p.chdir(chdir(p.state.Dir, p.state.Self, args[1:]), conditional)
	case "pushd":
		dir := chdir(p.state.Dir, p.state.Self, args[1:])
		p.dirs = append(p.dirs, p.state.Dir)
		p.chdir(dir, conditional)
	case "popd":
		dir := ""
		if len(args) == 1 && len(p.dirs) > 0 {
			dir = p.dirs[len(p.dirs)-1]
			p.dirs = p.dirs[:len(p.dirs)-1]
		}
		p.chdir(dir, conditional)
	case "export", "declare", "typeset", "readonly", "local":
		for _, a := range args[1:] {
			if strings.HasPrefix(a.Value, "-") {
				continue
			}
			name, _, _ := strings.Cut(a.Value, "=")
			if n, ok := identifier(name); ok {
				p.setVar(n)
			}
		}
	}
}

func (p *parser) setVar(n string) {
	if !slices.Contains(p.state.Env, n) {
		p.state.Env = append(p.state.Env, n)
	}
}

func (p *parser) chdir(dir string, conditional bool) {
	if conditional && dir != p.state.Dir {
		dir = ""
	}
	p.state.Dir = dir
}

// chdir returns the directory after `cd args` from cur, or "" when it is not certain. A path that
// starts with the script's own directory resolves against self (see ScriptDir), even when cur is
// unknown.
func chdir(cur, self string, args []Word) string {
	for len(args) > 0 && strings.HasPrefix(args[0].Value, "-") && args[0].Value != "-" {
		v := args[0].Value
		args = args[1:]
		if v == "--" {
			break
		}
	}
	if len(args) == 1 {
		if d, ok := ScriptDir(args[0].Value, self); ok {
			return d
		}
	}
	if len(args) != 1 || cur == "" {
		return ""
	}
	w := args[0]
	v := w.Value
	if strings.ContainsAny(w.Raw, "$`*?[") || v == "" || v == "-" || strings.HasPrefix(v, "~") || strings.HasPrefix(v, "/") || strings.HasPrefix(v, "+") {
		return ""
	}
	d := path.Join(cur, v)
	if d == ".." || strings.HasPrefix(d, "../") {
		return ""
	}
	return d
}

func isFuncName(words []Word) bool {
	name := ""
	switch {
	case len(words) == 1:
		name = words[0].Raw
	case len(words) == 2 && words[0].Raw == "function":
		name = words[1].Raw
	default:
		return false
	}
	return name != "" && !strings.ContainsAny(name, "$'\"`[=\\")
}

// isFuncHeader reports a function header: `name ()`, `function name` or `function name ()`. The
// "()" word is the only one that can start with "(", because "(" is an operator.
func isFuncHeader(words []Word) bool {
	paren := func(w Word) bool { return strings.HasPrefix(w.Raw, "(") }
	switch {
	case len(words) == 2 && paren(words[1]):
		return true
	case len(words) >= 2 && words[0].Raw == "function":
		return len(words) == 2 || len(words) == 3 && paren(words[2])
	}
	return false
}

func (p *parser) contAfter(conts []int) {
	for k, idx := range p.own {
		next := len(p.src)
		if k+1 < len(p.own) {
			next = p.cmds[p.own[k+1]].Start
		}
		end := p.cmds[idx].End
		for _, c := range conts {
			if c >= end && c < next {
				p.cmds[idx].ContAfter = true
				break
			}
		}
	}
}

// ---- lexer ----

type tokKind int

const (
	tWord tokKind = iota
	tOp
	tRedir
)

type token struct {
	kind       tokKind
	op         string // tOp and tRedir
	start, end int
	word       Word     // tWord, or the target of a tRedir
	heredocs   [][2]int // on a "\n": the heredoc bodies that follow it, [start, end)
}

type heredoc struct {
	delim string
	strip bool // <<- strips leading tabs
}

type lexer struct {
	src     string
	i       int
	toks    []token
	conts   []int // offsets of `\`-newlines between tokens
	pending []heredoc
	unsafe  int // offset of the first construct that does not close, or -1
}

var redirOps = []string{"&>>", "&>", "<<<", "<<-", "<<", "<>", "<&", ">>", ">|", ">&", "<", ">"}

// lex splits src into tokens. It also returns the offsets of the `\`-newlines between tokens and
// the offset of the first quote, substitution or expression that never closes (-1 if none). Such
// a construct is read as its opening characters only, so the rest of the script stays visible.
func lex(src string) ([]token, []int, int) {
	l := &lexer{src: src, unsafe: -1}
	for l.i < len(src) {
		c := src[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case c == '\\' && l.at(l.i+1) == '\n':
			l.conts = append(l.conts, l.i)
			l.i += 2
		case c == '#':
			for l.i < len(src) && src[l.i] != '\n' {
				l.i++
			}
		case c == '\n':
			t := token{kind: tOp, op: "\n", start: l.i, end: l.i + 1}
			l.i++
			t.heredocs, l.i, _ = heredocBodies(src, l.i, l.pending)
			l.pending = nil
			l.toks = append(l.toks, t)
		case c == '&' && l.at(l.i+1) == '>':
			l.redir(l.i, l.i)
		case c == '&' || c == '|' || c == ';' || c == '(' || c == ')':
			l.operator()
		case (c == '<' || c == '>') && l.at(l.i+1) != '(':
			l.redir(l.i, l.i)
		case isDigit(c):
			j := l.i
			for j < len(src) && isDigit(src[j]) {
				j++
			}
			if j < len(src) && (src[j] == '<' || src[j] == '>') && l.at(j+1) != '(' {
				l.redir(l.i, j)
			} else {
				l.addWord()
			}
		default:
			l.addWord()
		}
	}
	return l.toks, l.conts, l.unsafe
}

func (l *lexer) at(i int) byte {
	if i < len(l.src) {
		return l.src[i]
	}
	return 0
}

func (l *lexer) markUnsafe(pos int) {
	if l.unsafe < 0 || pos < l.unsafe {
		l.unsafe = pos
	}
}

func (l *lexer) operator() {
	s := l.src[l.i:]
	op := s[:1]
	for _, o := range []string{";;&", "&&", "||", ";;", ";&", "|&"} {
		if strings.HasPrefix(s, o) {
			op = o
			break
		}
	}
	l.toks = append(l.toks, token{kind: tOp, op: op, start: l.i, end: l.i + len(op)})
	l.i += len(op)
}

// redir lexes a redirection whose operator starts at at (start < at when a file descriptor
// number precedes it) and its target word.
func (l *lexer) redir(start, at int) {
	var op string
	for _, o := range redirOps {
		if strings.HasPrefix(l.src[at:], o) {
			op = o
			break
		}
	}
	end := at + len(op)
	t := token{kind: tRedir, op: l.src[start:end], start: start, end: end}
	l.i = end
	for l.i < len(l.src) {
		if c := l.src[l.i]; c == ' ' || c == '\t' || c == '\r' {
			l.i++
		} else if c == '\\' && l.at(l.i+1) == '\n' {
			l.conts = append(l.conts, l.i)
			l.i += 2
		} else {
			break
		}
	}
	if l.i < len(l.src) && !strings.ContainsRune("\n;&|()<>#", rune(l.src[l.i])) {
		t.word = l.word()
		t.end = t.word.Start + len(t.word.Raw)
	}
	if (op == "<<" || op == "<<-") && t.word.Raw != "" {
		l.pending = append(l.pending, heredoc{delim: t.word.Value, strip: op == "<<-"})
	}
	l.toks = append(l.toks, t)
}

// heredocBodies reads the bodies of the heredocs hs, which start at i (just after the newline
// that ended their line). It returns the bodies as [start, end) ranges, the offset after the last
// terminator line, and whether every terminator was found. A body without its terminator runs to
// the end of the script, as in bash.
func heredocBodies(src string, i int, hs []heredoc) ([][2]int, int, bool) {
	var out [][2]int
	closed := true
	for _, h := range hs {
		start, end, found := i, len(src), false
		for i < len(src) {
			lineStart, lineEnd, next := i, len(src), len(src)
			if nl := strings.IndexByte(src[i:], '\n'); nl >= 0 {
				lineEnd, next = i+nl, i+nl+1
			}
			line := strings.TrimSuffix(src[lineStart:lineEnd], "\r")
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			i = next
			if line == h.delim {
				end, found = lineStart, true
				break
			}
		}
		closed = closed && found
		out = append(out, [2]int{start, end})
	}
	return out, i, closed
}

func (l *lexer) addWord() {
	w := l.word()
	l.toks = append(l.toks, token{kind: tWord, start: w.Start, end: w.Start + len(w.Raw), word: w})
}

// word lexes one word starting at l.i. `\`-newlines at the end of the word are not part of it;
// they are recorded as continuations between tokens.
func (l *lexer) word() Word {
	src := l.src
	start, trail := l.i, -1
	var v strings.Builder
	done := func() Word {
		end := l.i
		if trail >= 0 {
			for c := trail; c < l.i; c += 2 {
				l.conts = append(l.conts, c)
			}
			end = trail
		}
		return Word{Raw: src[start:end], Value: v.String(), Start: start}
	}
	// group adds the construct src[l.i:j] to the word, or, when it does not close, only its first
	// n characters, and marks the rest of the script unsafe.
	group := func(j int, ok bool, n int, value string) {
		if !ok {
			l.markUnsafe(l.i)
			v.WriteString(src[l.i : l.i+n])
			l.i += n
			return
		}
		v.WriteString(value)
		l.i = j
	}
	for l.i < len(src) {
		c := src[l.i]
		if c == '\\' && l.at(l.i+1) == '\n' {
			if trail < 0 {
				trail = l.i
			}
			l.i += 2
			continue
		}
		if strings.IndexByte(" \t\r\n;&|)", c) >= 0 || (c == '<' || c == '>') && l.at(l.i+1) != '(' || c == '(' && !l.parenInWord(start) {
			return done()
		}
		trail = -1
		switch {
		case c == '<' || c == '>':
			j, ok := skipParens(src, l.i+1) // process substitution
			group(j, ok, 2, src[l.i:j])
		case c == '(':
			j, ok := skipParens(src, l.i)
			group(j, ok, 1, src[l.i:j])
		case c == '\\':
			if l.i+1 >= len(src) {
				v.WriteByte('\\')
				l.i++
				continue
			}
			v.WriteByte(src[l.i+1])
			l.i += 2
		case c == '\'':
			j, ok := skipSingle(src, l.i)
			group(j, ok, 1, src[l.i+1:max(j-1, l.i+1)])
		case c == '"':
			val, j, ok := dquote(src, l.i)
			group(j, ok, 1, val)
		case c == '$' && l.at(l.i+1) == '\'':
			j, ok := skipANSI(src, l.i+1)
			group(j, ok, 2, src[l.i+2:max(j-1, l.i+2)])
		case c == '$' && l.at(l.i+1) == '"':
			val, j, ok := dquote(src, l.i+1)
			group(j, ok, 2, val)
		case c == '$':
			j, ok := skipDollar(src, l.i)
			group(j, ok, dollarOpen(src, l.i), src[l.i:j])
		case c == '`':
			j, ok := skipBacktick(src, l.i)
			group(j, ok, 1, src[l.i:j])
		default:
			v.WriteByte(c)
			l.i++
		}
	}
	return done()
}

// parenInWord reports a "(" that belongs to the word: an array assignment `a=(…)` or an extglob
// `@(…)`, `!(…)`, `+(…)`, `*(…)`, `?(…)`.
func (l *lexer) parenInWord(start int) bool {
	so := l.src[start:l.i]
	if n, ok := assignName(so); ok && (so == n+"=" || so == n+"+=" || strings.HasSuffix(so, "]=") || strings.HasSuffix(so, "]+=")) {
		return true
	}
	return l.i > start && strings.IndexByte("@!+*?", l.src[l.i-1]) >= 0
}

// The skip functions below return the offset after a construct and whether it closed. One that
// does not close runs to the end of the script.

// dquote returns the value of the double-quoted string at src[i] and the offset after it.
func dquote(src string, i int) (string, int, bool) {
	var v strings.Builder
	j := i + 1
	for j < len(src) {
		switch c := src[j]; c {
		case '"':
			return v.String(), j + 1, true
		case '\\':
			if j+1 >= len(src) {
				return v.String(), len(src), false
			}
			switch n := src[j+1]; n {
			case '$', '`', '"', '\\':
				v.WriteByte(n)
			case '\n':
			default:
				v.WriteByte('\\')
				v.WriteByte(n)
			}
			j += 2
		case '$', '`':
			var k int
			var ok bool
			if c == '$' {
				k, ok = skipDollar(src, j)
			} else {
				k, ok = skipBacktick(src, j)
			}
			if !ok {
				return v.String(), len(src), false
			}
			v.WriteString(src[j:k])
			j = k
		default:
			v.WriteByte(c)
			j++
		}
	}
	return v.String(), len(src), false
}

// skipDollar skips the expansion that starts with the "$" at src[i]: ${{ … }}, $(…), $((…)),
// ${…}, or just the "$".
func skipDollar(src string, i int) (int, bool) {
	switch {
	case strings.HasPrefix(src[i:], "${{"):
		return skipExpr(src, i)
	case strings.HasPrefix(src[i:], "$("):
		return skipParens(src, i+1)
	case strings.HasPrefix(src[i:], "${"):
		return skipBraces(src, i+1)
	}
	return i + 1, true
}

// dollarOpen returns the length of the opening of the expansion at src[i].
func dollarOpen(src string, i int) int {
	switch {
	case strings.HasPrefix(src[i:], "${{"):
		return 3
	case strings.HasPrefix(src[i:], "$("), strings.HasPrefix(src[i:], "${"):
		return 2
	}
	return 1
}

// skipExpr skips a GitHub expression ${{ … }} at src[i]. GitHub substitutes it before the shell
// runs, so it is opaque: spaces, quotes and operators inside split nothing. Its string literals
// are single-quoted (two single quotes stand for one) and may contain "}}".
func skipExpr(src string, i int) (int, bool) {
	for j := i + 3; j < len(src); {
		switch {
		case src[j] == '\'':
			j++
			for j < len(src) {
				if src[j] == '\'' {
					if j+1 < len(src) && src[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			j++
		case strings.HasPrefix(src[j:], "}}"):
			return j + 2, true
		default:
			j++
		}
	}
	return len(src), false
}

// skipParens skips a parenthesized group at src[i], with the quoting, comment and heredoc rules
// of the script inside it.
func skipParens(src string, i int) (int, bool) {
	depth := 0
	var pending []heredoc
	for j := i; j < len(src); {
		var ok = true
		switch c := src[j]; {
		case c == '(':
			depth++
			j++
		case c == ')':
			depth--
			j++
			if depth == 0 {
				return j, true
			}
		case c == '\\':
			j += 2
		case c == '\n':
			j++
			if len(pending) > 0 {
				_, j, ok = heredocBodies(src, j, pending)
				pending = nil
			}
		case strings.HasPrefix(src[j:], "<<<"):
			j += 3
		case strings.HasPrefix(src[j:], "<<"):
			j += 2
			strip := j < len(src) && src[j] == '-'
			if strip {
				j++
			}
			var delim string
			delim, j = readDelim(src, j)
			if delim != "" {
				pending = append(pending, heredoc{delim: delim, strip: strip})
			}
		case c == '\'':
			j, ok = skipSingle(src, j)
		case c == '"':
			_, j, ok = dquote(src, j)
		case c == '`':
			j, ok = skipBacktick(src, j)
		case c == '$':
			j, ok = skipDollar(src, j)
		case c == '#' && j > 0 && strings.IndexByte(" \t\n;&|(", src[j-1]) >= 0:
			for j < len(src) && src[j] != '\n' {
				j++
			}
		default:
			j++
		}
		if !ok {
			return len(src), false
		}
	}
	return len(src), false
}

// readDelim reads a heredoc delimiter word at src[i] (after blanks) and returns it without
// quotes or backslashes, and the offset after it.
func readDelim(src string, i int) (string, int) {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	var v strings.Builder
	for i < len(src) && !strings.ContainsRune(" \t\n;&|()<>", rune(src[i])) {
		switch c := src[i]; c {
		case '\'', '"':
			end := strings.IndexByte(src[i+1:], c)
			if end < 0 {
				return v.String(), len(src)
			}
			v.WriteString(src[i+1 : i+1+end])
			i += end + 2
		case '\\':
			if i+1 < len(src) {
				v.WriteByte(src[i+1])
			}
			i += 2
		default:
			v.WriteByte(c)
			i++
		}
	}
	return v.String(), min(i, len(src))
}

// skipBraces skips the ${…} body whose "{" is at src[i].
func skipBraces(src string, i int) (int, bool) {
	depth := 0
	for j := i; j < len(src); {
		var ok = true
		switch src[j] {
		case '{':
			depth++
			j++
		case '}':
			depth--
			j++
			if depth == 0 {
				return j, true
			}
		case '\\':
			j += 2
		case '\'':
			j, ok = skipSingle(src, j)
		case '"':
			_, j, ok = dquote(src, j)
		case '`':
			j, ok = skipBacktick(src, j)
		case '$':
			j, ok = skipDollar(src, j)
		default:
			j++
		}
		if !ok {
			return len(src), false
		}
	}
	return len(src), false
}

// skipSingle skips a single-quoted string at src[i]. A GitHub expression inside it is opaque too,
// because GitHub replaces it before the shell sees the quotes.
func skipSingle(src string, i int) (int, bool) {
	for j := i + 1; j < len(src); {
		if strings.HasPrefix(src[j:], "${{") {
			k, ok := skipExpr(src, j)
			if !ok {
				return len(src), false
			}
			j = k
			continue
		}
		if src[j] == '\'' {
			return j + 1, true
		}
		j++
	}
	return len(src), false
}

// skipANSI skips the body of $'…' whose quote is at src[i]; backslash escapes a quote.
func skipANSI(src string, i int) (int, bool) {
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '\'':
			return j + 1, true
		}
	}
	return len(src), false
}

func skipBacktick(src string, i int) (int, bool) {
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '`':
			return j + 1, true
		}
	}
	return len(src), false
}
