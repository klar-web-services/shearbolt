package github

import (
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// readsOf is what assignRead checks for continued text rem, a step script and inherited names.
func readsOf(rem, script string, inherited ...string) *continued {
	cmds, _ := shell.Parse(script, shell.State{})
	return &continued{text: rem, cmds: commandsIn(rem), step: cmds, inherited: inherited}
}

// Each rule of assignRead on its own (design §4); the script and the commands before the
// assignment default to ones that read nothing.
func TestAssignRead(t *testing.T) {
	const push = "docker push x\n"
	for _, c := range []struct {
		rem, script, before string
		want                bool
	}{
		{rem: push},
		// A whole-word use, wherever it is.
		{rem: push + `echo "$live"`, want: true},
		{rem: push + "(( live + 1 ))", want: true},
		{rem: push + "[[ -v live ]]", want: true},
		{rem: push + "echo ${arr[live]}", want: true},
		{rem: push + "# live", want: true},
		{rem: "live=2\n" + push, want: true},
		{rem: push + `echo "$lively $alive live_x"`},
		// Indirect and prefix expansion.
		{rem: push + `echo "${!live_@}"`, want: true},
		{rem: push + `echo "${!ref}"`, want: true},
		// eval, source and ., also inside a substitution, and as a command only.
		{rem: push + `eval "$CMD"`, want: true},
		{rem: push + "command eval x", want: true},
		{rem: push + "builtin eval x", want: true},
		{rem: push + "source ./env.sh", want: true},
		{rem: push + ". ./env.sh", want: true},
		{rem: push + `echo "$(eval "$CMD")"`, want: true},
		{rem: push + "echo `source x`", want: true},
		{rem: push + "diff <(. ./env.sh) y", want: true},
		{rem: push + "echo evaluate source"},
		{rem: push + "docker build -t y ."},
		// Namerefs.
		{rem: push + `declare -n r="$TARGET"`, want: true},
		{rem: push + `local -n r="$TARGET"`, want: true},
		{rem: push + `typeset -n r="$TARGET"`, want: true},
		{rem: push + `declare -gn r="$TARGET"`, want: true},
		{rem: push + "declare -x FOO=1"},
		{rem: push + "echo declare -n"},
		// Commands that print variables.
		{rem: push + "set", want: true},
		{rem: push + "set -e"},
		{rem: push + "declare -p", want: true},
		{rem: push + "declare", want: true},
		{rem: push + "typeset -p", want: true},
		{rem: push + "export -p", want: true},
		{rem: push + "compgen -v", want: true},
		{rem: push + "compgen -A variable", want: true},
		{rem: push + "compgen -c"},
		// The script exports or protects the name, anywhere.
		{rem: push, script: "export live\n", want: true},
		{rem: push, script: "export FOO live\n", want: true},
		{rem: push, script: "export live=1\n", want: true},
		{rem: push, script: "declare -x live\n", want: true},
		{rem: push, script: "typeset -x live\n", want: true},
		{rem: push, script: "declare -r live\n", want: true},
		{rem: push, script: "readonly live\n", want: true},
		{rem: push, script: "export lively alive\n"},
		{rem: push, script: "declare -a live\n"},
		{rem: push, script: "echo export live\n"},
		// allexport before the assignment.
		{rem: push, before: "set -a\n", want: true},
		{rem: push, before: "set -eua\n", want: true},
		{rem: push, before: "set -o allexport\n", want: true},
		{rem: push, before: "shopt -s -o allexport\n", want: true},
		{rem: push, before: "set -euo pipefail\n"},
	} {
		before, _ := shell.Parse(c.before, shell.State{})
		if got := assignRead([]string{"live"}, readsOf(c.rem, c.script), before); got != c.want {
			t.Errorf("assignRead(rem %q, script %q, before %q) = %v, want %v", c.rem, c.script, c.before, got, c.want)
		}
	}
}

// Every name of a command that assigns several counts.
func TestAssignReadNames(t *testing.T) {
	if !assignRead([]string{"a", "b"}, readsOf("echo $b\n", ""), nil) {
		t.Error("a read of the second name does not count")
	}
}

// A name that may be inherited from the environment counts as read: one with no lowercase
// letter, a proxy variable, one of the inherited names, or any name when setup cannot read them.
func TestAssignReadInherited(t *testing.T) {
	for _, c := range []struct {
		name string
		env  []string
		want bool
	}{
		{name: "live"},
		{name: "Live"},
		{name: "live_2"},
		{name: "PATH", want: true},
		{name: "IMAGE_2", want: true},
		{name: "_", want: true},
		{name: "foo", env: []string{"BAR", "foo"}, want: true},
		{name: "foo", env: []string{"FOO", "food"}},
		{name: "http_proxy", want: true},
		{name: "https_proxy", want: true},
		{name: "ftp_proxy", want: true},
		{name: "no_proxy", want: true},
		{name: "all_proxy", want: true},
		{name: "my_proxy"},
	} {
		if got := assignRead([]string{c.name}, readsOf("docker push x\n", "", c.env...), nil); got != c.want {
			t.Errorf("assignRead(%s, env %q) = %v, want %v", c.name, c.env, got, c.want)
		}
	}
	any := readsOf("docker push x\n", "")
	any.anyName = true
	if !assignRead([]string{"live"}, any, nil) {
		t.Error("an env: setup cannot read does not make every name read")
	}
}

// Each form of an assignment hidden in an expansion, in arithmetic or in a builtin, and the
// forms that only read.
func TestHiddenAssign(t *testing.T) {
	for script, want := range map[string]bool{
		"x=${y:=v}":                             true,
		"x=${y=v}":                              true,
		": ${n:=5}":                             true,
		`echo "${a[0]:=v}"`:                     true,
		`echo "${x:-${y:=v}}"`:                  true,
		"x=$(( y = 5 ))":                        true,
		"echo $((i++))":                         true,
		"echo $((--i))":                         true,
		"echo $(( n += 2 ))":                    true,
		"echo $(( n -= 2 ))":                    true,
		"echo $(( n *= 2 ))":                    true,
		"echo $(( n /= 2 ))":                    true,
		"echo $(( n %= 2 ))":                    true,
		"echo $(( n <<= 1 ))":                   true,
		"echo $(( n >>= 1 ))":                   true,
		"echo $(( n &= 1 ))":                    true,
		"echo $(( n ^= 1 ))":                    true,
		"echo $(( n |= 1 ))":                    true,
		"echo $(( (n = 1) + 2 ))":               true,
		"echo $[n=1]":                           true,
		"(( n = 5 ))":                           true,
		"(( n++ )) || true":                     true,
		"(( a > 0 || n++ ))":                    true,
		"(( ok && (n = 1) ))":                   true,
		"if (( x || (n += 2) )); then :; fi":    true,
		"(( a | (n = 3) ))":                     true,
		"echo $[a || b++]":                      true,
		`echo "$(echo ")") $((n++))"`:           true,
		`echo $(echo "(") $((n++))`:             true,
		"echo `echo '`' $((n++))":               true,
		"for ((i = 0; i < 3; i++)); do :; done": true,
		`echo "${a[i++]}"`:                      true,
		`echo "${a[i=2]}"`:                      true,
		`echo "${s:i++:1}"`:                     true,
		`echo "${s:1:n=2}"`:                     true,
		"a[i++]=5":                              true,
		"declare a[n=1]=x":                      true,
		"let n=5":                               true,
		"printf -v n '%s' x":                    true,
		"builtin printf -v n x":                 true,
		"getopts ab opt":                        true,
		"wait -p pid":                           true,
		"cat <<EOF\n$((i++))\nEOF":              true,
		"echo $((a+1))":                         false,
		"echo $(( a == 1 ? 2 : 3 ))":            false,
		"echo $((a != 1)) $((a <= 1)) $((a >= 1)) $((a << 1))": false,
		"(( a == 1 )) && echo y":                               false,
		"echo $[a+1]":                                          false,
		`echo "${a[1]} ${a[@]} ${x:-y} ${x:+y} ${x:?y} ${s:1:2} ${s: -1} ${#x}"`: false,
		`echo "${{ github.event.labels[0] == 'x' }}"`:                            false,
		"docker build --build-arg N=$((n+1)) --tag x .":                          false,
		"x=$((a+1))": false,
		`echo "build $(( $(git rev-list --count HEAD) + 1 ))"`: false,
		"echo $(( $(grep -c a=1 f) + 1 ))":                     false,
		"x=$(echo $((i++)) ${y:=v})":                           false,
		`x=$(echo ")" $((i++)))`:                               false,
		`x=$(echo ')' $((i++)))`:                               false,
		`echo '$(' $((n++)) ')'`:                               true,
		"# don't\necho '$(' $((n++)) ')'":                      true,
		"echo `echo $((i++))`":                                 false,
		`echo "${tags[@]/#/--tag=}"`:                           false,
		`echo "${#a[@]} ${!a[@]}"`:                             false,
		"printf '%s' x":                                        false,
		"wait $pid":                                            false,
		"echo a=1 -- --b":                                      false,
	} {
		cmds, _ := shell.Parse(script, shell.State{})
		got := false
		for i := range cmds {
			got = got || hiddenAssign(maskSubst(script), cmds, i)
		}
		if got != want {
			t.Errorf("hiddenAssign(%q) = %v, want %v", script, got, want)
		}
	}
}

// The names a step's commands write to $GITHUB_ENV, or all when one write cannot be read.
func TestEnvWrites(t *testing.T) {
	for _, c := range []struct {
		script string
		names  []string
		all    bool
	}{
		{script: "echo hi"},
		{script: `echo "live=1" >> "$GITHUB_ENV"`, names: []string{"live"}},
		{script: `echo -n foo=$x >>$GITHUB_ENV`, names: []string{"foo"}},
		{script: `echo -e "a=1\nb=2" >> "${GITHUB_ENV}"`, names: []string{"a", "b"}},
		{script: `printf 'tag=%s\n' "$t" >> "$GITHUB_ENV"`, names: []string{"tag"}},
		{script: "cat >> \"$GITHUB_ENV\" <<EOF\na=1\njson<<X\nnot=a name\nX\nb=2\nEOF\necho c=3", names: []string{"a", "json", "b"}},
		{script: "cat <<-'EOF' >> \"$GITHUB_ENV\"\n\ta=1\n\tEOF", names: []string{"a"}},
		{script: `printf '%s\n' "a=1" >> "$GITHUB_ENV"`, all: true},
		{script: `echo "${P}_X=1" >> "$GITHUB_ENV"`, all: true},
		{script: `cat vars.env >> "$GITHUB_ENV"`, all: true},
		{script: `echo a=1 | tee -a "$GITHUB_ENV"`, all: true},
		{script: `{ echo a=1; echo b=2; } >> "$GITHUB_ENV"`, all: true},
		{script: `cat <<< "a=1" >> "$GITHUB_ENV"`, all: true},
	} {
		cmds, _ := shell.Parse(c.script, shell.State{})
		names, all := envWrites(c.script, cmds)
		slices.Sort(names)
		want := slices.Clone(c.names)
		slices.Sort(want)
		if all != c.all || !c.all && !slices.Equal(names, want) {
			t.Errorf("envWrites(%q) = %q, %v, want %q, %v", c.script, names, all, want, c.all)
		}
	}
}

// One word with many backticks parses each substitution once, not every nesting of them.
func TestCommandsInManyBackticks(t *testing.T) {
	text := "echo " + strings.Repeat("`eval x`", 20)
	cmds := commandsIn(text)
	if len(cmds) > 1+40 {
		t.Errorf("commandsIn returned %d commands for 20 substitutions", len(cmds))
	}
	evals := 0
	for _, c := range cmds {
		if argv := c.Argv(); len(argv) > 0 && argv[0] == "eval" {
			evals++
		}
	}
	if evals == 0 {
		t.Error("commandsIn does not see the commands inside the backticks")
	}
}

// A shell: or defaults.run.shell with -a or allexport exports every assignment.
func TestShellAllexport(t *testing.T) {
	for sh, want := range map[string]bool{
		"bash -ae {0}":          true,
		"sh -a {0}":             true,
		"bash -o allexport {0}": true,
		"bash --noprofile --norc -eo pipefail {0}": false,
		"bash":      false,
		"sh -e {0}": false,
	} {
		if got := shellAllexport(sh); got != want {
			t.Errorf("shellAllexport(%q) = %v, want %v", sh, got, want)
		}
	}
}
