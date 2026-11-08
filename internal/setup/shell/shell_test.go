package shell

import (
	"slices"
	"strings"
	"testing"
)

func raws(c Command) []string {
	var out []string
	for _, w := range c.Words {
		out = append(out, w.Raw)
	}
	return out
}

func values(c Command) []string {
	var out []string
	for _, w := range c.Words {
		out = append(out, w.Value)
	}
	return out
}

func parseOK(t *testing.T, script string, start State) []Command {
	t.Helper()
	cmds, _ := Parse(script, start)
	for _, c := range cmds {
		for _, w := range c.Words {
			if script[w.Start:w.Start+len(w.Raw)] != w.Raw {
				t.Fatalf("word %q has Start %d, but the script there reads %q", w.Raw, w.Start, script[w.Start:w.Start+len(w.Raw)])
			}
		}
		if c.Start > c.End || c.End > len(script) {
			t.Fatalf("command %q has bad offsets %d..%d", raws(c), c.Start, c.End)
		}
	}
	return cmds
}

func TestQuotedExpressionTagAndOperator(t *testing.T) {
	script := `docker build -t "$IMAGE:${{ github.sha }}" . && docker push "$IMAGE"`
	cmds := parseOK(t, script, State{Dir: "."})
	if len(cmds) != 2 {
		t.Fatalf("got %d commands: %+v", len(cmds), cmds)
	}
	if cmds[0].Op != "&&" || cmds[1].Op != "" {
		t.Fatalf("ops = %q, %q", cmds[0].Op, cmds[1].Op)
	}
	want := []string{"docker", "build", "-t", `"$IMAGE:${{ github.sha }}"`, "."}
	if !slices.Equal(raws(cmds[0]), want) {
		t.Fatalf("words = %q, want %q", raws(cmds[0]), want)
	}
	tag := cmds[0].Words[3]
	if tag.Value != "$IMAGE:${{ github.sha }}" || tag.Start != strings.Index(script, `"$IMAGE`) {
		t.Fatalf("tag = %+v", tag)
	}
	if cmds[0].End != strings.Index(script, " &&") {
		t.Fatalf("End = %d", cmds[0].End)
	}
	if !slices.Equal(values(cmds[1]), []string{"docker", "push", "$IMAGE"}) {
		t.Fatalf("push words = %q", values(cmds[1]))
	}
}

func TestUnquotedExpressionIsOneWordPart(t *testing.T) {
	cmds := parseOK(t, `docker build -t app:${{ github.sha }} .`, State{Dir: "."})
	if len(cmds) != 1 || len(cmds[0].Words) != 5 { // docker, build, -t, the tag, .
		t.Fatalf("commands = %+v", cmds)
	}
	if cmds[0].Words[3].Value != "app:${{ github.sha }}" {
		t.Fatalf("tag = %+v", cmds[0].Words[3])
	}
}

func TestExpressionOperatorsSplitNothing(t *testing.T) {
	cmds := parseOK(t, `echo ${{ a && 'x' || 'y' }}`, State{Dir: "."})
	if len(cmds) != 1 || len(cmds[0].Words) != 2 {
		t.Fatalf("commands = %+v", cmds)
	}
	if cmds[0].Words[1].Value != "${{ a && 'x' || 'y' }}" {
		t.Fatalf("word = %+v", cmds[0].Words[1])
	}
}

func TestExpressionInsideSingleQuotesIsOpaque(t *testing.T) {
	cmds := parseOK(t, `echo '${{ github.ref == 'refs/heads/main' }}' && make`, State{Dir: "."})
	if len(cmds) != 2 || cmds[0].Words[1].Value != "${{ github.ref == 'refs/heads/main' }}" {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestContinuationInsideCommand(t *testing.T) {
	script := "docker build \\\n  -t x \\\n  .\ndocker push x"
	cmds := parseOK(t, script, State{Dir: "."})
	if len(cmds) != 2 {
		t.Fatalf("got %d commands: %+v", len(cmds), cmds)
	}
	build := cmds[0]
	if !slices.Equal(values(build), []string{"docker", "build", "-t", "x", "."}) {
		t.Fatalf("words = %q", values(build))
	}
	if line := strings.Count(script[:build.End], "\n") + 1; line != 3 || script[build.End-1] != '.' {
		t.Fatalf("End %d is on line %d", build.End, line)
	}
	if build.ContAfter || build.Op != "\n" {
		t.Fatalf("ContAfter = %v, Op = %q", build.ContAfter, build.Op)
	}
}

func TestContinuationAfterOperator(t *testing.T) {
	cmds := parseOK(t, "docker build . && \\\n  docker push x", State{Dir: "."})
	if len(cmds) != 2 {
		t.Fatalf("got %d commands: %+v", len(cmds), cmds)
	}
	if cmds[0].Op != "&&" || !cmds[0].ContAfter || cmds[1].ContAfter {
		t.Fatalf("first = %+v, second ContAfter = %v", cmds[0], cmds[1].ContAfter)
	}
}

func TestContinuationBeforeOperator(t *testing.T) {
	cmds := parseOK(t, "docker build . \\\n  && docker push x", State{Dir: "."})
	if len(cmds) != 2 || cmds[0].Op != "&&" || !cmds[0].ContAfter {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestContinuationJoinsWordParts(t *testing.T) {
	cmds := parseOK(t, "echo ab\\\ncd", State{Dir: "."})
	if len(cmds) != 1 || len(cmds[0].Words) != 2 || cmds[0].Words[1].Value != "abcd" || cmds[0].Words[1].Raw != "ab\\\ncd" {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestCdChangesDirectory(t *testing.T) {
	cmds := parseOK(t, "cd apps/web && npm run build", State{Dir: "."})
	if len(cmds) != 2 || cmds[0].Before.Dir != "." || cmds[1].Before.Dir != "apps/web" {
		t.Fatalf("commands = %+v", cmds)
	}
	cmds = parseOK(t, `cd "$X"; make`, State{Dir: "."})
	if len(cmds) != 2 || cmds[1].Before.Dir != "" {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestCdVariants(t *testing.T) {
	cases := []struct {
		script, start, want string
	}{
		{"cd -; make", ".", ""},
		{"cd; make", ".", ""},
		{"cd /tmp; make", ".", ""},
		{"cd ~/x; make", ".", ""},
		{"cd ..; make", ".", ""},
		{"cd ..; make", "apps/web", "apps"},
		{"cd -P 'my app'; make", ".", "my app"},
		{"cd -- web; make", ".", "web"},
		{"cd web/*; make", ".", ""},
		{"cd `pwd`; make", ".", ""},
		{"cd web; make", "", ""},
		{"cd a b; make", ".", ""},
		{"cd web | cat; make", ".", "."},
		{"(cd web && make); make", ".", "."},
		{"test -d web || cd web; make", ".", ""},
		{"test -d web && cd web; make", ".", "web"},
		{"if [ -d web ]; then cd web; fi; make", ".", ""},
		{"{ cd web; }; make", ".", "web"},
		{"pushd web; popd; make", ".", "."},
		{"popd; make", ".", ""},
		{"f() { cd web; }; make", ".", ""},
		{"echo | cd web; make", ".", "."},
	}
	for _, c := range cases {
		cmds := parseOK(t, c.script, State{Dir: c.start})
		last := cmds[len(cmds)-1]
		if !slices.Equal(values(last), []string{"make"}) || last.Before.Dir != c.want {
			t.Errorf("%q from %q: last = %q in %q, want %q", c.script, c.start, values(last), last.Before.Dir, c.want)
		}
	}
}

func TestExportCarriesIntoNextParse(t *testing.T) {
	cmds, end := Parse("export CARGO_TARGET_DIR=/tmp/t\ncargo build", State{Dir: "."})
	if len(cmds) != 2 || len(cmds[0].Before.Env) != 0 || !slices.Equal(cmds[1].Before.Env, []string{"CARGO_TARGET_DIR"}) {
		t.Fatalf("commands = %+v", cmds)
	}
	next, _ := Parse("cargo build --release", end)
	if len(next) != 1 || !slices.Equal(next[0].Before.Env, []string{"CARGO_TARGET_DIR"}) {
		t.Fatalf("next = %+v", next)
	}
}

func TestBareAssignmentAddsVariable(t *testing.T) {
	cmds, end := Parse("A=1 B=\"x y\"\nC=2 make\nexport D E=3\nmake", State{Dir: "."})
	if !slices.Equal(end.Env, []string{"A", "B", "D", "E"}) {
		t.Fatalf("end env = %q", end.Env)
	}
	if !slices.Equal(cmds[1].Before.Env, []string{"A", "B"}) || !slices.Equal(cmds[1].Prefix(), []string{"C"}) {
		t.Fatalf("second = %+v", cmds[1])
	}
}

func TestStartStateIsNotModified(t *testing.T) {
	start := State{Dir: ".", Env: make([]string, 1, 8)}
	start.Env[0] = "X"
	cmds, end := Parse("export Y=1; export Z=2; make", start)
	if !slices.Equal(start.Env, []string{"X"}) || !slices.Equal(cmds[1].Before.Env, []string{"X", "Y"}) || !slices.Equal(end.Env, []string{"X", "Y", "Z"}) {
		t.Fatalf("start = %q, second = %q, end = %q", start.Env, cmds[1].Before.Env, end.Env)
	}
}

func TestComment(t *testing.T) {
	cmds := parseOK(t, `echo "a # b" # trailing`, State{Dir: "."})
	if len(cmds) != 1 || len(cmds[0].Words) != 2 || cmds[0].Words[1].Value != "a # b" {
		t.Fatalf("commands = %+v", cmds)
	}
	cmds = parseOK(t, "# only a comment \\\nmake a#b", State{Dir: "."})
	if len(cmds) != 1 || !slices.Equal(values(cmds[0]), []string{"make", "a#b"}) {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestIfDepth(t *testing.T) {
	cmds := parseOK(t, "if [ -n \"$X\" ]; then\n  docker build .\nfi", State{Dir: "."})
	var build, fi *Command
	for i := range cmds {
		switch cmds[i].Words[0].Value {
		case "docker":
			build = &cmds[i]
		case "fi":
			fi = &cmds[i]
		}
	}
	if build == nil || fi == nil || build.Depth != 1 || fi.Depth != 0 {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestDepthConstructs(t *testing.T) {
	script := strings.Join([]string{
		"for f in a b; do",
		"  docker push $f",
		"done",
		"while read -r l; do echo $l; done < list",
		"case $X in",
		"  a|b) docker push a ;;",
		"  (c) docker push c",
		"esac",
		"{ docker push d; }",
		"( docker push e )",
		"f() {",
		"  docker push f",
		"}",
		"! docker push g",
		"docker push h",
	}, "\n")
	cmds := parseOK(t, script, State{Dir: "."})
	depth := map[string]int{}
	for _, c := range cmds {
		if a := c.Argv(); len(a) == 3 && a[0] == "docker" {
			depth[a[2]] = c.Depth
		}
	}
	want := map[string]int{"$f": 1, "a": 1, "c": 1, "d": 1, "e": 1, "f": 1, "g": 1, "h": 0}
	for k, v := range want {
		if d, ok := depth[k]; !ok || d != v {
			t.Errorf("docker push %s: depth %d (found %v), want %d", k, d, ok, v)
		}
	}
	for _, c := range cmds {
		if v := c.Words; len(v) > 0 && (v[0].Value == "done" || v[0].Value == "esac" || v[0].Value == "}" || v[0].Value == ")") && c.Depth != 0 {
			t.Errorf("closer %q has depth %d", v[0].Value, c.Depth)
		}
	}
}

func TestFunctionDefinitionIsVisible(t *testing.T) {
	cmds := parseOK(t, "build() { make; }\nfunction deploy {\n  make\n}", State{Dir: "."})
	if !slices.Equal(values(cmds[0]), []string{"build", "()"}) {
		t.Fatalf("first = %q", values(cmds[0]))
	}
	var deploy bool
	for _, c := range cmds {
		if slices.Equal(values(c), []string{"function", "deploy"}) {
			deploy = true
		}
	}
	if !deploy {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestArgvAndPrefix(t *testing.T) {
	cmds := parseOK(t, "FOO=1 sudo cargo build --release", State{Dir: "."})
	if len(cmds) != 1 {
		t.Fatalf("commands = %+v", cmds)
	}
	if !slices.Equal(cmds[0].Argv(), []string{"cargo", "build", "--release"}) || !slices.Equal(cmds[0].Prefix(), []string{"FOO"}) {
		t.Fatalf("argv = %q, prefix = %q", cmds[0].Argv(), cmds[0].Prefix())
	}
	cmds = parseOK(t, "sudo -E -u ci env -i CARGO_TARGET_DIR=x cargo build", State{Dir: "."})
	if !slices.Equal(cmds[0].Argv(), []string{"cargo", "build"}) || !slices.Equal(cmds[0].Prefix(), []string{"CARGO_TARGET_DIR"}) {
		t.Fatalf("argv = %q, prefix = %q", cmds[0].Argv(), cmds[0].Prefix())
	}
	if w := cmds[0].ArgWords(); len(w) != 2 || w[0].Start != strings.Index("sudo -E -u ci env -i CARGO_TARGET_DIR=x cargo build", "cargo") {
		t.Fatalf("arg words = %+v", w)
	}
}

// Wrappers that run the next word as the program are skipped like sudo and env.
func TestArgvSkipsCommandWrappers(t *testing.T) {
	for script, want := range map[string][]string{
		"time cargo build --release":                        {"cargo", "build", "--release"},
		"time -p docker push x":                             {"docker", "push", "x"},
		"nohup docker push x":                               {"docker", "push", "x"},
		"nice -n 10 cargo build --release":                  {"cargo", "build", "--release"},
		"nice --adjustment=5 make release":                  {"make", "release"},
		"exec -a builder cargo build --release":             {"cargo", "build", "--release"},
		"exec cargo build":                                  {"cargo", "build"},
		"command cargo build --release":                     {"cargo", "build", "--release"},
		"command -p docker push x":                          {"docker", "push", "x"},
		"command -v docker":                                 {"command", "-v", "docker"},
		"command -V docker":                                 {"command", "-V", "docker"},
		"timeout 10m cargo build --release":                 {"cargo", "build", "--release"},
		"timeout -s KILL -k 5 10m docker push x":            {"docker", "push", "x"},
		"timeout --signal=TERM --preserve-status 1h make":   {"make"},
		"sudo time nice -n 5 env FOO=1 nohup docker push x": {"docker", "push", "x"},
	} {
		cmds := parseOK(t, script, State{Dir: "."})
		if len(cmds) != 1 || !slices.Equal(cmds[0].Argv(), want) {
			t.Errorf("%q: argv = %q, want %q", script, cmds[0].Argv(), want)
			continue
		}
		if w := cmds[0].ArgWords(); script[w[0].Start:w[0].Start+len(w[0].Raw)] != want[0] {
			t.Errorf("%q: arg word offsets are wrong: %+v", script, w[0])
		}
	}
	cmds := parseOK(t, "sudo time nice -n 5 env FOO=1 nohup docker push x", State{Dir: "."})
	if !slices.Equal(cmds[0].Prefix(), []string{"FOO"}) {
		t.Fatalf("prefix = %q", cmds[0].Prefix())
	}
	for _, s := range []string{"time", "timeout 5", "nice -n"} {
		if a := parseOK(t, s, State{Dir: "."})[0].Argv(); len(a) != 0 {
			t.Errorf("%q: argv = %q", s, a)
		}
	}
}

func TestRedirectionsAreNotWords(t *testing.T) {
	script := `docker build -t x . > build.log 2>&1 && echo "x" >> "$GITHUB_OUTPUT"`
	cmds := parseOK(t, script, State{Dir: "."})
	if len(cmds) != 2 {
		t.Fatalf("commands = %+v", cmds)
	}
	if !slices.Equal(cmds[0].Argv(), []string{"docker", "build", "-t", "x", "."}) || cmds[0].Op != "&&" {
		t.Fatalf("first = %+v", cmds[0])
	}
	if len(cmds[0].Redirs) != 2 || cmds[0].Redirs[0].Op != ">" || cmds[0].Redirs[0].Target.Value != "build.log" || cmds[0].Redirs[1].Op != "2>&" || cmds[0].Redirs[1].Target.Value != "1" {
		t.Fatalf("redirs = %+v", cmds[0].Redirs)
	}
	if cmds[0].End != strings.Index(script, " &&") {
		t.Fatalf("End = %d", cmds[0].End)
	}
	if r := cmds[1].Redirs; len(r) != 1 || r[0].Op != ">>" || r[0].Target.Value != "$GITHUB_OUTPUT" || cmds[1].End != len(script) {
		t.Fatalf("second = %+v", cmds[1])
	}
}

func TestOperators(t *testing.T) {
	cmds := parseOK(t, "a | b || c; d & e |& f\ng", State{Dir: "."})
	var ops []string
	for _, c := range cmds {
		ops = append(ops, c.Op)
	}
	if want := []string{"|", "||", ";", "&", "|", "\n", ""}; !slices.Equal(ops, want) {
		t.Fatalf("ops = %q, want %q", ops, want)
	}
}

func TestWordParts(t *testing.T) {
	cmds := parseOK(t, `echo $(echo "a b" ) ${X:-"c d"} `+"`echo e f`"+` 'g h' "i $(j "k") l" \m\ n $'o\'p'`, State{Dir: "."})
	want := []string{"echo", `$(echo "a b" )`, `${X:-"c d"}`, "`echo e f`", "g h", `i $(j "k") l`, "m n", `o\'p`}
	if len(cmds) != 1 || !slices.Equal(values(cmds[0]), want) {
		t.Fatalf("values = %q, want %q", values(cmds[0]), want)
	}
	cmds = parseOK(t, `arr=(a b) diff <(sort a) >(cat) "\$x\"\a"`, State{Dir: "."})
	want = []string{"arr=(a b)", "diff", "<(sort a)", ">(cat)", `$x"\a`}
	if len(cmds) != 1 || !slices.Equal(values(cmds[0]), want) {
		t.Fatalf("values = %q, want %q", values(cmds[0]), want)
	}
}

func TestHeredocBodyIsNested(t *testing.T) {
	cmds := parseOK(t, "cat <<EOF\ndocker push x\nEOF", State{Dir: "."})
	for _, c := range cmds {
		if a := c.Argv(); len(a) > 0 && a[0] == "docker" && c.Depth == 0 {
			t.Fatalf("docker push at depth 0: %+v", cmds)
		}
	}
}

func TestHeredocVariants(t *testing.T) {
	script := strings.Join([]string{
		"cat <<-'END' > f && echo hi",
		"\tit's a body line",
		"\tEND",
		"docker build -t x - <<EOF",
		"FROM alpine",
		"EOF",
		"docker push x",
	}, "\n")
	cmds := parseOK(t, script, State{Dir: "."})
	byFirst := map[string]Command{}
	for _, c := range cmds {
		if a := c.Argv(); len(a) > 0 {
			byFirst[a[0]+" "+strings.Join(a[1:], " ")] = c
		}
	}
	if c, ok := byFirst["echo hi"]; !ok || c.Depth != 1 {
		t.Errorf("echo after the heredoc operator on the same line: %+v", c)
	}
	if c, ok := byFirst["docker build -t x -"]; !ok || c.Depth != 1 {
		t.Errorf("build with a heredoc: %+v", c)
	}
	if c, ok := byFirst["docker push x"]; !ok || c.Depth != 0 {
		t.Errorf("push after the heredocs: %+v (all: %+v)", c, cmds)
	}
	if c, ok := byFirst["FROM alpine"]; !ok || c.Depth != 1 {
		t.Errorf("heredoc body line: %+v", c)
	}
}

func TestUnterminatedHeredocNestsTheRest(t *testing.T) {
	cmds := parseOK(t, "cat <<EOF\ndocker push x\nEOF \n", State{Dir: "."})
	for _, c := range cmds {
		if c.Depth == 0 && len(c.Argv()) > 0 && c.Argv()[0] == "docker" {
			t.Fatalf("commands = %+v", cmds)
		}
	}
}

func TestEmptyAndBlank(t *testing.T) {
	if cmds, end := Parse("\n\n  # x\n", State{Dir: "a"}); len(cmds) != 0 || end.Dir != "a" {
		t.Fatalf("commands = %+v, end = %+v", cmds, end)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`docker build -t "$IMAGE:${{ github.sha }}" . && docker push "$IMAGE"`,
		"if [ -n \"$X\" ]; then\n  docker build .\nfi",
		"cat <<-'E' | x\n\tbody\n\tE\ncase $a in (b) c;; esac",
		"f() ( cd x ); { a; } > o 2>&1 & `b` $(c \"d\" ')') ${e:-'}'} $'\\''",
		"'unterminated \"x ${{ y",
		"NOTES=$(cat <<EOF\nDon't forget\nEOF\n)\ndocker push app",
		"echo \"$(a\"\ndocker push x <<< y\ncat 0<<E\nE",
		"docker build .\\\n  && docker push x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		cmds, _ := Parse(s, State{Dir: "."})
		prev := -1
		for _, c := range cmds {
			c.Argv()
			c.Prefix()
			if c.Start > c.End || c.End > len(s) || c.Depth < 0 {
				t.Fatalf("bad command %+v", c)
			}
			for _, w := range c.Words {
				if w.Start < 0 || w.Start+len(w.Raw) > len(s) || s[w.Start:w.Start+len(w.Raw)] != w.Raw {
					t.Fatalf("word %+v does not match the script", w)
				}
				if strings.HasSuffix(w.Raw, "\\\n") {
					t.Fatalf("word %+v ends with a continuation", w)
				}
			}
			if c.Start < prev {
				t.Fatalf("commands out of order: %+v", cmds)
			}
			prev = c.Start
		}
	})
}

func TestHeredocInsideCommandSubstitution(t *testing.T) {
	script := "NOTES=$(cat <<EOF\nDon't forget\nEOF\n)\ndocker push app"
	cmds := parseOK(t, script, State{Dir: "."})
	if len(cmds) != 2 {
		t.Fatalf("got %d commands: %+v", len(cmds), cmds)
	}
	if w := cmds[0].Words; len(w) != 1 || !strings.HasSuffix(w[0].Raw, "\n)") || cmds[0].Depth != 0 {
		t.Fatalf("first = %+v", cmds[0])
	}
	if !slices.Equal(cmds[1].Argv(), []string{"docker", "push", "app"}) || cmds[1].Depth != 0 {
		t.Fatalf("push = %+v", cmds[1])
	}
	if !slices.Equal(cmds[1].Before.Env, []string{"NOTES"}) {
		t.Fatalf("env = %q", cmds[1].Before.Env)
	}
}

// A construct the lexer cannot close must not hide the rest of the script, and everything from
// it on is unsafe to edit (Depth+1).
func TestUnterminatedConstructsLeaveTheRestVisibleAndUnsafe(t *testing.T) {
	for _, open := range []string{"echo $(date", "echo 'a", "echo \"a", "echo `a", "echo ${{ a", "echo ${a", "echo \"$(a\"", "x=(a", "diff <(a"} {
		script := "docker build -t x .\n" + open + "\ndocker push x"
		cmds := parseOK(t, script, State{Dir: "."})
		var build, push *Command
		for i := range cmds {
			switch a := cmds[i].Argv(); {
			case len(a) >= 2 && a[0] == "docker" && a[1] == "build":
				build = &cmds[i]
			case len(a) >= 2 && a[0] == "docker" && a[1] == "push":
				push = &cmds[i]
			}
		}
		if build == nil || build.Depth != 0 {
			t.Errorf("%q: build = %+v", open, build)
		}
		if push == nil || push.Depth < 1 {
			t.Errorf("%q: push = %+v (all: %+v)", open, push, cmds)
		}
		for _, c := range cmds {
			if c.Start > build.End && c.Depth < 1 {
				t.Errorf("%q: command after the unterminated construct is at depth 0: %+v", open, c)
			}
		}
	}
}

func TestHereStringIsNotNested(t *testing.T) {
	cmds := parseOK(t, "cat <<< \"$X\"; cat 0<<<x\ndocker build .", State{Dir: "."})
	for _, c := range cmds {
		if c.Depth != 0 {
			t.Fatalf("depth %d for %+v", c.Depth, c)
		}
	}
	cmds = parseOK(t, "cat 0<<EOF\nx\nEOF\ndocker build .", State{Dir: "."})
	if cmds[0].Depth != 1 || cmds[len(cmds)-1].Depth != 0 {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestContinuationAtEndOfLastWord(t *testing.T) {
	script := "docker build .\\\n  && docker push x"
	cmds := parseOK(t, script, State{Dir: "."})
	if len(cmds) != 2 || cmds[0].Op != "&&" || !cmds[0].ContAfter || cmds[0].End != strings.Index(script, "\\") {
		t.Fatalf("commands = %+v", cmds)
	}
	cmds = parseOK(t, "docker build .\\\n\ndocker push x", State{Dir: "."})
	if len(cmds) != 2 || !cmds[0].ContAfter {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestExpressionLiteralWithClosingBraces(t *testing.T) {
	cmds := parseOK(t, "echo ${{ format('{0}}}', x) }} && make", State{Dir: "."})
	if len(cmds) != 2 || cmds[0].Words[1].Value != "${{ format('{0}}}', x) }}" || cmds[0].Op != "&&" {
		t.Fatalf("commands = %+v", cmds)
	}
}

// A script's own directory, $(dirname "$0") or $(dirname "${BASH_SOURCE[0]}"), resolves against
// State.Self.
func TestScriptDirectory(t *testing.T) {
	for v, want := range map[string]string{
		`$(dirname "$0")`: "scripts/ci", `$(dirname "$0")/..`: "scripts", `$(dirname $0)/lib.sh`: "scripts/ci/lib.sh",
		`$(dirname "${BASH_SOURCE[0]}")/../..`: ".", `$(dirname -- "${BASH_SOURCE[0]}")`: "scripts/ci",
		`$(dirname ${BASH_SOURCE})`: "scripts/ci",
	} {
		if got, ok := ScriptDir(v, "scripts/ci"); !ok || got != want {
			t.Errorf("ScriptDir(%q) = %q, %v; want %q", v, got, ok, want)
		}
	}
	for _, v := range []string{`$(dirname "$0")/$X`, `$(dirname "$0")/../../..`, `$(dirname "$1")`, `$(pwd)`, `x/$(dirname "$0")`, `$(dirname "$0")x`} {
		if got, ok := ScriptDir(v, "scripts/ci"); ok {
			t.Errorf("ScriptDir(%q) = %q", v, got)
		}
	}
	if _, ok := ScriptDir(`$(dirname "$0")`, ""); ok {
		t.Error("ScriptDir without a script directory")
	}
	cmds := parseOK(t, "cd \"$(dirname \"$0\")/..\" && make\npushd $(dirname ${BASH_SOURCE[0]}) && make", State{Dir: "", Self: "scripts/ci"})
	if cmds[1].Before.Dir != "scripts" || cmds[3].Before.Dir != "scripts/ci" || cmds[3].Before.Self != "scripts/ci" {
		t.Fatalf("dirs = %q, %q", cmds[1].Before.Dir, cmds[3].Before.Dir)
	}
	if cmds := parseOK(t, `cd "$(dirname "$0")" && make`, State{Dir: "."}); cmds[1].Before.Dir != "" {
		t.Fatalf("without Self: %q", cmds[1].Before.Dir)
	}
}
