package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/grype"
)

func TestVersionPrintsShearboltAndGrype(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "shearbolt dev") || !strings.Contains(out.String(), "grype "+grype.Version) {
		t.Fatalf("unexpected version output: %q", out.String())
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"frobnicate"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestNoArgumentsPrintsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(nil, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Usage:") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestIsTerminalRejectsDevNullAndPipes(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Errorf("%s counts as a terminal", os.DevNull)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) || isTerminal(&bytes.Buffer{}) {
		t.Errorf("a pipe or buffer counts as a terminal")
	}
}

// version takes no arguments: any argument, -h included, is a usage error with exit 2, the
// usage on stderr and nothing on stdout. version and --version alone print the versions.
func TestVersionRejectsArguments(t *testing.T) {
	for _, args := range [][]string{{"version", "extra"}, {"version", "-h"}, {"version", "--json"}, {"--version", "x"}} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 2 || out.Len() != 0 ||
			!strings.Contains(errb.String(), "shearbolt version: unexpected argument "+`"`+args[1]+`"`) || !strings.Contains(errb.String(), "Usage:") {
			t.Errorf("%q: exit = %d\nstdout: %q\nstderr: %q", args, code, out.String(), errb.String())
		}
	}
	for _, arg := range []string{"version", "--version"} {
		var out, errb bytes.Buffer
		if code := Run([]string{arg}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "shearbolt dev\ngrype ") || errb.Len() != 0 {
			t.Errorf("%s: exit = %d\nstdout: %q\nstderr: %q", arg, code, out.String(), errb.String())
		}
	}
}
