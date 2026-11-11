package shelledit

import (
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

func TestOwnLine(t *testing.T) {
	for line, want := range map[string]bool{
		ReachedLine: true,
		`echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`:   true,
		`echo shearbolt-image-2=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`: true,
		"docker tag web:1 web:shearbolt-${{ github.sha }}":                           true,
		`echo digest=abc >> "$GITHUB_OUTPUT"`:                                        false,
		"echo reached":                                                               false,
		"./deploy.sh":                                                                false,
	} {
		cmds, _ := shell.Parse(line, shell.State{})
		if len(cmds) != 1 {
			t.Fatalf("%q: %d commands", line, len(cmds))
		}
		if got := OwnLine(cmds[0]); got != want {
			t.Errorf("OwnLine(%q) = %v, want %v", line, got, want)
		}
	}
	if ReachedLine != `echo shearbolt-reached=true >> "$GITHUB_OUTPUT"` || ReachedOutput != "shearbolt-reached" {
		t.Errorf("ReachedLine = %q, ReachedOutput = %q", ReachedLine, ReachedOutput)
	}
}

// StopName is only asked about commands that stop the shell, so it reads no further than words.
func TestStopName(t *testing.T) {
	for line, want := range map[string]string{
		"exit 0":            "exit",
		"exit":              "exit",
		"exec ./other.sh":   "exec",
		"source ./env.sh":   "source",
		". ./env.sh":        "source",
		"command exit 0":    "exit",
		"X=1 exec ./run.sh": "exec",
		"echo exit-codes":   "",
	} {
		cmds, _ := shell.Parse(line, shell.State{})
		if len(cmds) != 1 {
			t.Fatalf("%q: %d commands", line, len(cmds))
		}
		if got := StopName(cmds[0]); got != want {
			t.Errorf("StopName(%q) = %q, want %q", line, got, want)
		}
	}
}
