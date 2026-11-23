package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// fakeGH stands in for the GitHub CLI: it records every call and answers `repo view` and
// `secret list` with the given JSON.
type fakeGH struct {
	repo, secrets string
	viewErr       error
	setErr        error
	calls         [][]string
	setStdin      io.Reader
}

func (f *fakeGH) run(dir string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	f.calls = append(f.calls, args)
	switch strings.Join(args[:2], " ") {
	case "repo view":
		if f.viewErr != nil {
			return f.viewErr
		}
		io.WriteString(stdout, f.repo)
	case "secret list":
		io.WriteString(stdout, f.secrets)
	case "secret set":
		f.setStdin = stdin
		return f.setErr
	}
	return nil
}

func (f *fakeGH) set() []string {
	for _, c := range f.calls {
		if c[0] == "secret" && c[1] == "set" {
			return c
		}
	}
	return nil
}

func zosiaEnable(t *testing.T, gh *fakeGH, tty bool, stdin string) (int, string, string) {
	t.Helper()
	d := setupDeps(tty, stdin)
	d.gh = gh.run
	var out, errb bytes.Buffer
	code := run([]string{"zosia", "enable"}, &out, &errb, d)
	return code, out.String(), errb.String()
}

func gatedRepo(t *testing.T) string {
	t.Helper()
	dir := fixtureRepo(t, "nextjs-docker")
	if code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes"); code != 0 {
		t.Fatalf("setup: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	return dir
}

const (
	ghRepo     = `{"nameWithOwner":"acme/web","viewerPermission":"ADMIN"}`
	manualNote = "Add the repository secret ZOSIA_SECRET"
)

func TestZosiaEnableStoresTheSecretWithGH(t *testing.T) {
	gatedRepo(t)
	gh := &fakeGH{repo: ghRepo, secrets: `[{"name":"OTHER"}]`}
	code, out, errb := zosiaEnable(t, gh, true, "")
	want := []string{"secret", "set", "ZOSIA_SECRET", "--repo", "acme/web"}
	if code != 0 || errb != "" || !slices.Equal(gh.set(), want) || gh.setStdin == nil {
		t.Fatalf("exit = %d, gh calls = %q\nstdout: %s\nstderr: %s", code, gh.calls, out, errb)
	}
	for _, s := range []string{"https://zosiasec.com/app/secrets", "for acme/web", "ZOSIA_SECRET is set for acme/web.\n", "zosia-secret: ${{ secrets.ZOSIA_SECRET }}"} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, manualNote) {
		t.Errorf("stdout still tells the user to add the secret by hand:\n%s", out)
	}
	if i, j := strings.Index(out, zosiaReview), strings.Index(out, "https://zosiasec.com/app/secrets"); i < 0 || j < i {
		t.Errorf("the prompt comes before the review line:\n%s", out)
	}
}

func TestZosiaEnableAsksBeforeReplacingTheSecret(t *testing.T) {
	gatedRepo(t)
	const secrets = `[{"name":"ZOSIA_SECRET"}]`
	gh := &fakeGH{repo: ghRepo, secrets: secrets}
	code, out, _ := zosiaEnable(t, gh, true, "\n")
	if code != 0 || gh.set() != nil || !strings.Contains(out, "ZOSIA_SECRET is already set for acme/web. Replace it? [y/N]") ||
		!strings.Contains(out, "Kept the existing ZOSIA_SECRET.") || strings.Contains(out, manualNote) {
		t.Fatalf("declined: exit = %d, gh calls = %q\nstdout: %s", code, gh.calls, out)
	}
	gh = &fakeGH{repo: ghRepo, secrets: secrets}
	code, out, _ = zosiaEnable(t, gh, true, "y\n")
	if code != 0 || gh.set() == nil || !strings.Contains(out, "ZOSIA_SECRET is set for acme/web.") {
		t.Fatalf("accepted: exit = %d, gh calls = %q\nstdout: %s", code, gh.calls, out)
	}
}

func TestZosiaEnableFallsBackToTheNote(t *testing.T) {
	dir := gatedRepo(t)
	tests := map[string]struct {
		gh    *fakeGH
		tty   bool
		calls int
	}{
		"no terminal":       {&fakeGH{repo: ghRepo, secrets: "[]"}, false, 2},
		"gh fails":          {&fakeGH{viewErr: errors.New("exit status 4")}, true, 1},
		"read access only":  {&fakeGH{repo: `{"nameWithOwner":"acme/web","viewerPermission":"READ"}`, secrets: "[]"}, true, 1},
		"gh prints no JSON": {&fakeGH{repo: "", secrets: "[]"}, true, 1},
		"secret list fails": {&fakeGH{repo: ghRepo, secrets: "HTTP 403"}, true, 2},
	}
	for name, tt := range tests {
		code, out, errb := zosiaEnable(t, tt.gh, tt.tty, "")
		if code != 0 || len(tt.gh.calls) != tt.calls || tt.gh.set() != nil || !strings.Contains(out, manualNote) || !strings.Contains(out, "gh secret set ZOSIA_SECRET") {
			t.Errorf("%s: exit = %d, gh calls = %q\nstdout: %s\nstderr: %s", name, code, tt.gh.calls, out, errb)
		}
	}

	gh := &fakeGH{repo: ghRepo, secrets: "[]", setErr: errors.New("exit status 1")}
	code, out, _ := zosiaEnable(t, gh, true, "")
	if code != 0 || gh.set() == nil || !strings.Contains(out, "ZOSIA_SECRET was not set") || !strings.Contains(out, manualNote) || strings.Contains(out, "is set for") {
		t.Errorf("gh secret set fails: exit = %d\nstdout: %s", code, out)
	}

	// Another CI system beside GitHub: its variable is set elsewhere, so nothing is offered.
	if err := os.WriteFile(filepath.Join(dir, ".gitlab-ci.yml"), []byte("build:\n  script:\n    - make\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gh = &fakeGH{repo: ghRepo, secrets: "[]"}
	code, out, _ = zosiaEnable(t, gh, true, "")
	if code != 0 || len(gh.calls) != 0 || !strings.Contains(out, manualNote) {
		t.Errorf("with a GitLab file: exit = %d, gh calls = %q\nstdout: %s", code, gh.calls, out)
	}
}

// Without a terminal nothing can be pasted, but gh still tells whether the secret is there: then
// the output says so instead of telling the user to add it.
func TestZosiaEnableSaysThatTheSecretExists(t *testing.T) {
	gatedRepo(t)
	gh := &fakeGH{repo: ghRepo, secrets: `[{"name":"OTHER"},{"name":"ZOSIA_SECRET"}]`}
	code, out, errb := zosiaEnable(t, gh, false, "")
	if code != 0 || errb != "" || gh.set() != nil || strings.Contains(out, manualNote) || strings.Contains(out, "Replace it?") {
		t.Fatalf("exit = %d, gh calls = %q\nstdout: %s\nstderr: %s", code, gh.calls, out, errb)
	}
	for _, s := range []string{"ZOSIA_SECRET is already set for acme/web", "gh secret set ZOSIA_SECRET", "zosia-secret: ${{ secrets.ZOSIA_SECRET }}"} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
}

func TestZosiaDisableNeverRunsGH(t *testing.T) {
	gatedRepo(t)
	gh := &fakeGH{repo: ghRepo, secrets: "[]"}
	d := setupDeps(true, "")
	d.gh = gh.run
	var out, errb bytes.Buffer
	if code := run([]string{"zosia", "disable"}, &out, &errb, d); code != 0 || len(gh.calls) != 0 {
		t.Errorf("exit = %d, gh calls = %q\nstdout: %s", code, gh.calls, out.String())
	}
}

// A gate written by hand runs shearbolt in a run: step. Enable hands that step the secret, and
// its last line says when the release the step pins still cannot send.
func TestZosiaEnableGateWrittenByHand(t *testing.T) {
	dir := fixtureRepo(t, "go-hand-gated")
	workflow := filepath.Join(dir, ".github/workflows/release.yml")
	const (
		env     = "          CANDIDATE_DIR: ${{ runner.temp }}/candidate\n          ZOSIA_SECRET: ${{ secrets.ZOSIA_SECRET }}\n        run: |\n"
		notSent = "No reports are sent yet from .github/workflows/release.yml:51: see the notes above.\n"
	)
	gh := &fakeGH{repo: ghRepo, secrets: "[]"}
	code, out, errb := zosiaEnable(t, gh, true, "")
	after, _ := os.ReadFile(workflow)
	if code != 0 || errb != "" || gh.set() == nil || !strings.Contains(string(after), env) {
		t.Fatalf("exit = %d, gh calls = %q\nstdout: %s\nstderr: %s\nworkflow:\n%s", code, gh.calls, out, errb, after)
	}
	if !strings.Contains(out, "Shearbolt 1.2.3") || !strings.HasSuffix(out, "ZOSIA_SECRET is set for acme/web.\n\n"+notSent) {
		t.Errorf("stdout does not end by saying that the pinned release cannot send:\n%s", out)
	}

	// Without a terminal the same line ends the output.
	if _, out, _ = zosiaEnable(t, &fakeGH{}, false, ""); !strings.HasSuffix(out, notSent) || !strings.Contains(out, "already sends to Zosia") {
		t.Errorf("without a terminal:\n%s", out)
	}

	if err := os.WriteFile(workflow, bytes.Replace(after, []byte("/v1.2.3/"), []byte("/v1.7.0/"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = zosiaEnable(t, &fakeGH{repo: ghRepo, secrets: `[{"name":"ZOSIA_SECRET"}]`}, true, "\n")
	if code != 0 || !strings.Contains(out, "already sends to Zosia") || strings.Contains(out, "No reports are sent") || strings.Contains(out, "1.7.0") {
		t.Errorf("with a release that sends: exit = %d\nstdout: %s", code, out)
	}

	code, out, errb = zosiaCmd(t, "disable")
	off, _ := os.ReadFile(workflow)
	if code != 0 || errb != "" || strings.Contains(string(off), "ZOSIA_SECRET") || !strings.Contains(out, "-          ZOSIA_SECRET: ${{ secrets.ZOSIA_SECRET }}") {
		t.Errorf("disable: exit = %d\nstdout: %s\nstderr: %s\nworkflow:\n%s", code, out, errb, off)
	}
}
