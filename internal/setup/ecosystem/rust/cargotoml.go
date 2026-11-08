package rust

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file is a minimal TOML reader for Cargo.toml and .cargo/config.toml, so setup needs no TOML
// dependency. It understands table headers ([a.b], [[bin]]), dotted and quoted keys, basic and
// literal strings (multi-line ones included), booleans, and string arrays that span lines with
// comments and trailing commas. Other values (numbers, dates, inline tables, nested arrays) are
// skipped. Unusual syntax can make it miss a value; callers fall back to the defaults.

// tomlValue is one value: a string, a boolean, or the strings of an array.
type tomlValue struct {
	kind byte // 's' string, 'b' boolean, 'a' array (its string elements), '?' anything else
	str  string
	b    bool
	list []string
}

// tomlEntry is one `key = value` line, or a table header (key == "").
type tomlEntry struct {
	table string // dotted table name, "" at the top level
	index int    // element of an array of tables ([[bin]]), from 0; -1 for a plain table
	key   string // dotted key relative to the table
	val   tomlValue
}

// full returns the entry's key from the top level: "package.name", "build.target".
func (e tomlEntry) full() string {
	switch {
	case e.table == "":
		return e.key
	case e.key == "":
		return e.table
	}
	return e.table + "." + e.key
}

type tomlReader struct {
	s string
	i int
}

func readTOML(src string) []tomlEntry {
	p := &tomlReader{s: src}
	var out []tomlEntry
	table, index := "", -1
	counts := map[string]int{}
	for p.i < len(p.s) {
		p.skipSpace()
		if p.i >= len(p.s) {
			break
		}
		switch c := p.s[p.i]; {
		case c == '\n' || c == '\r':
			p.i++
		case c == '#':
			p.restOfLine()
		case c == '[':
			array := strings.HasPrefix(p.s[p.i:], "[[")
			p.i++
			if array {
				p.i++
			}
			name, ok := p.key()
			p.restOfLine()
			if !ok {
				table, index = "\x00", -1 // an unreadable header: ignore its keys
				continue
			}
			table, index = name, -1
			if array {
				index = counts[name]
				counts[name]++
			}
			out = append(out, tomlEntry{table: table, index: index})
		default:
			k, ok := p.key()
			p.skipSpace()
			if !ok || p.i >= len(p.s) || p.s[p.i] != '=' {
				p.restOfLine()
				continue
			}
			p.i++
			p.skipSpace()
			v := p.value()
			p.restOfLine()
			if table != "\x00" {
				out = append(out, tomlEntry{table: table, index: index, key: k, val: v})
			}
		}
	}
	return out
}

func (p *tomlReader) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

// skipBlank skips whitespace, newlines and comments (inside arrays).
func (p *tomlReader) skipBlank() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\r', '\n':
			p.i++
		case '#':
			p.restOfLine()
		default:
			return
		}
	}
}

// restOfLine moves past the next newline.
func (p *tomlReader) restOfLine() {
	if j := strings.IndexByte(p.s[p.i:], '\n'); j >= 0 {
		p.i += j + 1
	} else {
		p.i = len(p.s)
	}
}

func bare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// key reads a dotted key of bare and quoted parts.
func (p *tomlReader) key() (string, bool) {
	var parts []string
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return "", false
		}
		if c := p.s[p.i]; c == '"' || c == '\'' {
			v, ok := p.str()
			if !ok {
				return "", false
			}
			parts = append(parts, v)
		} else {
			j := p.i
			for j < len(p.s) && bare(p.s[j]) {
				j++
			}
			if j == p.i {
				return "", false
			}
			parts = append(parts, p.s[p.i:j])
			p.i = j
		}
		p.skipSpace()
		if p.i < len(p.s) && p.s[p.i] == '.' {
			p.i++
			continue
		}
		return strings.Join(parts, "."), true
	}
}

// str reads the string that starts at p.i.
func (p *tomlReader) str() (string, bool) {
	q := p.s[p.i]
	if triple := strings.Repeat(string(q), 3); strings.HasPrefix(p.s[p.i:], triple) {
		p.i += 3
		end := strings.Index(p.s[p.i:], triple)
		if end < 0 {
			p.i = len(p.s)
			return "", false
		}
		v := p.s[p.i : p.i+end]
		p.i += end + 3
		v = strings.TrimPrefix(strings.TrimPrefix(v, "\r"), "\n")
		if q == '"' {
			v = unescape(v)
		}
		return v, true
	}
	p.i++
	start := p.i
	for p.i < len(p.s) {
		switch c := p.s[p.i]; {
		case c == q:
			v := p.s[start:p.i]
			p.i++
			if q == '"' {
				v = unescape(v)
			}
			return v, true
		case c == '\n':
			return "", false
		case c == '\\' && q == '"':
			p.i += 2
		default:
			p.i++
		}
	}
	return "", false
}

// unescape decodes the escapes of a basic string.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'u', 'U':
			n := 4
			if c == 'U' {
				n = 8
			}
			if i+n < len(s) {
				if r, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32); err == nil && utf8.ValidRune(rune(r)) {
					b.WriteRune(rune(r))
					i += n
					continue
				}
			}
			b.WriteByte(c)
		case '\n', '\r', ' ', '\t':
			// A line-ending backslash trims the whitespace that follows.
			for i < len(s) && strings.IndexByte(" \t\r\n", s[i]) >= 0 {
				i++
			}
			i--
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (p *tomlReader) value() tomlValue {
	if p.i >= len(p.s) {
		return tomlValue{kind: '?'}
	}
	switch c := p.s[p.i]; c {
	case '"', '\'':
		if s, ok := p.str(); ok {
			return tomlValue{kind: 's', str: s}
		}
	case '[':
		return p.array()
	case '{':
		p.skipInline()
	default:
		for _, w := range []string{"true", "false"} {
			if strings.HasPrefix(p.s[p.i:], w) && (p.i+len(w) == len(p.s) || !bare(p.s[p.i+len(w)])) {
				p.i += len(w)
				return tomlValue{kind: 'b', b: w == "true"}
			}
		}
	}
	return tomlValue{kind: '?'}
}

// array reads an array and keeps its string elements.
func (p *tomlReader) array() tomlValue {
	v := tomlValue{kind: 'a', list: []string{}}
	p.i++
	for p.i < len(p.s) {
		p.skipBlank()
		if p.i >= len(p.s) {
			break
		}
		switch c := p.s[p.i]; c {
		case ']':
			p.i++
			return v
		case ',':
			p.i++
		case '"', '\'':
			if s, ok := p.str(); ok {
				v.list = append(v.list, s)
			}
		case '[':
			p.array()
		case '{':
			p.skipInline()
		default:
			for p.i < len(p.s) && strings.IndexByte(",]#\n", p.s[p.i]) < 0 {
				p.i++
			}
		}
	}
	return v
}

// skipInline skips an inline table, including nested ones and the strings inside.
func (p *tomlReader) skipInline() {
	depth := 0
	for p.i < len(p.s) {
		switch c := p.s[p.i]; c {
		case '{', '[':
			depth++
			p.i++
		case '}', ']':
			depth--
			p.i++
			if depth == 0 {
				return
			}
		case '"', '\'':
			if _, ok := p.str(); !ok {
				return
			}
		case '\n':
			return
		default:
			p.i++
		}
	}
}

// cargoBin is one [[bin]] table.
type cargoBin struct {
	name, path       string
	requiredFeatures bool
}

// cargoManifest is what setup reads from a Cargo.toml.
type cargoManifest struct {
	pkg, workspace bool
	name           string
	edition        string
	autobins       *bool // nil when not set
	bins           []cargoBin
	members        []string
	defaultMembers []string // nil when not set
}

func parseManifest(src string) cargoManifest {
	var m cargoManifest
	for _, e := range readTOML(src) {
		full := e.full()
		top, _, _ := strings.Cut(full, ".")
		switch top {
		case "package":
			m.pkg = true
		case "workspace":
			m.workspace = true
		}
		if e.table == "bin" && e.index >= 0 {
			for len(m.bins) <= e.index {
				m.bins = append(m.bins, cargoBin{})
			}
			b := &m.bins[e.index]
			switch e.key {
			case "name":
				b.name = e.val.str
			case "path":
				b.path = e.val.str
			case "required-features":
				b.requiredFeatures = e.val.kind != 'a' || len(e.val.list) > 0
			}
			continue
		}
		switch full {
		case "package.name":
			m.name = e.val.str
		case "package.edition":
			m.edition = e.val.str
		case "package.autobins":
			if e.val.kind == 'b' {
				b := e.val.b
				m.autobins = &b
			}
		case "workspace.members":
			m.members = e.val.list
		case "workspace.default-members":
			m.defaultMembers = append([]string{}, e.val.list...)
		}
	}
	return m
}

// setsTarget reports whether a Cargo config file sets [build] target or target-dir, which move
// the output directory.
func setsTarget(src string) bool {
	for _, e := range readTOML(src) {
		if f := e.full(); f == "build.target" || f == "build.target-dir" {
			return true
		}
	}
	return false
}
