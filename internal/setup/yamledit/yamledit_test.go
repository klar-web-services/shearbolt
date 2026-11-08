package yamledit

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

func lines(s ...string) string { return strings.Join(s, "\n") + "\n" }

func mustParse(t *testing.T, src string) *Doc {
	t.Helper()
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func get(t *testing.T, d *Doc, path ...string) *yaml.Node {
	t.Helper()
	n := d.Get(d.Root(), path...)
	if n == nil {
		t.Fatalf("no node at %q", path)
	}
	return n
}

func check(t *testing.T, d *Doc, want string) {
	t.Helper()
	if got := result(t, d); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

var gate = []string{"name: shearbolt", "uses: klar-web-services/shearbolt@v1"}

// 1. Comments survive inserting a step.
func TestInsertItemKeepsComments(t *testing.T) {
	src := lines(
		"jobs:",
		"  build:",
		"    steps:",
		"      - uses: actions/checkout@v5   # pinned by Renovate",
		"      # Build the image",
		"      - name: Build",
		"        run: docker build -t app:${{ github.sha }} .",
		"",
		"      # Ship it",
		"      - name: Push",
		"        run: docker push app:${{ github.sha }}",
	)
	d := mustParse(t, src)
	if err := d.InsertItem(get(t, d, "jobs", "build", "steps"), 1, gate); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines(
		"jobs:",
		"  build:",
		"    steps:",
		"      - uses: actions/checkout@v5   # pinned by Renovate",
		"      # Build the image",
		"      - name: Build",
		"        run: docker build -t app:${{ github.sha }} .",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"",
		"      # Ship it",
		"      - name: Push",
		"        run: docker push app:${{ github.sha }}",
	))
}

// 2. The same with an indentless sequence.
func TestInsertItemIndentless(t *testing.T) {
	src := lines(
		"jobs:",
		"  build:",
		"    steps:",
		"    - uses: actions/checkout@v5   # pinned by Renovate",
		"    # Build the image",
		"    - name: Build",
		"      run: docker build -t app:${{ github.sha }} .",
		"",
		"    # Ship it",
		"    - name: Push",
		"      run: docker push app:${{ github.sha }}",
	)
	d := mustParse(t, src)
	if err := d.InsertItem(get(t, d, "jobs", "build", "steps"), 1, gate); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines(
		"jobs:",
		"  build:",
		"    steps:",
		"    - uses: actions/checkout@v5   # pinned by Renovate",
		"    # Build the image",
		"    - name: Build",
		"      run: docker build -t app:${{ github.sha }} .",
		"    - name: shearbolt",
		"      uses: klar-web-services/shearbolt@v1",
		"",
		"    # Ship it",
		"    - name: Push",
		"      run: docker push app:${{ github.sha }}",
	))
}

func TestInsertItemFirstAndLast(t *testing.T) {
	src := lines(
		"script:",
		"  # build",
		"  - make",
		"  - make push",
		"# trailing",
	)
	d := mustParse(t, src)
	seq := get(t, d, "script")
	if err := d.InsertItem(seq, -1, []string{"echo first"}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertItem(seq, 1, []string{"echo last"}); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines(
		"script:",
		"  - echo first",
		"  # build",
		"  - make",
		"  - make push",
		"  - echo last",
		"# trailing",
	))
}

// 3. (6a) Indentless sequence spans.
func TestIndentlessSequenceSpans(t *testing.T) {
	src := lines(
		"jobs:",
		"  build:",
		"    steps:",
		"    - uses: actions/checkout@v5",
		"    - run: make",
		"  test:",
		"    steps:",
		"    - run: make test",
	)
	d := mustParse(t, src)
	jobs, build := get(t, d, "jobs"), get(t, d, "jobs", "build")
	if l, err := d.EntryEnd(build, "steps"); err != nil || l != 5 {
		t.Fatalf("EntryEnd(build, steps) = %d, %v", l, err)
	}
	if l, err := d.EntryEnd(jobs, "build"); err != nil || l != 5 {
		t.Fatalf("EntryEnd(jobs, build) = %d, %v", l, err)
	}
	if err := d.SetKey(build, "env", "\n  A: b", ""); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines(
		"jobs:",
		"  build:",
		"    steps:",
		"    - uses: actions/checkout@v5",
		"    - run: make",
		"    env:",
		"      A: b",
		"  test:",
		"    steps:",
		"    - run: make test",
	))
}

// 4. (6b) A comment at the previous item's content column stays with that item.
func TestCommentAtContentColumnStaysWithItem(t *testing.T) {
	src := lines(
		"steps:",
		"  - name: Build",
		"    run: make",
		"    # remember to bump the cache key",
		"  - name: Push",
	)
	d := mustParse(t, src)
	steps := get(t, d, "steps")
	if l, err := d.ItemStart(steps, 1); err != nil || l != 5 {
		t.Fatalf("ItemStart(Push) = %d, %v", l, err)
	}
	if l, err := d.ItemEnd(steps, 0); err != nil || l != 4 {
		t.Fatalf("ItemEnd(Build) = %d, %v", l, err)
	}
	d = mustParse(t, strings.Replace(src, "    # remember", "  # remember", 1))
	steps = get(t, d, "steps")
	if l, err := d.ItemStart(steps, 1); err != nil || l != 4 {
		t.Fatalf("ItemStart(Push) with a comment at the dash column = %d, %v", l, err)
	}
	if l, err := d.ItemEnd(steps, 0); err != nil || l != 3 {
		t.Fatalf("ItemEnd(Build) with a comment at the dash column = %d, %v", l, err)
	}
}

const anchored = "definitions:\n" +
	"  steps:\n" +
	"    - step: &build\n" +
	"        name: Build\n" +
	"        script:\n" +
	"          - make\n"

// 5. (6c) Anchored containers.
func TestSetKeyOnAnchoredContainer(t *testing.T) {
	d := mustParse(t, anchored)
	step := get(t, d, "definitions", "steps", "0", "step")
	if c, err := d.KeyColumn(step); err != nil || c != 8 {
		t.Fatalf("KeyColumn = %d, %v", c, err)
	}
	if err := d.SetKey(step, "caches", "[shearbolt]", "name"); err != nil {
		t.Fatal(err)
	}
	check(t, d, strings.Replace(anchored, "        name: Build\n", "        name: Build\n        caches: [shearbolt]\n", 1))
}

// 6. ReplaceScalar keeps a trailing comment.
func TestReplaceScalarKeepsComment(t *testing.T) {
	src := lines(
		"with:",
		"  push: ${{ github.event_name != 'pull_request' }}  # PRs only build",
		"  tags: x",
	)
	d := mustParse(t, src)
	if err := d.ReplaceScalar(get(t, d, "with", "push"), "false"); err != nil {
		t.Fatal(err)
	}
	check(t, d, strings.Replace(src, "${{ github.event_name != 'pull_request' }}", "false", 1))
}

func TestReplaceQuotedScalars(t *testing.T) {
	src := lines(
		"a: 'it''s'   # one",
		"b: \"x \\\" y\" # two",
		"c: plain",
	)
	d := mustParse(t, src)
	for _, k := range []string{"a", "b", "c"} {
		if err := d.ReplaceScalar(get(t, d, k), "v"+k); err != nil {
			t.Fatal(err)
		}
	}
	check(t, d, lines("a: va   # one", "b: vb # two", "c: vc"))
}

// 7. SetKey after a given key.
func TestSetKeyAfter(t *testing.T) {
	src := lines(
		"steps:",
		"  - uses: docker/build-push-action@v6",
		"    with:",
		"      push: true",
		"      tags: x",
	)
	d := mustParse(t, src)
	if err := d.SetKey(get(t, d, "steps", "0", "with"), "load", "true", "push"); err != nil {
		t.Fatal(err)
	}
	check(t, d, strings.Replace(src, "      push: true\n", "      push: true\n      load: true\n", 1))
}

func TestSetKeyReplacesExistingValue(t *testing.T) {
	src := lines(
		"steps:",
		"  - name: x  # keep",
		"    env:",
		"      A: 1",
		"    run: make",
	)
	d := mustParse(t, src)
	step := get(t, d, "steps", "0")
	if err := d.SetKey(step, "name", "y", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.SetKey(step, "env", "\n  B: 2", ""); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines(
		"steps:",
		"  - name: y  # keep",
		"    env:",
		"      B: 2",
		"    run: make",
	))
	d = mustParse(t, src)
	if err := d.SetKey(get(t, d, "steps", "0"), "name", "\n  first: 1", ""); err != nil {
		t.Fatal(err)
	}
	check(t, d, strings.Replace(src, "  - name: x  # keep\n", "  - name:\n      first: 1\n", 1))
}

// A re-rendered entry keeps its key as written, so a quoted "on" (a boolean to YAML 1.1
// parsers such as GitHub's) stays quoted.
func TestSetKeyReRenderKeepsKeyText(t *testing.T) {
	for _, key := range []string{`"on"`, `'on'`, `on`, `"a: b"`, `"x\"y"`} {
		src := lines(key+": push  # trigger", "b: 1")
		d := mustParse(t, src)
		k := d.Root().Content[0].Value
		if err := d.SetKey(d.Root(), k, "\n  push:\n    branches: [main]", ""); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		check(t, d, lines(key+":", "  push:", "    branches: [main]", "b: 1"))
	}
	d := mustParse(t, lines("a: 1", `"on":`, "b: 1"))
	if err := d.SetKey(d.Root(), "on", "\n  push: {}", ""); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines("a: 1", `"on":`, "  push: {}", "b: 1"))
	d = mustParse(t, lines("? on", ": 1", "b: 1"))
	if err := d.SetKey(d.Root(), "on", "\n  push: {}", ""); err == nil {
		t.Fatalf("re-rendering an explicit key: no error\n%s", d.bytes())
	}
}

// Parse refuses anything that is not UTF-8 (yaml.v3 would decode UTF-16, but every position
// and line split here assumes UTF-8).
func TestParseRefusesInvalidUTF8(t *testing.T) {
	utf16 := []byte{0xff, 0xfe, 'a', 0, ':', 0, ' ', 0, '1', 0, '\n', 0}
	for _, src := range [][]byte{utf16, []byte("a: \xff\n"), []byte("\xef\xbb\xbfa: \xc3\n")} {
		if _, err := Parse(src); !errors.Is(err, ErrEncoding) {
			t.Errorf("Parse(%q): err = %v", src, err)
		}
	}
	if _, err := Parse([]byte("\xef\xbb\xbfa: größe\n")); err != nil {
		t.Errorf("UTF-8 with a BOM: %v", err)
	}
}

func TestSetKeyOnEmptyValue(t *testing.T) {
	d := mustParse(t, lines("a:", "b: 1"))
	if err := d.SetKey(d.Root(), "a", "2", ""); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines("a: 2", "b: 1"))
}

func TestDeleteEntry(t *testing.T) {
	src := lines(
		"steps:",
		"  - uses: klar-web-services/shearbolt@v1",
		"    with:",
		"      working-directory: apps/web",
		"      targets: >-",
		"        a",
		"        b",
		"    env:",
		"      X: 1",
	)
	d := mustParse(t, src)
	with := get(t, d, "steps", "0", "with")
	if err := d.DeleteEntry(with, "targets"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteEntry(with, "missing"); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines(
		"steps:",
		"  - uses: klar-web-services/shearbolt@v1",
		"    with:",
		"      working-directory: apps/web",
		"    env:",
		"      X: 1",
	))
	d = mustParse(t, src)
	if err := d.DeleteEntry(get(t, d, "steps", "0", "env"), "X"); !errors.Is(err, ErrLastEntry) {
		t.Fatalf("deleting the only entry: err = %v", err)
	}
	if err := d.DeleteEntry(get(t, d, "steps", "0"), "uses"); err == nil {
		t.Fatal("deleting the key on the dash line: no error")
	}
}

// 8. RenderScript, through ReplaceScript.
func TestReplaceScriptAppendsToBlock(t *testing.T) {
	src := lines(
		"steps:",
		"  - name: Build",
		"    run: |",
		"      docker build -t x .",
		"",
		"      docker images",
		"  - name: Next",
	)
	d := mustParse(t, src)
	run := get(t, d, "steps", "0", "run")
	if err := d.ReplaceScript(run, run.Value+"echo done\n"); err != nil {
		t.Fatal(err)
	}
	check(t, d, strings.Replace(src, "      docker images\n", "      docker images\n      echo done\n", 1))
}

func TestReplaceScriptKeepsIndentationAndStrip(t *testing.T) {
	src := lines(
		"steps:",
		"  - run: |-",
		"        make",
		"        make test",
		"    name: x",
	)
	d := mustParse(t, src)
	run := get(t, d, "steps", "0", "run")
	if err := d.ReplaceScript(run, run.Value+"\nmake push"); err != nil {
		t.Fatal(err)
	}
	check(t, d, strings.Replace(src, "        make test\n", "        make test\n        make push\n", 1))
}

func TestReplaceScriptPlainBecomesBlock(t *testing.T) {
	src := lines(
		"steps:",
		"  - name: Build",
		"    run: docker build -t x .  # image",
		"  - name: Next",
	)
	d := mustParse(t, src)
	run := get(t, d, "steps", "0", "run")
	if err := d.ReplaceScript(run, run.Value+"\necho out\n"); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines(
		"steps:",
		"  - name: Build",
		"    run: |  # image",
		"      docker build -t x .",
		"      echo out",
		"  - name: Next",
	))
}

func TestReplaceScriptDoubleQuotedBecomesBlock(t *testing.T) {
	src := lines(
		"script:",
		`  - "echo \"hi\" | tee x"`,
		"  - make",
	)
	d := mustParse(t, src)
	item := get(t, d, "script", "0")
	if err := d.ReplaceScript(item, item.Value+"\nmake push\n"); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines(
		"script:",
		"  - |",
		`    echo "hi" | tee x`,
		"    make push",
		"  - make",
	))
}

func TestRenderScript(t *testing.T) {
	cases := []struct {
		value   string
		style   yaml.Style
		key, in int
		want    []string
	}{
		{"make", 0, 4, 6, []string{"make"}},
		{"echo a: b", 0, 4, 6, []string{"'echo a: b'"}},
		{"make", yaml.DoubleQuotedStyle, 4, 6, []string{`"make"`}},
		{"it's", yaml.SingleQuotedStyle, 4, 6, []string{`'it''s'`}},
		{"make", yaml.LiteralStyle, 4, 6, []string{"|-", "      make"}},
		{"a\nb\n", 0, 4, 6, []string{"|", "      a", "      b"}},
		{"a\nb", yaml.FoldedStyle, 0, 2, []string{"|-", "  a", "  b"}},
		{"a\n\n", yaml.LiteralStyle, 0, 2, []string{"|+", "  a", ""}},
		{"  indented\nb\n", yaml.LiteralStyle, 2, 4, []string{"|2", "      indented", "    b"}},
		{"bell\a\nx", 0, 0, 2, []string{`"bell\x07\nx"`}},
	}
	for _, c := range cases {
		got := RenderScript(c.value, c.style, c.key, c.in)
		if !slices.Equal(got, c.want) {
			t.Errorf("RenderScript(%q, %d) = %q, want %q", c.value, c.style, got, c.want)
			continue
		}
		doc := strings.Repeat(" ", c.key) + "k: " + strings.Join(got, "\n") + "\n"
		var v map[string]string
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil || v["k"] != c.value {
			t.Errorf("%q renders as %q, which reads back as %q (%v)", c.value, doc, v["k"], err)
		}
	}
}

// 9. Scalar.
func TestScalar(t *testing.T) {
	plain := []string{"(curl -fsSL https://x || wget -qO- https://x) | sh", "-x", "?x", ":x", "a:b", "a#b", "storefront", "${SHEARBOLT_IMAGE:-web:local}", "target/release/demo"}
	for _, s := range plain {
		if got := Scalar(s); got != s {
			t.Errorf("Scalar(%q) = %q, want it plain", s, got)
		}
	}
	quoted := map[string]string{
		`"$HOME/.local/bin/shearbolt" scan --ci`: `'"$HOME/.local/bin/shearbolt" scan --ci'`,
		"@acme/web":                              "'@acme/web'",
		"`x`":                                    "'`x`'",
		"- x":                                    "'- x'",
		"a: b":                                   "'a: b'",
		"true":                                   "'true'",
		"yes":                                    "'yes'",
		"Off":                                    "'Off'",
		"null":                                   "'null'",
		"~":                                      "'~'",
		"1.2.3":                                  "'1.2.3'",
		"0x1F":                                   "'0x1F'",
		"12":                                     "'12'",
		".inf":                                   "'.inf'",
		"":                                       "''",
		"a #b":                                   "'a #b'",
		"ends:":                                  "'ends:'",
		" lead":                                  "' lead'",
		"trail ":                                 "'trail '",
		"it's: x":                                "'it''s: x'",
		"-":                                      "'-'",
		"[x]":                                    "'[x]'",
		"%x":                                     "'%x'",
		"---":                                    "'---'",
		"...":                                    "'...'",
		"--- x":                                  "'--- x'",
		"a\nb":                                   `"a\nb"`,
	}
	for s, want := range quoted {
		got := Scalar(s)
		if got != want {
			t.Errorf("Scalar(%q) = %q, want %q", s, got, want)
		}
		var v map[string]any
		if err := yaml.Unmarshal([]byte("k: "+got+"\n"), &v); err != nil || v["k"] != s {
			t.Errorf("Scalar(%q) = %q reads back as %#v (%v)", s, got, v["k"], err)
		}
	}
}

// 10. CRLF input.
func TestCRLF(t *testing.T) {
	src := "steps:\r\n  - run: make\r\n  - run: make push\r\n"
	d := mustParse(t, src)
	if d.EOL() != "\r\n" {
		t.Fatalf("EOL = %q", d.EOL())
	}
	if err := d.InsertItem(get(t, d, "steps"), 0, gate); err != nil {
		t.Fatal(err)
	}
	want := "steps:\r\n  - run: make\r\n  - name: shearbolt\r\n    uses: klar-web-services/shearbolt@v1\r\n  - run: make push\r\n"
	if got := result(t, d); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 11. Aliases resolve to the anchored node, and edits land at the anchor.
func TestAliasEditsLandAtAnchor(t *testing.T) {
	src := anchored + lines(
		"pipelines:",
		"  default:",
		"    - step: *build",
	)
	d := mustParse(t, src)
	step := get(t, d, "pipelines", "default", "0", "step")
	if step != get(t, d, "definitions", "steps", "0", "step") {
		t.Fatal("the alias does not resolve to the anchored node")
	}
	if err := d.SetKey(step, "caches", "[shearbolt]", "name"); err != nil {
		t.Fatal(err)
	}
	check(t, d, strings.Replace(src, "        name: Build\n", "        name: Build\n        caches: [shearbolt]\n", 1))
}

// 12. Overlaps, round trips, flow style, several documents.
func TestOverlapRoundTripFlowAndMultiDoc(t *testing.T) {
	src := lines("a: 1", "b: 2", "c: 3", "d: 4")
	d := mustParse(t, src)
	if err := d.ReplaceLines(2, 3, []string{"x: 0"}); err != nil {
		t.Fatal(err)
	}
	if err := d.ReplaceLines(3, 4, nil); !errors.Is(err, ErrOverlap) {
		t.Fatalf("overlapping replace: err = %v", err)
	}
	if err := d.InsertBefore(3, []string{"y: 0"}); !errors.Is(err, ErrOverlap) {
		t.Fatalf("insert inside a replaced range: err = %v", err)
	}
	if err := d.InsertBefore(2, []string{"w: 0"}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertAfter(3, []string{"z: 0"}); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines("a: 1", "w: 0", "x: 0", "z: 0", "d: 4"))

	for _, s := range []string{src, "a: 1\r\nb:\r\n  - x # c\r\n", "a: 1\nb: 2", "# only\n", ""} {
		if got := string(mustParse(t, s).bytes()); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}

	d = mustParse(t, lines("steps:", "  - uses: x", "    with: {push: true}", "    flow: [a, b]"))
	with := get(t, d, "steps", "0", "with")
	if err := d.SetKey(with, "load", "true", "push"); !errors.Is(err, ErrFlowStyle) {
		t.Fatalf("SetKey on a flow mapping: err = %v", err)
	}
	if err := d.ReplaceScalar(get(t, d, "steps", "0", "with", "push"), "false"); !errors.Is(err, ErrFlowStyle) {
		t.Fatalf("ReplaceScalar inside a flow mapping: err = %v", err)
	}
	if err := d.InsertItem(get(t, d, "steps", "0", "flow"), 0, []string{"c"}); !errors.Is(err, ErrFlowStyle) {
		t.Fatalf("InsertItem into a flow sequence: err = %v", err)
	}

	for _, s := range []string{"a: 1\n---\nb: 2\n", "a: 1\n---\n"} {
		if _, err := Parse([]byte(s)); !errors.Is(err, ErrMultiDoc) {
			t.Errorf("Parse(%q): err = %v", s, err)
		}
	}
	if _, err := Parse([]byte("---\na: 1\n")); err != nil {
		t.Errorf("a leading --- is one document: %v", err)
	}
	if _, err := Parse([]byte("a: [\n")); err == nil {
		t.Error("invalid YAML: no error")
	}
}

func TestNoFinalNewlineIsKept(t *testing.T) {
	d := mustParse(t, "steps:\n  - run: make")
	if err := d.InsertItem(get(t, d, "steps"), 0, []string{"run: make push"}); err != nil {
		t.Fatal(err)
	}
	if got := result(t, d); got != "steps:\n  - run: make\n  - run: make push" {
		t.Fatalf("got %q", got)
	}
}

func TestGet(t *testing.T) {
	d := mustParse(t, lines("a:", "  - b: 1", "  - c", "x: &x 1", "y: *x"))
	if n := d.Get(d.Root(), "a", "0", "b"); n == nil || n.Value != "1" {
		t.Fatalf("a.0.b = %+v", n)
	}
	if n := d.Get(d.Root(), "a", "1"); n == nil || n.Value != "c" {
		t.Fatalf("a.1 = %+v", n)
	}
	if n := d.Get(d.Root(), "y"); n == nil || n.Value != "1" || n.Kind != yaml.ScalarNode {
		t.Fatalf("y = %+v", n)
	}
	for _, p := range [][]string{{"a", "2"}, {"a", "x"}, {"nope"}, {"x", "deeper"}} {
		if n := d.Get(d.Root(), p...); n != nil {
			t.Errorf("Get(%q) = %+v, want nil", p, n)
		}
	}
	if d.Get(nil, "a") != nil {
		t.Error("Get(nil) is not nil")
	}
}

func TestReplaceScalarRefusesAnchorsAndTags(t *testing.T) {
	d := mustParse(t, lines("a: &x 1", "b: !!str 2"))
	for _, k := range []string{"a", "b"} {
		if err := d.ReplaceScalar(get(t, d, k), "3"); err == nil {
			t.Errorf("ReplaceScalar(%s): no error", k)
		}
	}
}

func TestIndentUnit(t *testing.T) {
	cases := map[string]int{
		lines("a:", "    b: 1", "    c:", "        - x"):         4,
		lines("jobs:", "  build:", "    steps:", "    - run: x"): 2,
		lines("a: 1"):                      2,
		lines("a:", "   b:", "      c: 1"): 3,
	}
	for src, want := range cases {
		if got := IndentUnit(mustParse(t, src)); got != want {
			t.Errorf("IndentUnit(%q) = %d, want %d", src, got, want)
		}
	}
}

func TestColumns(t *testing.T) {
	d := mustParse(t, lines("steps:", "  - &a", "    name: x", "  - name: y"))
	steps := get(t, d, "steps")
	if c, err := d.DashColumn(steps); err != nil || c != 2 {
		t.Fatalf("DashColumn = %d, %v", c, err)
	}
	if c, err := d.ContentColumn(steps); err != nil || c != 4 {
		t.Fatalf("ContentColumn = %d, %v", c, err)
	}
	if l, err := d.ItemStart(steps, 0); err != nil || l != 2 {
		t.Fatalf("ItemStart(0) = %d, %v", l, err)
	}
	if l, err := d.ItemEnd(steps, 0); err != nil || l != 3 {
		t.Fatalf("ItemEnd(0) = %d, %v", l, err)
	}
}

func FuzzScalarRoundTrip(f *testing.F) {
	for _, s := range []string{"", "a: b", "- x", "true", "1.2", "'q'", "\"$HOME\" x", "a #b", "x\ny", " t", "@a", "\x01", "${{ a }}", "\u008e", "\u0085", "\u2028", "\ufeffx"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return
		}
		out := Scalar(s)
		var v map[string]any
		if err := yaml.Unmarshal([]byte("k: "+out+"\n"), &v); err != nil || v["k"] != s {
			t.Fatalf("Scalar(%q) = %q reads back as %#v (%v)", s, out, v["k"], err)
		}
	})
}

func FuzzRenderScriptRoundTrip(f *testing.F) {
	f.Add("docker build .\necho x\n", 4, uint8(0))
	f.Add("  lead\n\nx", 2, uint8(1))
	f.Add("a\n\n\n", 0, uint8(2))
	f.Add("\n", 0, uint8(2))
	f.Add("\t", 3, uint8('6'))
	f.Fuzz(func(t *testing.T, value string, key int, style uint8) {
		if !utf8.ValidString(value) || key < 0 || key > 12 {
			return
		}
		st := []yaml.Style{0, yaml.LiteralStyle, yaml.DoubleQuotedStyle, yaml.SingleQuotedStyle, yaml.FoldedStyle}[int(style)%5]
		got := RenderScript(value, st, key, key+2)
		doc := strings.Repeat(" ", key) + "k: " + strings.Join(got, "\n") + "\n"
		var v map[string]any
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("RenderScript(%q) = %q is not valid YAML: %v", value, doc, err)
		}
		if s, _ := v["k"].(string); s != value && !(value == "" && v["k"] == nil) {
			t.Fatalf("RenderScript(%q) = %q reads back as %#v", value, doc, v["k"])
		}
	})
}

func FuzzReplaceScript(f *testing.F) {
	docs := []struct {
		src       string
		path      []string
		neighbors map[string][]string // expected value -> path
	}{
		{"steps:\n  - name: a\n    run: make  # c\n    env:\n      X: 1\n  - name: b\n", []string{"steps", "0", "run"},
			map[string][]string{"1": {"steps", "0", "env", "X"}, "b": {"steps", "1", "name"}}},
		{"script:\n  - make\n  - |\n    a\n    b\n  - last\n", []string{"script", "1"},
			map[string][]string{"make": {"script", "0"}, "last": {"script", "2"}}},
		{"jobs:\n  t:\n    steps:\n    - run: |-\n        x\n\n        y\n      name: n\n", []string{"jobs", "t", "steps", "0", "run"},
			map[string][]string{"n": {"jobs", "t", "steps", "0", "name"}}},
		{"a:\n  run: \"q\"\nb: 2\n", []string{"a", "run"}, map[string][]string{"2": {"b"}}},
		{"s:\n- 'x' # c\n- y\n", []string{"s", "0"}, map[string][]string{"y": {"s", "1"}}},
		{"k: |+\n  a\n\nz: 1\n", []string{"k"}, map[string][]string{"1": {"z"}}},
	}
	for i := range docs {
		f.Add(uint8(i), "docker build .\necho x\n")
		f.Add(uint8(i), "make")
	}
	f.Fuzz(func(t *testing.T, which uint8, value string) {
		if !utf8.ValidString(value) {
			return
		}
		c := docs[int(which)%len(docs)]
		d, err := Parse([]byte(c.src))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.ReplaceScript(d.Get(d.Root(), c.path...), value); err != nil {
			t.Fatal(err)
		}
		out := d.bytes()
		r, err := Parse(out)
		if err != nil {
			t.Fatalf("result does not parse: %v\n%s", err, out)
		}
		if n := r.Get(r.Root(), c.path...); n == nil || n.Value != value {
			t.Fatalf("script reads back as %+v, want %q\n%s", n, value, out)
		}
		for want, p := range c.neighbors {
			if n := r.Get(r.Root(), p...); n == nil || n.Value != want {
				t.Fatalf("%q changed to %+v\n%s", p, n, out)
			}
		}
	})
}

// result returns Result() and fails the test on error.
func result(t *testing.T, d *Doc) string {
	t.Helper()
	out, err := d.Result()
	if err != nil {
		t.Fatalf("Result: %v\n%s", err, d.bytes())
	}
	return string(out)
}

// C1(a): yaml.v3 columns count runes; edits must slice bytes at the right place.
func TestNonASCIIColumns(t *testing.T) {
	d := mustParse(t, "env:\n  größe: alt  # c\n  日本: \"x\" # q\n  😀: |\n    a\n")
	if err := d.ReplaceScalar(get(t, d, "env", "größe"), "neu"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetKey(get(t, d, "env"), "日本", "y", ""); err != nil {
		t.Fatal(err)
	}
	e := get(t, d, "env", "😀")
	if err := d.ReplaceScript(e, e.Value+"b\n"); err != nil {
		t.Fatal(err)
	}
	if got, want := result(t, d), "env:\n  größe: neu  # c\n  日本: y # q\n  😀: |\n    a\n    b\n"; got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	d = mustParse(t, "steps:\n  - name: Bäuen\n    with:\n      push: true\n")
	if err := d.SetKey(get(t, d, "steps", "0", "with"), "push", "false", ""); err != nil {
		t.Fatal(err)
	}
	if got, want := result(t, d), "steps:\n  - name: Bäuen\n    with:\n      push: false\n"; got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// C1(b): yaml.v3 skips a leading BOM; positions on line 1 must still match, and the BOM stays.
func TestByteOrderMark(t *testing.T) {
	bom := "\xef\xbb\xbf"
	src := bom + "push: true  # c\nb: 2\n"
	d := mustParse(t, src)
	if string(d.bytes()) != src {
		t.Fatal("round trip lost the BOM")
	}
	if err := d.ReplaceScalar(get(t, d, "push"), "false"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetKey(d.Root(), "c", "3", ""); err != nil {
		t.Fatal(err)
	}
	if got, want := result(t, d), bom+"push: false  # c\nb: 2\nc: 3\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// C1(c): yaml.v3 counts these as line breaks, which would shift every line after them.
func TestUnusualLineBreaksAreRefused(t *testing.T) {
	step := "steps:\n  - uses: docker/build-push-action@v6  # note%smore\n    with:\n      push: true\n"
	for _, br := range []string{"\r", "\xc2\x85", "\xe2\x80\xa8", "\xe2\x80\xa9"} {
		if _, err := Parse([]byte(strings.Replace(step, "%s", br, 1))); !errors.Is(err, ErrLineBreak) {
			t.Errorf("line break %q: err = %v", br, err)
		}
	}
	if _, err := Parse([]byte(strings.ReplaceAll(strings.Replace(step, "%s", " ", 1), "\n", "\r\n"))); err != nil {
		t.Errorf("CRLF: %v", err)
	}
}

// I2: a re-render or delete must not remove an anchor, and Result catches broken output.
func TestAnchorsAreNotRemoved(t *testing.T) {
	src := lines(
		"steps:",
		"  - name: x",
		"    env: &e",
		"      A: 1",
		"    with:",
		"      deep:",
		"        b: &x 1",
		"      c: 2",
		"  - env: *e",
	)
	d := mustParse(t, src)
	step := get(t, d, "steps", "0")
	if err := d.SetKey(step, "env", "\n  B: 2", ""); !errors.Is(err, ErrAnchor) {
		t.Errorf("SetKey re-render: err = %v", err)
	}
	if err := d.DeleteEntry(step, "env"); !errors.Is(err, ErrAnchor) {
		t.Errorf("DeleteEntry: err = %v", err)
	}
	if err := d.DeleteEntry(get(t, d, "steps", "0", "with"), "deep"); !errors.Is(err, ErrAnchor) {
		t.Errorf("DeleteEntry of a nested anchor: err = %v", err)
	}
	if err := d.DeleteEntry(get(t, d, "steps", "0", "with"), "c"); err != nil {
		t.Errorf("DeleteEntry without anchors: %v", err)
	}
	if _, err := d.Result(); err != nil {
		t.Fatal(err)
	}
}

func TestResultRefusesBrokenOutput(t *testing.T) {
	cases := map[string]func(d *Doc) error{
		"invalid":       func(d *Doc) error { return d.ReplaceLines(1, 1, []string{"a: ["}) },
		"unknown alias": func(d *Doc) error { return d.ReplaceLines(1, 1, []string{"a: 1"}) },
		"two documents": func(d *Doc) error { return d.InsertAfter(2, []string{"---", "c: 3"}) },
	}
	for name, edit := range cases {
		d := mustParse(t, lines("a: &x 1", "b: *x"))
		if err := edit(d); err != nil {
			t.Fatal(err)
		}
		if out, err := d.Result(); err == nil {
			t.Errorf("%s: no error for %q", name, out)
		}
	}
}

// I3: a keep-chomped block owns its trailing blank lines; nothing is inserted before them.
func TestKeepBlockKeepsTrailingLines(t *testing.T) {
	d := mustParse(t, lines("a: |+", "  x", "", "b: 1"))
	if err := d.SetKey(d.Root(), "c", "2", "a"); err != nil {
		t.Fatal(err)
	}
	out := result(t, d)
	if want := lines("a: |+", "  x", "", "c: 2", "b: 1"); out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if v := get(t, mustParse(t, out), "a").Value; v != "x\n\n" {
		t.Fatalf("a = %q", v)
	}

	d = mustParse(t, lines("s:", "  - |+", "    x", "", "  - y"))
	if err := d.InsertItem(get(t, d, "s"), 0, []string{"z"}); err != nil {
		t.Fatal(err)
	}
	out = result(t, d)
	if want := lines("s:", "  - |+", "    x", "", "  - z", "  - y"); out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if v := get(t, mustParse(t, out), "s", "0").Value; v != "x\n\n" {
		t.Fatalf("s.0 = %q", v)
	}

	d = mustParse(t, lines("jobs:", "  t:", "    run: |+", "      x", "", "  # c", "other: 1"))
	if err := d.SetKey(get(t, d, "jobs", "t"), "if", "true", ""); err != nil {
		t.Fatal(err)
	}
	if v := get(t, mustParse(t, result(t, d)), "jobs", "t", "run").Value; v != "x\n\n" {
		t.Fatalf("run = %q", v)
	}
}

// Minor 1: nothing is inserted after a document end marker.
func TestDocumentEndMarker(t *testing.T) {
	for _, tail := range []string{"...\n", "... # end\n# c\n"} {
		d := mustParse(t, "a: 1\n"+tail)
		if err := d.SetKey(d.Root(), "b", "2", ""); err != nil {
			t.Fatal(err)
		}
		if got, want := result(t, d), "a: 1\nb: 2\n"+tail; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// Minor 2: a null first item (a dash at the end of its line).
func TestNullFirstItem(t *testing.T) {
	d := mustParse(t, lines("steps:", "  -", "  - run: x"))
	steps := get(t, d, "steps")
	if c, err := d.DashColumn(steps); err != nil || c != 2 {
		t.Fatalf("DashColumn = %d, %v", c, err)
	}
	if c, err := d.ContentColumn(steps); err != nil || c != 4 {
		t.Fatalf("ContentColumn = %d, %v", c, err)
	}
	if err := d.InsertItem(steps, -1, []string{"name: a", "run: b"}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertItem(steps, 1, []string{"run: y"}); err != nil {
		t.Fatal(err)
	}
	if got, want := result(t, d), lines("steps:", "  - name: a", "    run: b", "  -", "  - run: x", "  - run: y"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Minor 3: a comment between the key column and the block's content is not part of the block.
func TestBlockEndsAtContentIndentation(t *testing.T) {
	src := lines("steps:", "  - run: |", "      make", "     # between", "    name: x")
	d := mustParse(t, src)
	run := get(t, d, "steps", "0", "run")
	if err := d.ReplaceScript(run, run.Value+"make test\n"); err != nil {
		t.Fatal(err)
	}
	if got, want := result(t, d), lines("steps:", "  - run: |", "      make", "      make test", "     # between", "    name: x"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Minor 8: a missing `after` key is an error, not a silent append.
func TestSetKeyMissingAfter(t *testing.T) {
	d := mustParse(t, lines("a: 1"))
	if err := d.SetKey(d.Root(), "b", "2", "nope"); err == nil {
		t.Fatal("no error")
	}
}

// leaves maps the path of every scalar under n (aliases not followed) to its value.
func leaves(n *yaml.Node, path string, out map[string]string) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			leaves(n.Content[i+1], path+"/"+n.Content[i].Value, out)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			leaves(c, path+"/"+strconv.Itoa(i), out)
		}
	case yaml.AliasNode:
		out[path] = "*" + n.Value
	default:
		out[path] = n.Value
	}
}

// containers lists the paths of the block mappings and sequences under n.
func containers(n *yaml.Node, path []string, out *[][]string) {
	if n.Kind != yaml.MappingNode && n.Kind != yaml.SequenceNode || n.Style&yaml.FlowStyle != 0 {
		return
	}
	*out = append(*out, slices.Clone(path))
	for i, c := range n.Content {
		switch {
		case n.Kind == yaml.MappingNode && i%2 == 1:
			containers(c, append(path, n.Content[i-1].Value), out)
		case n.Kind == yaml.SequenceNode:
			containers(c, append(path, strconv.Itoa(i)), out)
		}
	}
}

// FuzzEdits applies one structural edit to documents with BOM, CRLF and non-ASCII content and
// checks that Result parses, the edit took effect, and nothing outside the edited container moved.
func FuzzEdits(f *testing.F) {
	docs := []string{
		"jobs:\n  bäu:\n    steps:\n      - name: Prüfen # c\n        run: make\n      - uses: x/y@v1\n        with:\n          größe: 1\n          tags: a\n    env:\n      日本: x\n",
		"steps:\n- run: |+\n    a\n\n- name: 😀  # é\n  if: always()\ndone: true\n...\n",
		"a:\n  -\n  - b: &x 1\n    c: 2\n  - *x\nz: \"q\" # k\n",
		"# head\nlist:\n  # first\n  - 'it''s'\n  - \"ü\"\n\nmap:\n    k: v   # c\n    k2: |-\n        x\n         # deeper\n",
	}
	for i := range docs {
		for op := range 4 {
			f.Add(uint8(i), uint8(op), uint8(1), i%2 == 0, i%3 == 0, "wert ü")
		}
	}
	f.Fuzz(func(t *testing.T, which, op, pick uint8, withBOM, crlf bool, value string) {
		if !utf8.ValidString(value) {
			return
		}
		src := docs[int(which)%len(docs)]
		if crlf {
			src = strings.ReplaceAll(src, "\n", "\r\n")
		}
		if withBOM {
			src = "\xef\xbb\xbf" + src
		}
		d, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		var conts [][]string
		containers(d.Root(), nil, &conts)
		path := conts[int(pick)%len(conts)]
		c := d.Get(d.Root(), path...)
		prefix := "/" + strings.Join(path, "/")
		if len(path) == 0 {
			prefix = ""
		}
		before := map[string]string{}
		leaves(d.Root(), "", before)

		var check func(r *yaml.Node)
		switch {
		case c.Kind == yaml.SequenceNode:
			after := int(op)%(len(c.Content)+1) - 1
			if err := d.InsertItem(c, after, []string{"name: " + Scalar(value)}); err != nil {
				t.Fatal(err)
			}
			check = func(r *yaml.Node) {
				if n := d2get(r, append(path, strconv.Itoa(after+1), "name")...); n == nil || n.Value != value {
					t.Fatalf("inserted item reads back as %+v", n)
				}
				for k, v := range before {
					if rest, ok := strings.CutPrefix(k, prefix+"/"); ok {
						i, tail, _ := strings.Cut(rest, "/")
						n, _ := strconv.Atoi(i)
						if n > after {
							n++
						}
						k = prefix + "/" + strconv.Itoa(n)
						if tail != "" {
							k += "/" + tail
						}
					}
					expect(t, r, k, v)
				}
			}
		case op%3 == 0:
			if err := d.SetKey(c, "neu_ä", Scalar(value), ""); err != nil {
				t.Fatal(err)
			}
			check = func(r *yaml.Node) {
				if n := d2get(r, append(path, "neu_ä")...); n == nil || n.Value != value {
					t.Fatalf("new key reads back as %+v", n)
				}
				for k, v := range before {
					expect(t, r, k, v)
				}
			}
		default:
			key := c.Content[2*(int(pick)%(len(c.Content)/2))].Value
			edited := prefix + "/" + key
			if op%3 == 1 {
				err = d.SetKey(c, key, Scalar(value), "")
			} else {
				err = d.DeleteEntry(c, key)
			}
			if errors.Is(err, ErrAnchor) || errors.Is(err, ErrLastEntry) || err != nil && strings.Contains(err.Error(), "anchored or tagged") || err != nil && strings.Contains(err.Error(), "sequence dash") {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			check = func(r *yaml.Node) {
				n := d2get(r, append(path, key)...)
				if op%3 == 1 && (n == nil || n.Value != value) || op%3 == 2 && n != nil {
					t.Fatalf("edited key %q reads back as %+v", key, n)
				}
				for k, v := range before {
					if k != edited && !strings.HasPrefix(k, edited+"/") {
						expect(t, r, k, v)
					}
				}
			}
		}
		out, err := d.Result()
		if err != nil {
			t.Fatalf("%v\n%q", err, d.bytes())
		}
		if withBOM != strings.HasPrefix(string(out), "\xef\xbb\xbf") {
			t.Fatal("BOM changed")
		}
		r, err := Parse(out)
		if err != nil {
			t.Fatal(err)
		}
		check(r.Root())
	})
}

func d2get(r *yaml.Node, path ...string) *yaml.Node { return (&Doc{}).Get(r, path...) }

func expect(t *testing.T, r *yaml.Node, path, want string) {
	t.Helper()
	got := map[string]string{}
	leaves(r, "", got)
	if v, ok := got[path]; !ok || v != want {
		t.Fatalf("%s = %q (present %v), want %q", path, v, ok, want)
	}
}

func TestCheckpointRollback(t *testing.T) {
	src := lines("a: 1", "b: 2", "c: 3")
	d := mustParse(t, src)
	if err := d.SetKey(d.Root(), "a", "10", ""); err != nil {
		t.Fatal(err)
	}
	cp := d.Checkpoint()
	if err := d.SetKey(d.Root(), "b", "20", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertAfter(3, []string{"d: 4"}); err != nil {
		t.Fatal(err)
	}
	d.Rollback(cp)
	check(t, d, lines("a: 10", "b: 2", "c: 3"))
	// The rolled-back lines can be edited again.
	if err := d.SetKey(d.Root(), "b", "21", ""); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines("a: 10", "b: 21", "c: 3"))
	d.Rollback(0)
	check(t, d, src)
}

func TestScriptLines(t *testing.T) {
	src := lines(
		"steps:",
		"  - run: docker build .  # image",
		"  - name: x",
		"    run: |",
		"      make",
		"    env: {}",
	)
	d := mustParse(t, src)
	n := get(t, d, "steps", "0", "run")
	first, last, got, err := d.ScriptLines(n, "docker build .\necho x\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"  - run: |  # image", "      docker build .", "      echo x"}
	if first != 2 || last != 2 || !slices.Equal(got, want) {
		t.Fatalf("ScriptLines = %d-%d %q, want 2-2 %q", first, last, got, want)
	}
	n = get(t, d, "steps", "1", "run")
	first, last, got, err = d.ScriptLines(n, "make\nmake test\n")
	want = []string{"    run: |", "      make", "      make test"}
	if err != nil || first != 4 || last != 5 || !slices.Equal(got, want) {
		t.Fatalf("ScriptLines = %d-%d %q (%v), want 4-5 %q", first, last, got, err, want)
	}
	// ScriptLines queues nothing; ReplaceScript queues the same lines.
	check(t, d, src)
	if err := d.ReplaceScript(n, "make\nmake test\n"); err != nil {
		t.Fatal(err)
	}
	check(t, d, lines("steps:", "  - run: docker build .  # image", "  - name: x", "    run: |", "      make", "      make test", "    env: {}"))
}

// GitHub and config.Load reject duplicate mapping keys, which yaml.v3 accepts in a Node, so Result
// refuses an edit that creates one. Merge keys may repeat.
func TestResultRefusesDuplicateKeys(t *testing.T) {
	d := mustParse(t, lines("step:", "  id: a", "  name: x"))
	if err := d.InsertAfter(3, []string{"  id: b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Result(); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("Result = %v, want ErrDuplicateKey", err)
	}
	d = mustParse(t, lines("a: &a {x: 1}", "b: &b {y: 1}", "c:", "  <<: *a", "  <<: *b", "  z: 1"))
	if err := d.SetKey(get(t, d, "c"), "w", "2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Result(); err != nil {
		t.Fatalf("repeated merge keys: %v", err)
	}
	// A file without edits comes back as it is.
	d = mustParse(t, lines("a: 1", "a: 2"))
	if out, err := d.Result(); err != nil || string(out) != lines("a: 1", "a: 2") {
		t.Fatalf("unedited: %q, %v", out, err)
	}
}
