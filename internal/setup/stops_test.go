package setup

import (
	"slices"
	"testing"
)

// stopping returns the indexes of the commands of script, run from the repository root, that can
// stop the shell early.
func stopping(t *testing.T, files map[string]string, script string) []int {
	t.Helper()
	_, cl := classifyAll(t, repo(t, files), script)
	var out []int
	for i, c := range cl {
		if c.Stops {
			out = append(out, i)
		}
	}
	return out
}

// P6: exit without an argument, with 0 or with a variable, and exec with a command, can stop the
// shell early and successfully; a literal non-zero exit fails it and does not count.
func TestStopsDirect(t *testing.T) {
	for script, want := range map[string][]int{
		"exit 0":                            {0},
		"exit":                              {0},
		`exit "$code"`:                      {0},
		"exit $?":                           {0},
		"exit 256":                          {0}, // the status is taken modulo 256
		"exit 1":                            nil,
		"exit 2 # deploy failed":            nil,
		"cd x || exit 1":                    nil,
		`if [ -n "$X" ]; then exit 0; fi`:   {1},
		"(exit 0)":                          {0},
		"exec docker push x":                {0},
		"FOO=1 exec ./run.sh":               {0},
		"exec >log 2>&1":                    nil,
		"exec 3>&1":                         nil,
		"echo exit":                         nil,
		"echo exec docker push x":           nil,
		"bash -c 'exit 0'":                  nil,
		"npm run build && exit 0 || exit 3": {1},
	} {
		if got := stopping(t, nil, script); !slices.Equal(got, want) {
			t.Errorf("%q: stops at %v, want %v", script, got, want)
		}
	}
}

// A command that builds or pushes can stop too: Stops is computed apart from the first match.
func TestStopsWithBuildOrPush(t *testing.T) {
	cl := one(t, repo(t, nil), "exec docker push x")
	if !cl.Stops || !cl.Push {
		t.Errorf("exec docker push: %+v", cl)
	}
	cl = one(t, repo(t, nil), "exec docker build -t x .")
	if !cl.Stops || len(cl.Builds) != 1 {
		t.Errorf("exec docker build: %+v", cl)
	}
	cl = one(t, repo(t, nil), "docker build -t x .")
	if cl.Stops {
		t.Errorf("docker build: %+v", cl)
	}
}

// A sourced in-repo file stops the shell when it holds a stop anywhere (followed recursively), or
// when it cannot be followed. A file sourced from outside the repository and a child shell never
// count.
func TestStopsSourced(t *testing.T) {
	files := map[string]string{
		"ci/skip.sh":     "#!/bin/sh\nif true; then exit 0; fi\n",
		"ci/env.sh":      "export A=1\ncd app || exit 1\nbash -c 'exit 0'\n",
		"ci/outer.sh":    "echo outer\n. ci/inner.sh\n",
		"ci/inner.sh":    "[ -n \"$DONE\" ] && exit\n",
		"ci/a.sh":        "source ci/b.sh\n",
		"ci/b.sh":        "source ci/c.sh\n",
		"ci/c.sh":        "source ci/d.sh\n",
		"ci/d.sh":        "echo deepest\n",
		"ci/binary.sh":   "echo \x00\n",
		"ci/execs.sh":    "exec \"$@\"\n",
		"app/skip.sh":    "exit 0\n",
		"ci/runs.sh":     "./ci/skip.sh\nbash ci/skip.sh\n",
		"ci/home.sh":     "source ~/.cargo/env\n",
		"ci/missing2.sh": ". ./ci/nope.sh\n",
	}
	for script, want := range map[string][]int{
		". ./ci/skip.sh":       {0},
		"source ci/skip.sh":    {0},
		"source ci/env.sh":     nil,
		"source ci/outer.sh":   {0},
		"source ./missing.sh":  {0},
		"source $SCRIPT":       {0},
		"source ci":            {0}, // a directory cannot be read
		". ci/binary.sh":       {0},
		". ci/execs.sh":        {0},
		". ci/missing2.sh":     {0},
		"cd app && . skip.sh":  {1},
		"source ~/.cargo/env":  nil,
		`. "$HOME/.cargo/env"`: nil,
		". ci/home.sh":         nil,
		"bash ./ci/skip.sh":    nil,
		"sh ci/skip.sh":        nil,
		"./ci/skip.sh":         nil,
		". ci/runs.sh":         nil,
		"source ci/b.sh":       nil, // b, c and d: three levels, all followed
		"source ci/a.sh":       {0}, // d is a fourth level, which is not followed
	} {
		if got := stopping(t, files, script); !slices.Equal(got, want) {
			t.Errorf("%q: stops at %v, want %v", script, got, want)
		}
	}
}
