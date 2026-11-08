package setup

import (
	"regexp"
	"slices"
	"strings"
)

// This file reads Makefiles and justfiles just far enough to know which recipe lines a make or
// just invocation runs. It is not make or just: variables are not expanded.

// span is a byte range of shell text in a called file.
type span struct{ start, end int }

// lines splits text into lines; each span excludes its line ending.
func lines(text string) []span {
	var out []span
	for start := 0; start < len(text); {
		end := strings.IndexByte(text[start:], '\n')
		next := 0
		if end < 0 {
			end, next = len(text), len(text)
		} else {
			end, next = start+end, start+end+1
		}
		e := end
		if e > start && text[e-1] == '\r' {
			e--
		}
		out = append(out, span{start, e})
		start = next
	}
	return out
}

// continued reports a line that ends in an odd number of backslashes.
func continued(line string) bool {
	n := len(line) - len(strings.TrimRight(line, `\`))
	return n%2 == 1
}

// ---- Makefiles ----

type makeRule struct {
	targets, prereqs []string
	recipes          []span
}

type makefile struct {
	rules       []*makeRule
	defaultGoal string
	exports     []string
	vars        map[string]string // variables whose value holds no reference (CARGO ?= cargo)
	includes    bool              // include lines: rules may come from files not read here
}

// assign records `name op value` in vars when the value holds no reference; a reference, or a
// shell assignment (!=), makes the variable unknown.
func (mf *makefile) assign(lhs, value string) string {
	lhs = strings.TrimSpace(lhs)
	name := strings.TrimRight(lhs, " :?+!")
	op := strings.TrimSpace(lhs[len(name):])
	v := strings.TrimSpace(value)
	plain := !strings.ContainsAny(v, "$`")
	switch old, set := mf.vars[name]; {
	case op == "?" && set:
	case plain && op == "+":
		mf.vars[name] = strings.TrimSpace(old + " " + v)
	case plain && op != "!":
		mf.vars[name] = v
	default:
		delete(mf.vars, name)
	}
	return name
}

var makeRef = regexp.MustCompile(`^\$[({]([A-Za-z_][A-Za-z0-9_]*)[)}]$`)

// expand resolves a prerequisite that is one variable reference to the variable's words.
func (mf *makefile) expand(p string) ([]string, bool) {
	m := makeRef.FindStringSubmatch(p)
	if m == nil {
		return nil, false
	}
	v, ok := mf.vars[m[1]]
	return strings.Fields(v), ok
}

// parseMakefile reads rules (targets, prerequisites before any "|", and recipe lines with their
// `\` continuations and leading @, - and + removed), .DEFAULT_GOAL, and exported variables. It
// is not make: variables are not expanded, and conditionals are ignored (both branches count).
// Each recipe line runs in its own shell; .ONESHELL is not supported.
func parseMakefile(text string) *makefile {
	mf := &makefile{vars: map[string]string{}}
	ls := lines(text)
	var cur *makeRule
	var assigned []string
	exportAll := false
	for i := 0; i < len(ls); i++ {
		raw := text[ls[i].start:ls[i].end]
		j := i
		for j+1 < len(ls) && continued(text[ls[j].start:ls[j].end]) {
			j++
		}
		if strings.HasPrefix(raw, "\t") {
			if cur != nil {
				start := ls[i].start + 1
				for start < ls[j].end && strings.IndexByte("@-+ \t", text[start]) >= 0 {
					start++
				}
				if start < ls[j].end {
					cur.recipes = append(cur.recipes, span{start, ls[j].end})
				}
			}
			i = j
			continue
		}
		logical := raw
		for k := i + 1; k <= j; k++ {
			logical = strings.TrimSuffix(logical, `\`) + " " + text[ls[k].start:ls[k].end]
		}
		single := i == j
		i = j
		line := strings.TrimSpace(stripMakeComment(logical))
		if line == "" {
			continue
		}
		first, rest, _ := strings.Cut(line, " ")
		switch first {
		case "define":
			for i+1 < len(ls) {
				if f := strings.Fields(text[ls[i+1].start:ls[i+1].end]); len(f) > 0 && f[0] == "endef" {
					break
				}
				i++
			}
			i++
			cur = nil
			continue
		case "ifeq", "ifneq", "ifdef", "ifndef", "else", "endif":
			continue
		case "include", "-include", "sinclude":
			mf.includes = true
			cur = nil
			continue
		case "vpath", "unexport":
			cur = nil
			continue
		case "export":
			cur = nil
			rest = strings.TrimSpace(rest)
			if rest == "" {
				exportAll = true
			} else if k := strings.IndexByte(rest, '='); k >= 0 {
				mf.exports = append(mf.exports, mf.assign(rest[:k], rest[k+1:]))
			} else {
				mf.exports = append(mf.exports, strings.Fields(rest)...)
			}
			continue
		case "override":
			line = strings.TrimSpace(rest)
		}
		colon, isAssign := -1, false
		for k := 0; k < len(line); k++ {
			if line[k] == '=' {
				isAssign = true
				break
			}
			if line[k] == ':' {
				if strings.HasPrefix(line[k:], ":=") || strings.HasPrefix(line[k:], "::=") || strings.HasPrefix(line[k:], ":::=") {
					isAssign = true
				} else {
					colon = k
				}
				break
			}
		}
		if isAssign {
			cur = nil
			lhs, value, _ := strings.Cut(line, "=")
			name := mf.assign(lhs, value)
			if name == ".DEFAULT_GOAL" {
				if f := strings.Fields(value); len(f) > 0 {
					mf.defaultGoal = f[0]
				}
			}
			assigned = append(assigned, name)
			continue
		}
		if colon < 0 {
			cur = nil
			continue
		}
		after := strings.TrimPrefix(line[colon+1:], ":")
		prereqs, recipe, hasRecipe := strings.Cut(after, ";")
		if strings.Contains(prereqs, "=") {
			cur = nil // a target-specific variable
			continue
		}
		if k := strings.LastIndexByte(prereqs, ':'); k >= 0 {
			prereqs = prereqs[k+1:] // a static pattern rule
		}
		prereqs, _, _ = strings.Cut(prereqs, "|")
		rl := &makeRule{}
		for _, tg := range strings.Fields(strings.ReplaceAll(line[:colon], "&", " ")) {
			rl.targets = append(rl.targets, tg)
			if tg == ".EXPORT_ALL_VARIABLES" {
				exportAll = true
			}
		}
		for _, p := range strings.Fields(prereqs) {
			if !strings.Contains(p, "%") {
				rl.prereqs = append(rl.prereqs, p)
			}
		}
		if hasRecipe && single && strings.TrimSpace(recipe) != "" {
			if k := strings.IndexByte(raw, ';'); k >= 0 {
				start := ls[i].start + k + 1
				for start < ls[i].end && strings.IndexByte("@-+ \t", text[start]) >= 0 {
					start++
				}
				rl.recipes = append(rl.recipes, span{start, ls[i].end})
			}
		}
		mf.rules = append(mf.rules, rl)
		cur = rl
	}
	if exportAll {
		mf.exports = append(mf.exports, assigned...)
	}
	return mf
}

// stripMakeComment removes a # comment that is not escaped.
func stripMakeComment(s string) string {
	for k := 0; k < len(s); k++ {
		if s[k] == '\\' {
			k++
			continue
		}
		if s[k] == '#' {
			return s[:k]
		}
	}
	return s
}

// reachable returns the rules that run for goals (the default goal when there are none): their
// own rules and, transitively, their prerequisites'. A target or prerequisite that is one
// variable reference resolves through vars, so `release: $(BIN)` reaches the `$(BIN):` rule.
//
// maybe holds the rules that are not reached but may run: those with a target that does not
// resolve (they may run under any name) and, transitively, the rules of their prerequisites,
// literal or resolved through vars. Their prerequisites that do not resolve are skipped, not
// counted as unknown, so a `$(BIN): $(SRCS)` rule elsewhere does not make every make invocation
// a push.
//
// unknown reports what cannot be seen and counts as a push: a named goal without a rule (it may
// come from an include, or be a pattern), a variable prerequisite that does not resolve, or no
// default goal in a Makefile with includes.
func (mf *makefile) reachable(goals []string) (reach, maybe map[*makeRule]bool, unknown bool) {
	names := map[string][]*makeRule{}
	unresolved := map[*makeRule]bool{}
	for _, rl := range mf.rules {
		for _, tg := range rl.targets {
			if !strings.Contains(tg, "$") {
				names[tg] = append(names[tg], rl)
			} else if words, ok := mf.expand(tg); ok {
				for _, w := range words {
					names[w] = append(names[w], rl)
				}
			} else {
				unresolved[rl] = true
			}
		}
	}
	reach = map[*makeRule]bool{}
	var queue []string
	visit := func(rl *makeRule) {
		if !reach[rl] {
			reach[rl] = true
			queue = append(queue, rl.prereqs...)
		}
	}
	named := slices.Clone(goals)
	if len(named) == 0 {
		switch rl := mf.firstRule(); {
		case mf.defaultGoal != "":
			named = []string{mf.defaultGoal}
		case rl != nil:
			visit(rl)
		default:
			unknown = mf.includes
		}
	}
	isNamed := map[string]bool{}
	for _, g := range named {
		isNamed[g] = true
	}
	queue = append(queue, named...)
	seen := map[string]bool{}
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		if strings.Contains(g, "$") {
			words, ok := mf.expand(g)
			unknown = unknown || !ok
			queue = append(queue, words...)
			continue
		}
		if seen[g] {
			continue
		}
		seen[g] = true
		for _, rl := range names[g] {
			visit(rl)
		}
		if len(names[g]) == 0 && isNamed[g] {
			unknown = true
		}
	}
	// A maybe rule may run, and so may its prerequisites' rules, transitively. A $ prerequisite
	// that resolves is followed; one that does not is skipped, not counted as unknown.
	maybe = map[*makeRule]bool{}
	var pending []*makeRule
	mark := func(rl *makeRule) {
		if !reach[rl] && !maybe[rl] {
			maybe[rl] = true
			pending = append(pending, rl)
		}
	}
	for _, rl := range mf.rules {
		if unresolved[rl] {
			mark(rl)
		}
	}
	for len(pending) > 0 {
		rl := pending[0]
		pending = pending[1:]
		for _, p := range rl.prereqs {
			words := []string{p}
			if strings.Contains(p, "$") {
				var ok bool
				if words, ok = mf.expand(p); !ok {
					continue
				}
			}
			for _, w := range words {
				for _, dep := range names[w] {
					mark(dep)
				}
			}
		}
	}
	return reach, maybe, unknown
}

// firstRule returns the rule of make's default goal: the first whose target does not start with
// "." (unless it holds a "/") and is not a pattern. A target that does not resolve counts, since
// the rule is known even if its name is not.
func (mf *makefile) firstRule() *makeRule {
	for _, rl := range mf.rules {
		for _, tg := range rl.targets {
			if strings.Contains(tg, "$") {
				words, ok := mf.expand(tg)
				if !ok {
					return rl
				}
				if len(words) == 0 {
					continue
				}
				tg = words[0]
			}
			if (!strings.HasPrefix(tg, ".") || strings.Contains(tg, "/")) && !strings.Contains(tg, "%") {
				return rl
			}
		}
	}
	return nil
}

// ---- justfiles ----

type justRecipe struct {
	name     string
	params   int
	variadic bool
	deps     []string
	lines    []span // one per shell line; one for the whole body of a shebang recipe
	shebang  string // the #! line of a shebang recipe
}

type justfile struct {
	recipes []*justRecipe
	aliases map[string]string
	exports []string
	imports bool // import or mod lines: recipes may come from files not read here
}

var (
	justHeader = regexp.MustCompile(`^@?([A-Za-z_][A-Za-z0-9_-]*)([^:]*?)\s*:([^=].*|)$`)
	justAlias  = regexp.MustCompile(`^alias\s+([A-Za-z_][A-Za-z0-9_-]*)\s*:=\s*([A-Za-z_][A-Za-z0-9_-]*)`)
	justExport = regexp.MustCompile(`^export\s+([A-Za-z_][A-Za-z0-9_]*)`)
	justAssign = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*:=`)
	justImport = regexp.MustCompile(`^(import|mod)\??\s`)
	justSetExp = regexp.MustCompile(`^set\s+export(\s*:=\s*true)?\s*(#.*)?$`)
)

// parseJustfile reads recipes (name, parameters, dependencies, and body lines with leading @ and
// - removed), aliases and exported variables. Settings other than `set export`, modules,
// imports and attributes are ignored; recipes run in the justfile's directory.
func parseJustfile(text string) *justfile {
	jf := &justfile{aliases: map[string]string{}}
	ls := lines(text)
	var cur *justRecipe
	var body []span
	var assigned []string
	exportAll := false
	flush := func() {
		if cur == nil {
			return
		}
		indented := func(sp span) span {
			for sp.start < sp.end && (text[sp.start] == ' ' || text[sp.start] == '\t') {
				sp.start++
			}
			return sp
		}
		if len(body) > 0 && strings.HasPrefix(text[indented(body[0]).start:body[0].end], "#!") {
			first := indented(body[0])
			cur.shebang = text[first.start:first.end]
			cur.lines = []span{{first.start, body[len(body)-1].end}}
		} else {
			for k := 0; k < len(body); k++ {
				sp := indented(body[k])
				for sp.start < sp.end && (text[sp.start] == '@' || text[sp.start] == '-') {
					sp.start++
				}
				for k+1 < len(body) && continued(text[body[k].start:body[k].end]) {
					k++
					sp.end = body[k].end
				}
				if sp.start < sp.end {
					cur.lines = append(cur.lines, sp)
				}
			}
		}
		jf.recipes = append(jf.recipes, cur)
		cur, body = nil, nil
	}
	for _, sp := range ls {
		raw := text[sp.start:sp.end]
		line := strings.TrimSpace(raw)
		if cur != nil && (line == "" || raw[0] == ' ' || raw[0] == '\t') {
			if line != "" {
				body = append(body, sp)
			}
			continue
		}
		flush()
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "["):
		case justAlias.MatchString(line):
			m := justAlias.FindStringSubmatch(line)
			jf.aliases[m[1]] = m[2]
		case justSetExp.MatchString(line):
			exportAll = true
		case justImport.MatchString(line):
			jf.imports = true
		case strings.HasPrefix(line, "set "):
		case justExport.MatchString(line):
			jf.exports = append(jf.exports, justExport.FindStringSubmatch(line)[1])
		case justAssign.MatchString(line):
			assigned = append(assigned, justAssign.FindStringSubmatch(line)[1])
		case justHeader.MatchString(line):
			m := justHeader.FindStringSubmatch(line)
			cur = &justRecipe{name: m[1], deps: justDeps(m[3])}
			for _, p := range strings.Fields(m[2]) {
				if p[0] == '=' || p[0] == '\'' || p[0] == '"' {
					continue
				}
				cur.params++
				cur.variadic = cur.variadic || p[0] == '+' || p[0] == '*'
			}
		}
	}
	flush()
	if exportAll {
		jf.exports = append(jf.exports, assigned...)
	}
	return jf
}

// justDeps returns the recipe names in a dependency list: `a (b "x") && c  # comment`.
func justDeps(s string) []string {
	if k := strings.IndexByte(s, '#'); k >= 0 {
		s = s[:k]
	}
	var out []string
	depth := 0
	for _, f := range strings.Fields(strings.NewReplacer("(", " ( ", ")", " ) ").Replace(s)) {
		switch {
		case f == "(":
			depth++
			if depth == 1 {
				out = append(out, "")
			}
		case f == ")":
			depth--
		case f == "&&":
		case depth == 0:
			out = append(out, f)
		case depth == 1 && len(out) > 0 && out[len(out)-1] == "":
			out[len(out)-1] = f
		}
	}
	return slices.DeleteFunc(out, func(s string) bool { return s == "" })
}

// reachable returns the recipes that run for the command-line arguments (the first recipe when
// there are none): each named recipe consumes its parameters' arguments, and dependencies are
// followed.
//
// unknown reports what cannot be seen and counts as a push: a named recipe or dependency that
// does not exist (it may come from an import), or no recipe at all in a justfile with imports.
func (jf *justfile) reachable(args []string) (reach map[*justRecipe]bool, unknown bool) {
	find := func(name string) *justRecipe {
		if a, ok := jf.aliases[name]; ok {
			name = a
		}
		for _, rc := range jf.recipes {
			if rc.name == name {
				return rc
			}
		}
		return nil
	}
	var goals []*justRecipe
	if len(args) == 0 {
		if len(jf.recipes) == 0 {
			return nil, jf.imports
		}
		goals = append(goals, jf.recipes[0])
	}
	for i := 0; i < len(args); i++ {
		rc := find(args[i])
		if rc == nil {
			unknown = true
			continue
		}
		goals = append(goals, rc)
		if rc.variadic {
			break
		}
		i += rc.params
	}
	reach = map[*justRecipe]bool{}
	for len(goals) > 0 {
		rc := goals[0]
		goals = goals[1:]
		if reach[rc] {
			continue
		}
		reach[rc] = true
		for _, d := range rc.deps {
			if dep := find(d); dep != nil {
				goals = append(goals, dep)
			} else {
				unknown = true
			}
		}
	}
	return reach, unknown
}
