// Package yamledit edits YAML files by splicing text at the positions go.yaml.in/yaml/v3 reports,
// never by re-serializing, so comments, anchors, quoting and every untouched line stay as written.
//
// Lines are 1-based, as in yaml.v3. Columns are 0-based and count characters (runes), as
// yaml.v3 does; they double as indentation widths. Edits are queued and applied by Result,
// which also checks that the edited file still parses; an edit that overlaps an earlier one is
// refused when it is queued.
package yamledit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

var (
	// ErrMultiDoc is returned for a file with more than one YAML document.
	ErrMultiDoc = errors.New("the YAML file has more than one document")
	// ErrLineBreak is returned for a file with a line break other than \n or \r\n (a lone \r,
	// U+0085, U+2028 or U+2029). yaml.v3 counts those as lines, so its positions would not
	// match the file's lines.
	ErrLineBreak = errors.New("the YAML file has line breaks other than \\n and \\r\\n")
	// ErrFlowStyle is returned when an edit would have to change a flow-style mapping or sequence.
	ErrFlowStyle = errors.New("flow-style YAML ({...} or [...]) cannot be edited in place")
	// ErrOverlap is returned for an edit that overlaps an edit queued earlier.
	ErrOverlap = errors.New("overlapping YAML edits")
	// ErrLastEntry is returned when DeleteEntry would empty a mapping; delete its parent entry instead.
	ErrLastEntry = errors.New("cannot delete the only entry of a mapping")
	// ErrAnchor is returned when an edit would replace or delete text that defines an anchor.
	ErrAnchor = errors.New("the edit would remove an anchor")
	// ErrDuplicateKey is returned by Result for a mapping with the same key twice; GitHub and
	// config.Load reject those, although yaml.v3 accepts them.
	ErrDuplicateKey = errors.New("a mapping has the same key twice")
	// ErrEncoding is returned for a file that is not valid UTF-8, such as a UTF-16 file.
	ErrEncoding = errors.New("the YAML file is not valid UTF-8; save it as UTF-8 (UTF-16 is not supported)")
)

// bom is the UTF-8 byte order mark. yaml.v3 skips it, so Parse strips it before splitting lines
// (positions on line 1 then match) and Result puts it back.
const bom = "\xef\xbb\xbf"

// Doc is a parsed YAML file plus its queued edits.
type Doc struct {
	doc    *yaml.Node // the document node; nil for a file without content
	src    []byte     // the file as given, BOM included
	bom    bool
	lines  []string // each line without its line ending (and line 1 without the BOM)
	ends   []string // each line's ending as written; "" for a last line without one
	eol    string   // line ending for inserted lines
	docEnd int      // last line of the document: before a "..." end marker, else the last line
	parent map[*yaml.Node]*yaml.Node
	edits  []edit
}

type edit struct {
	first, last int // replace lines first..last; last == first-1 inserts before first
	lines       []string
}

func (e edit) insert() bool { return e.last < e.first }

// Parse parses src, which must be UTF-8, hold at most one YAML document and use only \n or
// \r\n line breaks. A leading BOM is allowed and kept.
func Parse(src []byte) (*Doc, error) {
	d := &Doc{src: src, eol: "\n", parent: map[*yaml.Node]*yaml.Node{}}
	body := src
	if bytes.HasPrefix(body, []byte(bom)) {
		d.bom, body = true, body[len(bom):]
	}
	if !utf8.Valid(body) {
		return nil, ErrEncoding
	}
	for _, br := range []string{"\xc2\x85", "\xe2\x80\xa8", "\xe2\x80\xa9"} {
		if bytes.Contains(body, []byte(br)) {
			return nil, ErrLineBreak
		}
	}
	for i, c := range body {
		if c == '\r' && (i+1 == len(body) || body[i+1] != '\n') {
			return nil, ErrLineBreak
		}
	}
	s := string(body)
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			d.lines, d.ends = append(d.lines, s), append(d.ends, "")
			break
		}
		line, end := s[:i], "\n"
		if strings.HasSuffix(line, "\r") {
			line, end = line[:len(line)-1], "\r\n"
		}
		d.lines, d.ends = append(d.lines, line), append(d.ends, end)
		s = s[i+1:]
	}
	if len(d.ends) > 0 && d.ends[0] == "\r\n" {
		d.eol = "\r\n"
	}
	d.docEnd = len(d.lines)
	dec := yaml.NewDecoder(bytes.NewReader(body))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return d, nil
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrMultiDoc
	}
	d.doc = &root
	d.index(&root)
	if r := d.Root(); r != nil {
		for l := r.Line; l <= len(d.lines); l++ {
			if t := d.lines[l-1]; t == "..." || strings.HasPrefix(t, "... ") || strings.HasPrefix(t, "...\t") {
				d.docEnd = l - 1
				break
			}
		}
	}
	return d, nil
}

func (d *Doc) index(n *yaml.Node) {
	for _, c := range n.Content {
		d.parent[c] = n
		d.index(c)
	}
}

// Root returns the document's top-level node, or nil for a file without content.
func (d *Doc) Root() *yaml.Node {
	if d.doc == nil || len(d.doc.Content) == 0 {
		return nil
	}
	return d.doc.Content[0]
}

// Parent returns the container n is in (a mapping, sequence or the document node).
func (d *Doc) Parent(n *yaml.Node) *yaml.Node { return d.parent[n] }

// EOL returns the line ending used for inserted lines: "\r\n" when the file uses CRLF.
func (d *Doc) EOL() string { return d.eol }

// NumLines returns the number of lines in the original file.
func (d *Doc) NumLines() int { return len(d.lines) }

// Line returns line n of the original file without its line ending (and line 1 without a BOM).
func (d *Doc) Line(n int) string {
	if n < 1 || n > len(d.lines) {
		return ""
	}
	return d.lines[n-1]
}

// offset returns the byte offset of node n in its line. yaml.v3 columns count runes.
func (d *Doc) offset(n *yaml.Node) int {
	line, col := d.Line(n.Line), n.Column-1
	for i := range line {
		if col == 0 {
			return i
		}
		col--
	}
	return len(line)
}

// column returns the rune column of byte offset i in line.
func column(line string, i int) int { return utf8.RuneCountInString(line[:min(i, len(line))]) }

// Get follows path from n: mapping keys and sequence indexes, given as strings. Aliases are
// resolved, so edits on the result land at the anchor. It returns nil when the path is missing.
// Merge keys (<<) are not resolved.
func (d *Doc) Get(n *yaml.Node, path ...string) *yaml.Node {
	n = resolve(n)
	for _, p := range path {
		switch {
		case n == nil:
			return nil
		case n.Kind == yaml.MappingNode:
			i := findKey(n, p)
			if i < 0 {
				return nil
			}
			n = n.Content[2*i+1]
		case n.Kind == yaml.SequenceNode:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(n.Content) {
				return nil
			}
			n = n.Content[i]
		default:
			return nil
		}
		n = resolve(n)
	}
	return n
}

func resolve(n *yaml.Node) *yaml.Node {
	for range 100 {
		switch {
		case n == nil:
			return nil
		case n.Kind == yaml.AliasNode:
			n = n.Alias
		case n.Kind == yaml.DocumentNode:
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
		default:
			return n
		}
	}
	return nil
}

// findKey returns the index of key's entry in mapping m (entry i is m.Content[2i:2i+2]), or -1.
func findKey(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key {
			return i / 2
		}
	}
	return -1
}

// inFlow reports whether n is, or is inside, a flow-style mapping or sequence.
func (d *Doc) inFlow(n *yaml.Node) bool {
	for ; n != nil; n = d.parent[n] {
		if (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && n.Style&yaml.FlowStyle != 0 {
			return true
		}
	}
	return false
}

// hasAnchor reports whether n or anything inside it defines an anchor (aliases are not followed).
func hasAnchor(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Anchor != "" {
		return true
	}
	return slices.ContainsFunc(n.Content, hasAnchor)
}

func (d *Doc) mapping(m *yaml.Node) (*yaml.Node, error) {
	m = resolve(m)
	if m == nil || m.Kind != yaml.MappingNode || len(m.Content) == 0 {
		return nil, errors.New("yamledit: not a non-empty mapping")
	}
	if d.inFlow(m) {
		return nil, ErrFlowStyle
	}
	return m, nil
}

func (d *Doc) sequence(s *yaml.Node) (*yaml.Node, error) {
	s = resolve(s)
	if s == nil || s.Kind != yaml.SequenceNode || len(s.Content) == 0 {
		return nil, errors.New("yamledit: not a non-empty sequence")
	}
	if d.inFlow(s) {
		return nil, ErrFlowStyle
	}
	return s, nil
}

// ---- columns ----

// KeyColumn returns the indentation of mapping m's keys, taken from its first key. The
// mapping's own position is never used: yaml.v3 reports an anchor or tag there.
func (d *Doc) KeyColumn(m *yaml.Node) (int, error) {
	m, err := d.mapping(m)
	if err != nil {
		return 0, err
	}
	return m.Content[0].Column - 1, nil
}

// contentNode is the node whose position starts a sequence item's content: a mapping's first
// key, otherwise the item itself.
func contentNode(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.MappingNode && n.Style&yaml.FlowStyle == 0 && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

// emptyNull reports a null written as nothing at all, such as the item of a lone "-".
func emptyNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null" && n.Value == "" && n.Style == 0
}

// ContentColumn returns the column where sequence s's items start (after "- ").
func (d *Doc) ContentColumn(s *yaml.Node) (int, error) {
	s, err := d.sequence(s)
	if err != nil {
		return 0, err
	}
	return d.contentColumn(s), nil
}

func (d *Doc) contentColumn(s *yaml.Node) int {
	if cn := contentNode(s.Content[0]); !emptyNull(cn) {
		return cn.Column - 1
	}
	return d.dashColumn(s) + 2
}

// DashColumn returns the column of sequence s's dashes, from the "-" before its first item.
func (d *Doc) DashColumn(s *yaml.Node) (int, error) {
	s, err := d.sequence(s)
	if err != nil {
		return 0, err
	}
	return d.dashColumn(s), nil
}

func (d *Doc) dashColumn(s *yaml.Node) int {
	cn := contentNode(s.Content[0])
	line, col := cn.Line, cn.Column-1
	text := d.Line(line)
	for c := min(d.offset(cn), len(text)) - 1; c >= 0; c-- {
		if text[c] == '-' && (c == 0 || text[c-1] == ' ') && (c+1 == len(text) || text[c+1] == ' ' || text[c+1] == '\t') {
			return column(text, c)
		}
	}
	// The content starts on a later line than its dash (`- &anchor` alone on a line).
	for l := line - 1; l >= 1; l-- {
		t := d.lines[l-1]
		ind := indent(t)
		if ind < len(t) && t[ind] == '-' && ind < col && (ind+1 == len(t) || t[ind+1] == ' ' || t[ind+1] == '\t') {
			return ind
		}
		if !blank(t) && !isComment(t) {
			break
		}
	}
	return max(col-2, 0)
}

// dashLine returns the line of item i's dash.
func (d *Doc) dashLine(s *yaml.Node, i int) int {
	dc := d.dashColumn(s)
	line := contentNode(s.Content[i]).Line
	for l := line; l >= 1; l-- {
		t := d.lines[l-1]
		if indent(t) == dc && len(t) > dc && t[dc] == '-' {
			return l
		}
		if l < line && !blank(t) && !isComment(t) {
			break
		}
	}
	return line
}

// parentIndent returns the indentation a block scalar n must exceed: its mapping's key column,
// or its sequence's dash column; -1 at the top level.
func (d *Doc) parentIndent(n *yaml.Node) int {
	switch p := d.parent[n]; {
	case p == nil || p.Kind == yaml.DocumentNode:
		return -1
	case p.Kind == yaml.MappingNode && len(p.Content) > 0:
		return p.Content[0].Column - 1
	case p.Kind == yaml.SequenceNode && len(p.Content) > 0:
		return d.dashColumn(p)
	}
	return -1
}

// IndentUnit returns the smallest positive indentation step between a key and its block
// mapping or sequence value, or 2 when there is none.
func IndentUnit(d *Doc) int {
	unit := 0
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode && n.Style&yaml.FlowStyle == 0 && len(n.Content) > 0 {
			kc := n.Content[0].Column - 1
			for k := 1; k < len(n.Content); k += 2 {
				v := n.Content[k]
				if v.Style&yaml.FlowStyle != 0 || len(v.Content) == 0 {
					continue
				}
				c := -1
				switch v.Kind {
				case yaml.MappingNode:
					c = v.Content[0].Column - 1
				case yaml.SequenceNode:
					c = d.dashColumn(v)
				}
				if step := c - kc; c >= 0 && step > 0 && (unit == 0 || step < unit) {
					unit = step
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	if r := d.Root(); r != nil {
		walk(r)
	}
	if unit == 0 {
		return 2
	}
	return unit
}

// ---- spans ----

// ItemStart returns the first line of item i: its dash line, moved up over directly preceding
// comment lines indented less than the item's content column.
func (d *Doc) ItemStart(s *yaml.Node, i int) (int, error) {
	s, err := d.sequence(s)
	if err != nil {
		return 0, err
	}
	if i < 0 || i >= len(s.Content) {
		return 0, fmt.Errorf("yamledit: no item %d", i)
	}
	cc := d.contentColumn(s)
	l := d.dashLine(s, i)
	for l > 1 && isComment(d.lines[l-2]) && indent(d.lines[l-2]) < cc {
		l--
	}
	return l, nil
}

// ItemEnd returns the last line of item i: the line before item i+1's dash line, or the
// sequence's end, trimmed back over blank lines and comments at or left of the dash column.
func (d *Doc) ItemEnd(s *yaml.Node, i int) (int, error) {
	s, err := d.sequence(s)
	if err != nil {
		return 0, err
	}
	if i < 0 || i >= len(s.Content) {
		return 0, fmt.Errorf("yamledit: no item %d", i)
	}
	return d.itemEnd(s, i), nil
}

// EntryEnd returns the last line of key's entry in m: the line before the next key, or the
// mapping's end, trimmed back over blank lines and comments at or left of the key column.
func (d *Doc) EntryEnd(m *yaml.Node, key string) (int, error) {
	m, err := d.mapping(m)
	if err != nil {
		return 0, err
	}
	i := findKey(m, key)
	if i < 0 {
		return 0, fmt.Errorf("yamledit: no key %q", key)
	}
	return d.entryEnd(m, i), nil
}

func (d *Doc) itemEnd(s *yaml.Node, i int) int {
	end := 0
	if i+1 < len(s.Content) {
		end = d.dashLine(s, i+1) - 1
	} else {
		end = d.end(s)
	}
	return d.trim(end, d.dashColumn(s), max(d.dashLine(s, i), d.keepFloor(s.Content[i])))
}

func (d *Doc) entryEnd(m *yaml.Node, i int) int {
	end := 0
	if 2*(i+1) < len(m.Content) {
		end = m.Content[2*(i+1)].Line - 1
	} else {
		end = d.end(m)
	}
	return d.trim(end, m.Content[0].Column-1, max(m.Content[2*i].Line, d.keepFloor(m.Content[2*i+1])))
}

// keepFloor returns the last line of the keep-chomped (|+) block scalar that n's text ends with,
// or 0. Its trailing blank lines are part of its value, so a span never ends before them.
func (d *Doc) keepFloor(n *yaml.Node) int {
	for (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && n.Style&yaml.FlowStyle == 0 && len(n.Content) > 0 {
		n = n.Content[len(n.Content)-1]
	}
	if n.Kind != yaml.ScalarNode || n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0 || !strings.Contains(d.blockHeader(n), "+") {
		return 0
	}
	_, last := d.block(n)
	return last
}

// end returns the last line of node n: its parent entry's or item's end, or the document's.
func (d *Doc) end(n *yaml.Node) int {
	switch p := d.parent[n]; {
	case p == nil || p.Kind == yaml.DocumentNode:
		return d.docEnd
	case p.Kind == yaml.MappingNode:
		for k := 1; k < len(p.Content); k += 2 {
			if p.Content[k] == n {
				return d.entryEnd(p, k/2)
			}
		}
	case p.Kind == yaml.SequenceNode:
		for i, c := range p.Content {
			if c == n {
				return d.itemEnd(p, i)
			}
		}
	}
	return d.docEnd
}

// trim moves end up over blank lines and comment lines at or left of col, but not above start.
func (d *Doc) trim(end, col, start int) int {
	end = min(end, len(d.lines))
	for end > start {
		t := d.lines[end-1]
		if !blank(t) && !(isComment(t) && indent(t) <= col) {
			break
		}
		end--
	}
	return max(end, start)
}

// ---- edits ----

// InsertBefore queues lines to be inserted before line (len+1 appends).
func (d *Doc) InsertBefore(line int, lines []string) error { return d.queue(line, line-1, lines) }

// InsertAfter queues lines to be inserted after line (0 inserts at the top).
func (d *Doc) InsertAfter(line int, lines []string) error { return d.queue(line+1, line, lines) }

// ReplaceLines queues lines first..last to be replaced by lines (nil deletes them).
func (d *Doc) ReplaceLines(first, last int, lines []string) error {
	if last < first {
		return fmt.Errorf("yamledit: empty line range %d-%d", first, last)
	}
	return d.queue(first, last, lines)
}

func (d *Doc) queue(first, last int, lines []string) error {
	if first < 1 || first > len(d.lines)+1 || last > len(d.lines) {
		return fmt.Errorf("yamledit: lines %d-%d are outside the file (%d lines)", first, last, len(d.lines))
	}
	e := edit{first: first, last: last}
	for _, l := range lines {
		for _, part := range strings.Split(l, "\n") {
			e.lines = append(e.lines, strings.TrimSuffix(part, "\r"))
		}
	}
	for _, o := range d.edits {
		if overlaps(o, e) {
			return fmt.Errorf("%w: lines %d-%d and %d-%d", ErrOverlap, o.first, o.last, e.first, e.last)
		}
	}
	d.edits = append(d.edits, e)
	return nil
}

// overlaps reports whether two edits touch the same text. Inserts at the same point do not
// overlap; they are applied in the order they were queued. An insert at either edge of a
// replaced range does not overlap it.
func overlaps(a, b edit) bool {
	switch {
	case a.insert() && b.insert():
		return false
	case a.insert():
		return a.first > b.first && a.first <= b.last
	case b.insert():
		return b.first > a.first && b.first <= a.last
	}
	return a.first <= b.last && b.first <= a.last
}

// Checkpoint returns a mark for Rollback: the number of edits queued so far.
func (d *Doc) Checkpoint() int { return len(d.edits) }

// Rollback drops every edit queued after checkpoint cp, so a caller can give up a group of
// edits (one CI job's, say) and keep the others.
func (d *Doc) Rollback(cp int) {
	if cp >= 0 && cp < len(d.edits) {
		d.edits = d.edits[:cp]
	}
}

// Result returns the file with the queued edits applied, after checking that it still parses
// as a single YAML document under Parse's rules (which also catches an alias whose anchor an
// edit removed). It is the only way to get the edited file out of a Doc.
func (d *Doc) Result() ([]byte, error) {
	if len(d.edits) == 0 {
		return slices.Clone(d.src), nil // Parse accepted it
	}
	out := d.bytes()
	nd, err := Parse(out)
	if err != nil {
		return nil, fmt.Errorf("yamledit: the edited file is not valid YAML: %w", err)
	}
	if k := duplicateKey(nd.Root()); k != nil {
		return nil, fmt.Errorf("yamledit: the edited file is not valid YAML: line %d: %w (%s)", k.Line, ErrDuplicateKey, k.Value)
	}
	return out, nil
}

// duplicateKey returns the second occurrence of a repeated mapping key under n (merge keys may
// repeat), or nil. Aliases are not followed.
func duplicateKey(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Value == "<<" {
				continue
			}
			if seen[k.Value] {
				return k
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if k := duplicateKey(c); k != nil {
			return k
		}
	}
	return nil
}

// bytes returns the file with the queued edits applied, without checking it; Result checks it.
// Untouched lines are byte-identical, inserted lines use EOL, a BOM is kept, and a missing final
// newline stays missing.
func (d *Doc) bytes() []byte {
	if len(d.edits) == 0 {
		return slices.Clone(d.src)
	}
	es := slices.Clone(d.edits)
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].first != es[j].first {
			return es[i].first < es[j].first
		}
		return es[i].insert() && !es[j].insert()
	})
	var texts, ends []string
	cur := 1
	for _, e := range es {
		for ; cur < e.first; cur++ {
			texts, ends = append(texts, d.lines[cur-1]), append(ends, d.ends[cur-1])
		}
		for _, l := range e.lines {
			texts, ends = append(texts, l), append(ends, d.eol)
		}
		if !e.insert() {
			cur = e.last + 1
		}
	}
	for ; cur <= len(d.lines); cur++ {
		texts, ends = append(texts, d.lines[cur-1]), append(ends, d.ends[cur-1])
	}
	finalNewline := len(d.lines) == 0 || d.ends[len(d.ends)-1] != ""
	var b bytes.Buffer
	if d.bom {
		b.WriteString(bom)
	}
	for i, t := range texts {
		end := ends[i]
		switch {
		case i < len(texts)-1 && end == "":
			end = d.eol
		case i == len(texts)-1 && !finalNewline:
			end = ""
		case i == len(texts)-1 && end == "":
			end = d.eol
		}
		b.WriteString(t)
		b.WriteString(end)
	}
	return b.Bytes()
}

// ReplaceScalar replaces scalar n with text, which may span several lines (a block scalar
// rendering, for example from RenderScript). For a scalar on one line, the text from its column
// to its end is replaced and anything after it, such as a trailing comment, is kept; further
// lines of text follow that line. For a block scalar, the header and content lines are replaced,
// keeping a comment after the header. A multi-line flow scalar is replaced up to its entry's end.
// Anchored or tagged scalars are refused.
func (d *Doc) ReplaceScalar(n *yaml.Node, text string) error {
	first, last, lines, err := d.scalarLines(n, text)
	if err != nil {
		return err
	}
	return d.ReplaceLines(first, last, lines)
}

// scalarLines returns the lines ReplaceScalar replaces (first..last) and their replacement.
func (d *Doc) scalarLines(n *yaml.Node, text string) (int, int, []string, error) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return 0, 0, nil, errors.New("yamledit: not a scalar")
	}
	if d.inFlow(n) {
		return 0, 0, nil, ErrFlowStyle
	}
	line, col := n.Line, d.offset(n)
	src := d.Line(line)
	if col >= len(src) {
		return 0, 0, nil, fmt.Errorf("yamledit: line %d: the scalar has no text to replace", line)
	}
	if n.Anchor != "" || src[col] == '&' || src[col] == '!' {
		return 0, 0, nil, fmt.Errorf("yamledit: line %d: an anchored or tagged scalar cannot be replaced", line)
	}
	repl := strings.Split(text, "\n")
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		rest := src[col+len(d.blockHeader(n)):]
		_, last := d.block(n)
		return line, last, append([]string{src[:col] + repl[0] + rest}, repl[1:]...), nil
	}
	if end, ok := d.singleLineEnd(n); ok {
		return line, line, append([]string{src[:col] + repl[0] + src[end:]}, repl[1:]...), nil
	}
	return line, max(d.end(n), line), append([]string{src[:col] + repl[0]}, repl[1:]...), nil
}

// singleLineEnd returns the byte offset after a flow scalar that ends on its first line.
func (d *Doc) singleLineEnd(n *yaml.Node) (int, bool) {
	src, col := d.Line(n.Line), d.offset(n)
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		for j := col + 1; j < len(src); j++ {
			switch src[j] {
			case '\\':
				j++
			case '"':
				return j + 1, true
			}
		}
	case n.Style&yaml.SingleQuotedStyle != 0:
		for j := col + 1; j < len(src); j++ {
			if src[j] == '\'' {
				if j+1 < len(src) && src[j+1] == '\'' {
					j++
					continue
				}
				return j + 1, true
			}
		}
	default:
		rest := src[col:]
		if i := commentStart(rest); i >= 0 {
			rest = rest[:i]
		}
		rest = strings.TrimRight(rest, " \t")
		if rest == n.Value {
			return col + len(rest), true
		}
	}
	return 0, false
}

// blockHeader returns block scalar n's header: the indicator and its chomping and indentation
// indicators ("|", "|-", ">2+").
func (d *Doc) blockHeader(n *yaml.Node) string {
	src := d.Line(n.Line)
	col := d.offset(n)
	h := min(col+1, len(src))
	for h < len(src) && strings.IndexByte("+-0123456789", src[h]) >= 0 {
		h++
	}
	return src[col:h]
}

// block returns block scalar n's content indentation (-1 if it has no content lines) and its
// last line. A non-blank line indented less than the content ends the block, so a comment
// between the key column and the content is not part of it. Trailing blank lines belong to the
// block only when it is keep-chomped (|+).
func (d *Doc) block(n *yaml.Node) (int, int) {
	pi := d.parentIndent(n)
	header := d.blockHeader(n)
	keep := strings.Contains(header, "+")
	ci := -1
	for _, c := range header {
		if c >= '1' && c <= '9' {
			ci = max(pi, 0) + int(c-'0')
		}
	}
	last := n.Line
	for l := n.Line + 1; l <= d.docEnd; l++ {
		t := d.lines[l-1]
		if blank(t) {
			if keep {
				last = l
			}
			continue
		}
		ind := indent(t)
		if ci < 0 {
			if ind <= pi {
				break
			}
			ci = ind
		}
		if ind < ci {
			break
		}
		last = l
	}
	return ci, last
}

// ReplaceScript re-renders script scalar n with value (RenderScript), keeping a block's content
// indentation and using IndentUnit for a scalar that becomes a block.
func (d *Doc) ReplaceScript(n *yaml.Node, value string) error {
	first, last, lines, err := d.ScriptLines(n, value)
	if err != nil {
		return err
	}
	return d.ReplaceLines(first, last, lines)
}

// ScriptLines returns what ReplaceScript would queue, without queuing it: the lines first..last
// it replaces and their replacement. A caller can then change those lines (for example, insert a
// key before a script that shares its line with a sequence dash) and queue them itself.
func (d *Doc) ScriptLines(n *yaml.Node, value string) (first, last int, lines []string, err error) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return 0, 0, nil, errors.New("yamledit: not a scalar")
	}
	keyIndent := max(d.parentIndent(n), 0)
	content := keyIndent + IndentUnit(d)
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		if c, _ := d.block(n); c > keyIndent {
			content = c
		}
	}
	return d.scalarLines(n, strings.Join(RenderScript(value, n.Style, keyIndent, content), "\n"))
}

// SetKey sets key in mapping m to value, which is raw YAML: pass Scalar(v) for a string. An
// existing scalar value is replaced in place (keeping a trailing comment) when value is one line;
// otherwise the whole entry is re-rendered, which is refused if it would remove an anchor. A new
// key is inserted after entry after (after the last entry when after is ""; an error when after
// is missing) at the mapping's key column.
//
// The first line of value goes on the key's line; further lines are indented relative to the key
// column, so "\n  a: 1" renders a nested mapping.
func (d *Doc) SetKey(m *yaml.Node, key, value, after string) error {
	m, err := d.mapping(m)
	if err != nil {
		return err
	}
	kc := m.Content[0].Column - 1
	if i := findKey(m, key); i >= 0 {
		k, v := m.Content[2*i], m.Content[2*i+1]
		if v.Kind == yaml.ScalarNode && !emptyNull(v) && !strings.Contains(value, "\n") {
			return d.ReplaceScalar(v, value)
		}
		if hasAnchor(k) || hasAnchor(v) {
			return fmt.Errorf("%w (%s)", ErrAnchor, key)
		}
		text, err := d.keyText(k)
		if err != nil {
			return err
		}
		return d.ReplaceLines(k.Line, d.entryEnd(m, i), renderEntry(d.Line(k.Line)[:d.offset(k)], text, value, kc))
	}
	at := len(m.Content)/2 - 1
	if after != "" {
		if at = findKey(m, after); at < 0 {
			return fmt.Errorf("yamledit: no key %q to insert %q after", after, key)
		}
	}
	return d.InsertAfter(d.entryEnd(m, at), renderEntry(strings.Repeat(" ", kc), key, value, kc))
}

// keyText returns key node k as written, so a re-rendered entry keeps its quoting: a quoted
// "on" must stay quoted, because YAML 1.1 parsers such as GitHub's read a plain on as true. Only
// a plain or quoted key on its own line (after indentation and sequence dashes) is accepted; an
// explicit (?) or tagged key is refused.
func (d *Doc) keyText(k *yaml.Node) (string, error) {
	src, col := d.Line(k.Line), d.offset(k)
	if strings.Trim(src[:col], " \t-") == "" {
		switch {
		case k.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0:
			if end, ok := d.singleLineEnd(k); ok {
				return src[col:end], nil
			}
		case k.Style == 0 && k.Value != "" && strings.HasPrefix(src[col:], k.Value):
			return k.Value, nil
		}
	}
	return "", fmt.Errorf("yamledit: line %d: the key %q cannot be re-rendered as written", k.Line, k.Value)
}

func renderEntry(prefix, key, value string, kc int) []string {
	parts := strings.Split(value, "\n")
	first := prefix + key + ":"
	if parts[0] != "" {
		first += " " + parts[0]
	}
	out := []string{first}
	for _, p := range parts[1:] {
		if p == "" {
			out = append(out, "")
		} else {
			out = append(out, strings.Repeat(" ", kc)+p)
		}
	}
	return out
}

// DeleteEntry removes key's entry from mapping m; a missing key is not an error. It refuses to
// delete a mapping's only entry (ErrLastEntry), an entry that defines an anchor (ErrAnchor), and
// a key that shares its line with a sequence dash, because each would break the YAML around it.
func (d *Doc) DeleteEntry(m *yaml.Node, key string) error {
	m, err := d.mapping(m)
	if err != nil {
		return err
	}
	i := findKey(m, key)
	if i < 0 {
		return nil
	}
	if len(m.Content) == 2 {
		return fmt.Errorf("%w (%s)", ErrLastEntry, key)
	}
	k, v := m.Content[2*i], m.Content[2*i+1]
	if hasAnchor(k) || hasAnchor(v) {
		return fmt.Errorf("%w (%s)", ErrAnchor, key)
	}
	if strings.TrimSpace(d.Line(k.Line)[:d.offset(k)]) != "" {
		return fmt.Errorf("yamledit: line %d: %s shares its line with a sequence dash", k.Line, key)
	}
	return d.ReplaceLines(k.Line, d.entryEnd(m, i), nil)
}

// InsertItem inserts a new item after item after (-1: before the first item, above its head
// comments). itemLines are the item's lines relative to the content column: the first goes after
// "- ", the others are indented to the content column.
func (d *Doc) InsertItem(s *yaml.Node, after int, itemLines []string) error {
	s, err := d.sequence(s)
	if err != nil {
		return err
	}
	if after < -1 || after >= len(s.Content) {
		return fmt.Errorf("yamledit: no item %d", after)
	}
	dc, cc := d.dashColumn(s), d.contentColumn(s)
	var out []string
	for i, l := range itemLines {
		switch {
		case i == 0:
			out = append(out, strings.Repeat(" ", dc)+"-"+strings.Repeat(" ", max(cc-dc-1, 1))+l)
		case l == "":
			out = append(out, "")
		default:
			out = append(out, strings.Repeat(" ", max(cc, dc+2))+l)
		}
	}
	if after < 0 {
		start, err := d.ItemStart(s, 0)
		if err != nil {
			return err
		}
		return d.InsertBefore(start, out)
	}
	return d.InsertAfter(d.itemEnd(s, after), out)
}

// ---- line helpers ----

func indent(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func isComment(s string) bool { return strings.HasPrefix(strings.TrimLeft(s, " \t"), "#") }

// commentStart returns the index of the whitespace before a " #" comment in s, or -1.
func commentStart(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i - 1
		}
	}
	return -1
}
