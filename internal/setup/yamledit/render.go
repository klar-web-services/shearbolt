package yamledit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// yaml11Literals are words other YAML parsers (YAML 1.1, GitHub's) read as booleans or null,
// although yaml.v3 reads them as strings.
var yaml11Literals = map[string]bool{"y": true, "yes": true, "n": true, "no": true, "true": true, "false": true, "on": true, "off": true, "null": true, "~": true}

// numberLike matches anything a YAML parser might read as a number (ints, floats, hex, octal,
// sexagesimal, versions); quoting those is always safe.
var numberLike = regexp.MustCompile(`^[-+]?(\.?[0-9])[0-9A-Za-z_.:+-]*$`)

// Scalar renders s as a block-context YAML scalar: plain when that is safe, otherwise
// single-quoted (” escapes a quote), or double-quoted when s has a newline or a control
// character.
func Scalar(s string) string {
	switch {
	case needsEscape(s) || strings.Contains(s, "\n"):
		return doubleQuote(s)
	case plainOK(s):
		return s
	}
	return singleQuote(s)
}

func plainOK(s string) bool {
	switch {
	case s == "":
		return false
	case strings.ContainsRune("\"'&*!|>%@#,[]{}`", rune(s[0])):
		return false
	case (s[0] == '-' || s[0] == '?' || s[0] == ':') && (len(s) == 1 || s[1] == ' ' || s[1] == '\t'):
		return false
	case strings.Contains(s, ": ") || strings.Contains(s, ":\t") || strings.Contains(s, " #") || strings.Contains(s, "\t#") || strings.HasSuffix(s, ":"):
		return false
	case strings.TrimSpace(s) != s:
		return false
	case strings.HasPrefix(s, "---") || strings.HasPrefix(s, "..."):
		return false
	case yaml11Literals[strings.ToLower(s)] || numberLike.MatchString(s):
		return false
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("k: "+s+"\n"), &n); err != nil {
		return false
	}
	v := resolve(&n)
	if v == nil || v.Kind != yaml.MappingNode || len(v.Content) != 2 {
		return false
	}
	val := v.Content[1]
	return val.Kind == yaml.ScalarNode && val.Tag == "!!str" && val.Style == 0 && val.Value == s
}

// needsEscape reports characters only a double-quoted scalar can hold: anything outside YAML's
// printable set, plus the BOM and the characters YAML 1.1 parsers read as line breaks.
func needsEscape(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && !printable(r) {
			return true
		}
	}
	return false
}

func printable(r rune) bool {
	switch {
	case r == 0xfeff || r == 0x2028 || r == 0x2029:
		return false
	case r >= 0x20 && r <= 0x7e, r >= 0xa0 && r <= 0xd7ff, r >= 0xe000 && r <= 0xfffd, r >= 0x10000 && r <= 0x10ffff:
		return true
	}
	return false
}

func singleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func doubleQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case printable(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// RenderScript renders a script value for the scalar of a key (or sequence dash) at keyIndent.
// The first returned line goes where the scalar starts; the others are complete lines.
//
//   - A value without a newline in a plain or quoted scalar stays on one line: plain when Scalar
//     allows it, otherwise in its original quote style (Scalar's single quotes for a plain one).
//   - Anything else is a literal block at contentIndent (keyIndent+2 if that is not deeper).
//     Its chomping indicator follows the value: "|-" without a final newline, "|" with one, "|+"
//     with more. A folded block becomes literal, which keeps the value exact.
//   - A value with control characters, or one a literal block cannot hold exactly (it is parsed
//     back to check), is double-quoted on one line.
func RenderScript(value string, style yaml.Style, keyIndent, contentIndent int) []string {
	block := style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0
	switch {
	case needsEscape(value):
		return []string{doubleQuote(value)}
	case !block && !strings.Contains(value, "\n"):
		switch {
		case style&yaml.DoubleQuotedStyle != 0:
			return []string{doubleQuote(value)}
		case style&yaml.SingleQuotedStyle != 0:
			return []string{singleQuote(value)}
		}
		return []string{Scalar(value)}
	}
	if contentIndent <= keyIndent || contentIndent-keyIndent > 9 {
		contentIndent = keyIndent + 2
	}
	body := strings.TrimRight(value, "\n")
	trailing := len(value) - len(body)
	chomp := "+"
	switch trailing {
	case 0:
		chomp = "-"
	case 1:
		chomp = ""
	}
	var content []string
	if body != "" {
		content = strings.Split(body, "\n")
	}
	for range max(trailing-1, 0) {
		content = append(content, "")
	}
	header := "|"
	for _, l := range content {
		if l == "" {
			continue
		}
		if l[0] == ' ' {
			header += strconv.Itoa(contentIndent - keyIndent)
		}
		break
	}
	out := []string{header + chomp}
	pad := strings.Repeat(" ", contentIndent)
	for _, l := range content {
		if l == "" {
			out = append(out, "")
		} else {
			out = append(out, pad+l)
		}
	}
	if !readsBack(out, keyIndent, value) {
		return []string{doubleQuote(value)}
	}
	return out
}

// readsBack reports whether block lines, rendered for a key at keyIndent, parse back to value.
// Some values have no literal block form (only newlines, whitespace-only lines with tabs), so
// they fall back to a double-quoted scalar.
func readsBack(block []string, keyIndent int, value string) bool {
	doc := strings.Repeat(" ", keyIndent) + "k: " + strings.Join(block, "\n") + "\n"
	var v map[string]any
	if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
		return false
	}
	s, ok := v["k"].(string)
	return ok && s == value
}
