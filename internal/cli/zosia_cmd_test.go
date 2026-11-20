package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func zosiaCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"zosia"}, args...), &out, &errb, setupDeps(false, ""))
	return code, out.String(), errb.String()
}

func TestZosiaEnableAndDisable(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	if code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes"); code != 0 {
		t.Fatalf("setup: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	workflow := filepath.Join(dir, ".github/workflows/release.yml")
	before, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatal(err)
	}
	const line = "zosia-secret: ${{ secrets.ZOSIA_SECRET }}"

	code, out, errb := zosiaCmd(t, "enable")
	after, _ := os.ReadFile(workflow)
	if code != 0 || errb != "" || strings.Count(string(after), line) != 1 || !strings.Contains(out, "+          "+line) ||
		!strings.Contains(out, "https://zosiasec.com/app/secrets") || !strings.Contains(out, "gh secret set ZOSIA_SECRET") {
		t.Fatalf("enable: exit = %d\nstdout: %s\nstderr: %s\nworkflow:\n%s", code, out, errb, after)
	}

	code, out, errb = zosiaCmd(t, "enable")
	again, _ := os.ReadFile(workflow)
	if code != 0 || !bytes.Equal(again, after) || !strings.Contains(out, "already sends to Zosia") || strings.Contains(out, "+++ ") ||
		!strings.Contains(out, "gh secret set ZOSIA_SECRET") {
		t.Errorf("second enable: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}

	code, out, errb = zosiaCmd(t, "disable")
	off, _ := os.ReadFile(workflow)
	if code != 0 || errb != "" || !bytes.Equal(off, before) || !strings.Contains(out, "-          "+line) || !strings.Contains(out, "The secret itself stays") {
		t.Errorf("disable: exit = %d\nstdout: %s\nstderr: %s\nworkflow:\n%s", code, out, errb, off)
	}
}

func TestZosiaUsage(t *testing.T) {
	fixtureRepo(t, "nextjs-docker")
	for _, args := range [][]string{nil, {"connect"}, {"enable", "extra"}} {
		if code, out, errb := zosiaCmd(t, args...); code != 2 || out != "" || !strings.Contains(errb, "shearbolt zosia enable") {
			t.Errorf("%q: exit = %d\nstdout: %s\nstderr: %s", args, code, out, errb)
		}
	}
	if code, out, _ := zosiaCmd(t, "-h"); code != 0 || !strings.Contains(out, "shearbolt zosia enable|disable") {
		t.Errorf("-h: exit = %d\nstdout: %s", code, out)
	}
	// No gate yet: nothing to enable.
	code, out, errb := zosiaCmd(t, "enable")
	if code != 2 || out != "" || !strings.Contains(errb, "shearbolt setup") {
		t.Errorf("without a gate: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if !strings.Contains(usage, "shearbolt zosia enable|disable") {
		t.Errorf("the usage text does not list the zosia command")
	}
}
