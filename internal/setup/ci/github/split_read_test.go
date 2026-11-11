package github

import (
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

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
		if got := assignRead([]string{"live"}, c.rem, c.script, before, nil); got != c.want {
			t.Errorf("assignRead(rem %q, script %q, before %q) = %v, want %v", c.rem, c.script, c.before, got, c.want)
		}
	}
}

// Every name of a command that assigns several counts.
func TestAssignReadNames(t *testing.T) {
	if !assignRead([]string{"a", "b"}, "echo $b\n", "", nil, nil) {
		t.Error("a read of the second name does not count")
	}
}

// A name that may be inherited from the environment counts as read: one with no lowercase
// letter, or one of env, the env: keys around the step.
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
	} {
		if got := assignRead([]string{c.name}, "docker push x\n", "", nil, c.env); got != c.want {
			t.Errorf("assignRead(%s, env %q) = %v, want %v", c.name, c.env, got, c.want)
		}
	}
}
