package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/zosia"
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

func TestZosiaPolicyFlag(t *testing.T) {
	dir := fixtureRepo(t, "nextjs-docker")
	if code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes"); code != 0 {
		t.Fatalf("setup: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	const secret, block = "zosia-secret: ${{ secrets.ZOSIA_SECRET }}", "zosia:\n  policy: true\n"
	before := read("shearbolt.yaml")

	code, out, errb := zosiaCmd(t, "enable", "--policy")
	if code != 0 || errb != "" || read("shearbolt.yaml") != before+block || strings.Count(read(".github/workflows/release.yml"), secret) != 1 ||
		!strings.Contains(out, "+zosia:\n+  policy: true") || !strings.Contains(out, "keep one there that you can live with") {
		t.Fatalf("enable --policy: exit = %d\nstdout: %s\nstderr: %s\nshearbolt.yaml:\n%s", code, out, errb, read("shearbolt.yaml"))
	}
	// The policy goes, the reports stay.
	code, out, errb = zosiaCmd(t, "disable", "--policy")
	if code != 0 || errb != "" || read("shearbolt.yaml") != before || strings.Count(read(".github/workflows/release.yml"), secret) != 1 ||
		!strings.Contains(out, "-zosia:\n-  policy: true") || strings.Contains(out, "The secret itself stays") {
		t.Fatalf("disable --policy: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	// Plain disable takes both away.
	if code, _, _ := zosiaCmd(t, "enable", "--policy"); code != 0 {
		t.Fatal("enable --policy again")
	}
	code, out, errb = zosiaCmd(t, "disable")
	if code != 0 || read("shearbolt.yaml") != before || strings.Contains(read(".github/workflows/release.yml"), secret) || !strings.Contains(out, "The secret itself stays") {
		t.Fatalf("disable: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

func TestZosiaUsage(t *testing.T) {
	fixtureRepo(t, "nextjs-docker")
	for _, args := range [][]string{nil, {"connect"}, {"enable", "extra"}, {"--policy"}, {"enable", "--polic"}, {"enable", "--policy", "extra"}, {"enable", "--policy=false"},
		{"enable", "--projects"}, {"enable", "--projects", ""}, {"enable", "--projects", "a/shearbolt.yaml", "--all-projects"}, {"enable", "--secret-name"},
		{"disable", "--connect"}, {"disable", "--projects", "a/shearbolt.yaml"}, {"disable", "--all-projects"}, {"disable", "--secret-name", "x"}} {
		if code, out, errb := zosiaCmd(t, args...); code != 2 || out != "" || !strings.Contains(errb, "shearbolt zosia enable") {
			t.Errorf("%q: exit = %d\nstdout: %s\nstderr: %s", args, code, out, errb)
		}
	}
	if code, out, _ := zosiaCmd(t, "-h"); code != 0 || !strings.Contains(out, "shearbolt zosia enable [--policy] [--projects <path>[,<path>...] | --all-projects] [--connect] [--secret-name <name>]") ||
		!strings.Contains(out, "shearbolt zosia disable [--policy]") || !strings.Contains(out, "1.11.0") || !strings.Contains(out, "1.12.0") || !strings.Contains(out, "3 a selection is needed") {
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

const testSecret = "zosia_AbCdEfGhIjKlMnOpQrStUvWxYzAbCdEfGhIjKlMnOpQrStUvWxYzAbCdEfGhIjKl"

// fakeZosia stands in for Zosia: it records what connect and the plan are asked, and answers
// with a secret, or connectErr, and with plan, or planErr.
type fakeZosia struct {
	asks       []zosia.ConnectAsk
	urls       []string
	connectErr error
	plans      []zosia.PlanRequest
	plan       zosia.PlanAnswer
	planErr    error
}

func (f *fakeZosia) connect(_ context.Context, url, _ string, ask zosia.ConnectAsk, started func(zosia.ConnectRequest)) (string, string, error) {
	f.asks, f.urls = append(f.asks, ask), append(f.urls, url)
	if f.connectErr != nil {
		return "", "", f.connectErr
	}
	started(zosia.ConnectRequest{DeviceCode: "device", UserCode: "BCDF-GHJK", ExpiresIn: 600, Interval: 5})
	return testSecret, ask.SecretName, nil
}

func (f *fakeZosia) room(_ context.Context, r zosia.PlanRequest) (zosia.PlanAnswer, error) {
	f.plans = append(f.plans, r)
	return f.plan, f.planErr
}

// free is a plan of three projects of which used are in use, here those of this repository.
func free(used int, known bool, here ...string) zosia.PlanAnswer {
	three := 3
	return zosia.PlanAnswer{Plan: "Free", MaxProjects: &three, Projects: used, Known: known, Here: here}
}

type enableRun struct {
	gh    *fakeGH
	z     *fakeZosia
	tty   bool
	stdin string
	env   map[string]string
	args  []string
}

func (r enableRun) run(t *testing.T) (int, string, string) {
	t.Helper()
	d := setupDeps(r.tty, r.stdin)
	if r.gh != nil {
		d.gh = r.gh.run
	}
	z := r.z
	if z == nil {
		z = &fakeZosia{}
	}
	d.connectZosia, d.zosiaPlan = z.connect, z.room
	d.getenv = func(k string) string { return r.env[k] }
	d.hostname = func() string { return "laptop" }
	d.gitRemote = func(string) string { return "https://deploy:token123@GitHub.com/acme/web.git" }
	var out, errb bytes.Buffer
	code := run(append([]string{"zosia", "enable"}, r.args...), &out, &errb, d)
	return code, out.String(), errb.String()
}

func zosiaEnable(t *testing.T, gh *fakeGH, tty bool, stdin string) (int, string, string) {
	t.Helper()
	return enableRun{gh: gh, tty: tty, stdin: stdin}.run(t)
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

func stdinOf(t *testing.T, gh *fakeGH) string {
	t.Helper()
	if gh.setStdin == nil {
		return ""
	}
	b, err := io.ReadAll(gh.setStdin)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestZosiaEnableConnectsAndStoresTheSecretWithGH(t *testing.T) {
	gatedRepo(t)
	gh, z := &fakeGH{repo: ghRepo, secrets: `[{"name":"OTHER"}]`}, &fakeZosia{}
	code, out, errb := enableRun{gh: gh, z: z, tty: true, stdin: "\n"}.run(t)
	want := []string{"secret", "set", "ZOSIA_SECRET", "--repo", "acme/web"}
	if code != 0 || errb != "" || !slices.Equal(gh.set(), want) || stdinOf(t, gh) != testSecret {
		t.Fatalf("exit = %d, gh calls = %q\nstdout: %s\nstderr: %s", code, gh.calls, out, errb)
	}
	// What Zosia is told of the repository is its host and path, and nothing else of the remote's URL.
	if len(z.asks) != 1 || z.asks[0] != (zosia.ConnectAsk{Repository: "github.com/acme/web", Hostname: "laptop", Version: "1.2.3", SecretName: "acme/web"}) || z.urls[0] != zosia.DefaultURL {
		t.Fatalf("asked %+v at %q", z.asks, z.urls)
	}
	for _, s := range []string{"Name of the secret in Zosia [acme/web]: ", "https://zosiasec.com/app/connect?code=BCDF-GHJK", "BCDF-GHJK", "ZOSIA_SECRET is set for acme/web.\n", "zosia-secret: ${{ secrets.ZOSIA_SECRET }}"} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
	for _, s := range []string{manualNote, testSecret, "token123", "Replace it?", "ZOSIA_URL"} {
		if strings.Contains(out+errb, s) {
			t.Errorf("the output holds %q:\n%s", s, out)
		}
	}
	if i, j := strings.Index(out, zosiaReview), strings.Index(out, "Name of the secret"); i < 0 || j < i {
		t.Errorf("the question comes before the review line:\n%s", out)
	}
	// The plan's room is asked with that secret, for the repository as its reports will name it.
	if len(z.plans) != 1 || z.plans[0].Secret != testSecret || z.plans[0].Server != "https://github.com" || z.plans[0].Repository != "acme/web" || z.plans[0].URL != zosia.DefaultURL {
		t.Errorf("plan asked %+v", z.plans)
	}

	// A name typed at the question, or given as a flag, is the one proposed.
	gatedRepo(t)
	z = &fakeZosia{}
	enableRun{gh: &fakeGH{repo: ghRepo, secrets: "[]"}, z: z, tty: true, stdin: "  web CI \n"}.run(t)
	if len(z.asks) != 1 || z.asks[0].SecretName != "web CI" {
		t.Errorf("typed name: asked %+v", z.asks)
	}
	gatedRepo(t)
	z = &fakeZosia{}
	_, out, _ = enableRun{gh: &fakeGH{repo: ghRepo, secrets: "[]"}, z: z, tty: true, args: []string{"--secret-name", "from a flag"}}.run(t)
	if len(z.asks) != 1 || z.asks[0].SecretName != "from a flag" || strings.Contains(out, "Name of the secret") {
		t.Errorf("--secret-name: asked %+v\n%s", z.asks, out)
	}
}

func TestZosiaEnableAsksBeforeReplacingTheSecret(t *testing.T) {
	gatedRepo(t)
	const secrets = `[{"name":"ZOSIA_SECRET"}]`
	gh, z := &fakeGH{repo: ghRepo, secrets: secrets}, &fakeZosia{}
	code, out, _ := enableRun{gh: gh, z: z, tty: true, stdin: "\n"}.run(t)
	if code != 0 || gh.set() != nil || len(z.asks) != 0 || len(z.plans) != 0 || !strings.Contains(out, "ZOSIA_SECRET is already set for acme/web. Replace it? [y/N]") ||
		!strings.Contains(out, "Kept the existing ZOSIA_SECRET.") || !strings.Contains(out, "was not checked") || strings.Contains(out, manualNote) {
		t.Fatalf("declined: exit = %d, gh calls = %q, asked %+v\nstdout: %s", code, gh.calls, z.asks, out)
	}
	gh, z = &fakeGH{repo: ghRepo, secrets: secrets}, &fakeZosia{}
	code, out, _ = enableRun{gh: gh, z: z, tty: true, stdin: "y\n\n"}.run(t)
	if code != 0 || gh.set() == nil || stdinOf(t, gh) != testSecret || !strings.Contains(out, "ZOSIA_SECRET is set for acme/web.") ||
		!strings.Contains(out, "stays valid until you revoke it at https://zosiasec.com/app/secrets") {
		t.Fatalf("accepted: exit = %d, gh calls = %q\nstdout: %s", code, gh.calls, out)
	}
}

func TestZosiaEnableWhereGHCannotStoreTheSecret(t *testing.T) {
	dir := gatedRepo(t)
	const ask = "Make a secret for this repository in Zosia now? [Y/n]"
	tests := map[string]struct {
		gh    *fakeGH
		calls int
	}{
		"gh fails":          {&fakeGH{viewErr: errors.New("exit status 4")}, 1},
		"read access only":  {&fakeGH{repo: `{"nameWithOwner":"acme/web","viewerPermission":"READ"}`, secrets: "[]"}, 1},
		"gh prints no JSON": {&fakeGH{repo: "", secrets: "[]"}, 1},
		"secret list fails": {&fakeGH{repo: ghRepo, secrets: "HTTP 403"}, 2},
	}
	for name, tt := range tests {
		// Whether a secret is there cannot be told, so it asks before it makes one. No: the note stays.
		z := &fakeZosia{}
		code, out, errb := enableRun{gh: tt.gh, z: z, tty: true, stdin: "n\n"}.run(t)
		if code != 0 || len(tt.gh.calls) != tt.calls || tt.gh.set() != nil || len(z.asks) != 0 || !strings.Contains(out, ask) || !strings.Contains(out, manualNote) {
			t.Errorf("%s, declined: exit = %d, gh calls = %q\nstdout: %s\nstderr: %s", name, code, tt.gh.calls, out, errb)
		}
	}
	// Yes: the secret is made and shown once, with where it goes.
	z := &fakeZosia{}
	code, out, _ := enableRun{gh: &fakeGH{viewErr: errors.New("exit status 4")}, z: z, tty: true, stdin: "\n\n"}.run(t)
	if code != 0 || len(z.asks) != 1 || strings.Count(out, testSecret) != 1 || !strings.Contains(out, "It is shown this once") || !strings.Contains(out, "repository secret ZOSIA_SECRET") {
		t.Errorf("accepted: exit = %d, asked %+v\nstdout: %s", code, z.asks, out)
	}

	// Without a terminal nothing is asked and nothing is made.
	gh := &fakeGH{repo: ghRepo, secrets: "[]"}
	z = &fakeZosia{}
	code, out, _ = enableRun{gh: gh, z: z}.run(t)
	if code != 0 || len(gh.calls) != 2 || gh.set() != nil || len(z.asks) != 0 || !strings.Contains(out, manualNote) || !strings.Contains(out, "gh secret set ZOSIA_SECRET") {
		t.Errorf("no terminal: exit = %d, gh calls = %q\nstdout: %s", code, gh.calls, out)
	}

	// gh cannot store a secret that was made: on a terminal it is shown once.
	gh = &fakeGH{repo: ghRepo, secrets: "[]", setErr: errors.New("exit status 1")}
	code, out, _ = enableRun{gh: gh, tty: true, stdin: "\n"}.run(t)
	if code != 0 || gh.set() == nil || !strings.Contains(out, "ZOSIA_SECRET was not set") || strings.Count(out, testSecret) != 1 || strings.Contains(out, "is set for") {
		t.Errorf("gh secret set fails: exit = %d\nstdout: %s", code, out)
	}

	// Another CI system beside GitHub: gh is not asked, and the secret goes to the user.
	if err := os.WriteFile(filepath.Join(dir, ".gitlab-ci.yml"), []byte("build:\n  script:\n    - make\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gh = &fakeGH{repo: ghRepo, secrets: "[]"}
	code, out, _ = enableRun{gh: gh, tty: true, stdin: "y\n\n"}.run(t)
	if code != 0 || len(gh.calls) != 0 || !strings.Contains(out, ask) || strings.Count(out, testSecret) != 1 {
		t.Errorf("with a GitLab file: exit = %d, gh calls = %q\nstdout: %s", code, gh.calls, out)
	}
}

func TestZosiaEnableWhenConnectingFails(t *testing.T) {
	gatedRepo(t)
	denied := &zosia.Error{Code: "DENIED", Message: "The request was denied in Zosia."}
	gh := &fakeGH{repo: ghRepo, secrets: "[]"}
	code, out, _ := enableRun{gh: gh, z: &fakeZosia{connectErr: denied}, tty: true, stdin: "\n"}.run(t)
	if code != 0 || gh.set() != nil || !strings.Contains(out, "No secret was made (DENIED): The request was denied in Zosia.") || !strings.Contains(out, manualNote) {
		t.Errorf("on a terminal: exit = %d\nstdout: %s", code, out)
	}
	// Asked for by flag, it is the command's failure.
	code, out, errb := enableRun{gh: &fakeGH{repo: ghRepo, secrets: "[]"}, z: &fakeZosia{connectErr: denied}, args: []string{"--connect"}}.run(t)
	if code != 2 || !strings.Contains(out+errb, "No secret was made (DENIED)") {
		t.Errorf("--connect: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

func TestZosiaEnableConnectFlagWithoutATerminal(t *testing.T) {
	gatedRepo(t)
	// It asks nothing: the name is the default, and a secret that is there is replaced.
	gh, z := &fakeGH{repo: ghRepo, secrets: `[{"name":"ZOSIA_SECRET"}]`}, &fakeZosia{}
	code, out, errb := enableRun{gh: gh, z: z, args: []string{"--connect"}}.run(t)
	if code != 0 || errb != "" || stdinOf(t, gh) != testSecret || len(z.asks) != 1 || z.asks[0].SecretName != "acme/web" ||
		strings.Contains(out, "Replace it?") || strings.Contains(out, "Name of the secret") || !strings.Contains(out, "replaced") || strings.Contains(out, testSecret) {
		t.Fatalf("exit = %d, asked %+v\nstdout: %s\nstderr: %s", code, z.asks, out, errb)
	}
	// The link and the code are the first lines of the step, for whoever passes them on.
	if i, j := strings.Index(out, "https://zosiasec.com/app/connect?code=BCDF-GHJK"), strings.Index(out, "ZOSIA_SECRET is set"); i < 0 || j < i {
		t.Errorf("the link does not come first:\n%s", out)
	}
	// Nowhere to store it: it does not start, since a log is no place for a secret.
	z = &fakeZosia{}
	code, out, errb = enableRun{gh: &fakeGH{viewErr: errors.New("exit status 4")}, z: z, args: []string{"--connect"}}.run(t)
	if code != 2 || len(z.asks) != 0 || !strings.Contains(errb, "--connect") || !strings.Contains(errb, "GitHub CLI") {
		t.Errorf("no gh: exit = %d, asked %+v\nstdout: %s\nstderr: %s", code, z.asks, out, errb)
	}
	// Made and not stored: the secret is named, never printed.
	gh = &fakeGH{repo: ghRepo, secrets: "[]", setErr: errors.New("exit status 1")}
	code, out, errb = enableRun{gh: gh, args: []string{"--connect", "--secret-name", "web CI"}}.run(t)
	if code != 2 || strings.Contains(out+errb, testSecret) || !strings.Contains(errb, `"web CI"`) || !strings.Contains(errb, "Revoke it at https://zosiasec.com/app/secrets") {
		t.Errorf("gh secret set fails: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

func TestZosiaEnableWithTheSecretOfTheEnvironment(t *testing.T) {
	gatedRepo(t)
	for _, tty := range []bool{true, false} {
		gh, z := &fakeGH{repo: ghRepo, secrets: "[]"}, &fakeZosia{}
		code, out, _ := enableRun{gh: gh, z: z, tty: tty, env: map[string]string{"ZOSIA_SECRET": " zosia_env \n"}}.run(t)
		if code != 0 || gh.set() != nil || len(z.asks) != 0 || len(z.plans) != 1 || z.plans[0].Secret != "zosia_env" || strings.Contains(out, "zosia_env") || !strings.Contains(out, manualNote) {
			t.Errorf("terminal %v: exit = %d, asked %+v, plans %+v\nstdout: %s", tty, code, z.asks, z.plans, out)
		}
	}
}

func TestZosiaEnableSaysWhenAnotherAddressIsUsed(t *testing.T) {
	gatedRepo(t)
	z := &fakeZosia{}
	code, out, _ := enableRun{gh: &fakeGH{repo: ghRepo, secrets: "[]"}, z: z, tty: true, stdin: "\n", env: map[string]string{"ZOSIA_URL": "http://127.0.0.1:3000/"}}.run(t)
	if code != 0 || len(z.urls) != 1 || z.urls[0] != "http://127.0.0.1:3000/" || len(z.plans) != 1 || z.plans[0].URL != "http://127.0.0.1:3000/" ||
		!strings.Contains(out, "http://127.0.0.1:3000/app/connect?code=BCDF-GHJK") || strings.Contains(out, "zosiasec.com/app/connect") {
		t.Fatalf("exit = %d, urls %q, plans %+v\nstdout: %s", code, z.urls, z.plans, out)
	}
	if i, j := strings.Index(out, "ZOSIA_URL is set"), strings.Index(out, "app/connect?code="); i < 0 || j < i || !strings.Contains(out, "127.0.0.1:3000, not https://zosiasec.com") {
		t.Errorf("it does not say so before the link:\n%s", out)
	}
}

func TestRemoteNameIsHostAndPathOnly(t *testing.T) {
	for url, want := range map[string][2]string{
		"https://github.com/acme/shop.git":                  {"github.com", "acme/shop"},
		"https://deploy:s3cret@GitHub.com:443/acme/shop/\n": {"github.com", "acme/shop"},
		"https://gitlab.example.org/group/sub/app.git?x=1":  {"gitlab.example.org", "group/sub/app"},
		"git@github.com:acme/shop.git":                      {"github.com", "acme/shop"},
		"ssh://git@bitbucket.org:22/acme/shop.git":          {"bitbucket.org", "acme/shop"},
		"":                 {"", ""},
		"../elsewhere":     {"", ""},
		"/srv/git/shop":    {"", ""},
		"https://host/":    {"", ""},
		"git@host:a b.git": {"", ""},
		"file:///srv/shop": {"", ""},
	} {
		if host, path := remoteName(url); host != want[0] || path != want[1] {
			t.Errorf("%q: %q %q, want %q", url, host, path, want)
		}
	}
}

// monoRepo is the monorepo fixture after setup and enable: three projects, all sending.
func monoRepo(t *testing.T) string {
	t.Helper()
	dir := fixtureRepo(t, "monorepo")
	if code, out, errb := setupCmd(t, setupDeps(false, ""), "--yes"); code != 0 {
		t.Fatalf("setup: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if code, out, errb := zosiaCmd(t, "enable"); code != 0 {
		t.Fatalf("enable: exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	return dir
}

func off(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, rel := range []string{"apps/api/shearbolt.yaml", "apps/web/shearbolt.yaml", "crates/cli/shearbolt.yaml"} {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "zosia:\n  send: false\n") {
			out = append(out, rel)
		}
	}
	return out
}

var envSecret = map[string]string{"ZOSIA_SECRET": "zosia_env"}

func TestZosiaEnableAsksNothingWhenEveryProjectFits(t *testing.T) {
	dir := monoRepo(t)
	for name, plan := range map[string]zosia.PlanAnswer{
		"room for all":           free(0, false),
		"all of them are there":  free(3, true, "apps/api/shearbolt.yaml", "apps/web/shearbolt.yaml", "crates/cli/shearbolt.yaml"),
		"a plan without a limit": {Plan: "Unlimited", Projects: 40},
	} {
		code, out, errb := enableRun{z: &fakeZosia{plan: plan}, tty: true, env: envSecret}.run(t)
		if code != 0 || errb != "" || len(off(t, dir)) != 0 || strings.Contains(out, "Select the projects") {
			t.Errorf("%s: exit = %d, off %v\nstdout: %s\nstderr: %s", name, code, off(t, dir), out, errb)
		}
	}
	// The plan cannot be read: enable stands, and says that the room was not checked.
	code, out, _ := enableRun{z: &fakeZosia{planErr: &zosia.Error{Code: "NETWORK", Message: "cannot reach Zosia: timeout"}}, tty: true, env: envSecret}.run(t)
	if code != 0 || len(off(t, dir)) != 0 || !strings.Contains(out, "was not checked (NETWORK): cannot reach Zosia: timeout") {
		t.Errorf("no plan: exit = %d\nstdout: %s", code, out)
	}
}

func TestZosiaEnableLetsTheUserSelectTheProjectsThatSend(t *testing.T) {
	dir := monoRepo(t)
	// Two of the plan's three places are in use by other repositories; the web project has the third.
	z := &fakeZosia{plan: free(3, true, "apps/web/shearbolt.yaml")}
	code, out, errb := enableRun{z: z, tty: true, env: envSecret, stdin: "nine\n4\n1,2,3\n 2 \n"}.run(t)
	if code != 0 || errb != "" || !slices.Equal(off(t, dir), []string{"apps/api/shearbolt.yaml", "crates/cli/shearbolt.yaml"}) {
		t.Fatalf("exit = %d, off %v\nstdout: %s\nstderr: %s", code, off(t, dir), out, errb)
	}
	for _, s := range []string{
		"This repository has 3 projects that send to Zosia, and your Free plan allows 3 projects (2 are in use by other repositories). Select the projects that send their reports.",
		"  1. api  apps/api/shearbolt.yaml\n", "  2. web  apps/web/shearbolt.yaml  (in Zosia already)\n", "  3. cli  crates/cli/shearbolt.yaml\n",
		"Give numbers from the list, such as 1,3.", "That is 3 projects, of which 2 need a place, and the plan has room for none.",
		"+zosia:\n+  send: false", "apps/api/shearbolt.yaml", zosiaReview,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "Zosia has no projects of") {
		t.Errorf("it says the repository is not known:\n%s", out)
	}
	// Run again with the same answer from Zosia: what does not send needs no place, so nothing is asked.
	code, out, _ = enableRun{z: z, tty: true, env: envSecret}.run(t)
	if code != 0 || strings.Contains(out, "Select the projects") || len(off(t, dir)) != 2 {
		t.Errorf("again: exit = %d, off %v\nstdout: %s", code, off(t, dir), out)
	}
}

func TestZosiaEnableSelectionCanBeSkipped(t *testing.T) {
	dir := monoRepo(t)
	// Zosia does not know the repository, which may be one it has under another name.
	code, out, _ := enableRun{z: &fakeZosia{plan: free(2, false)}, tty: true, env: envSecret, stdin: "\n"}.run(t)
	if code != 0 || len(off(t, dir)) != 0 || !strings.Contains(out, "Zosia has no projects of github.com/acme/web yet.") || !strings.Contains(out, "Nothing was changed") {
		t.Errorf("exit = %d, off %v\nstdout: %s", code, off(t, dir), out)
	}
	// The input ends: the same.
	if code, _, _ := (enableRun{z: &fakeZosia{plan: free(2, false)}, tty: true, env: envSecret}).run(t); code != 0 || len(off(t, dir)) != 0 {
		t.Errorf("input ended: exit = %d, off %v", code, off(t, dir))
	}
}

func TestZosiaEnableWithoutATerminalEndsWithExitCode3WhenASelectionIsNeeded(t *testing.T) {
	dir := monoRepo(t)
	code, out, errb := enableRun{z: &fakeZosia{plan: free(2, false)}, env: envSecret}.run(t)
	if code != 3 || len(off(t, dir)) != 0 {
		t.Fatalf("exit = %d, off %v\nstdout: %s\nstderr: %s", code, off(t, dir), out, errb)
	}
	for _, s := range []string{"This repository has 3 projects that send to Zosia, and your Free plan allows 3 projects (2 are in use by other repositories).",
		"  1. api  apps/api/shearbolt.yaml\n", "shearbolt zosia enable --projects"} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
}

func TestZosiaEnableProjectsFlags(t *testing.T) {
	dir := monoRepo(t)
	z := &fakeZosia{plan: free(2, false)}
	// With the plan at hand, a choice that does not fit writes nothing.
	code, out, errb := enableRun{z: z, env: envSecret, args: []string{"--projects", "apps/api/shearbolt.yaml,apps/web/shearbolt.yaml"}}.run(t)
	if code != 2 || len(off(t, dir)) != 0 || !strings.Contains(errb, "That is 2 projects, of which 2 need a place, and the plan has room for 1.") {
		t.Fatalf("too many: exit = %d, off %v\nstdout: %s\nstderr: %s", code, off(t, dir), out, errb)
	}
	code, _, errb = enableRun{z: z, env: envSecret, args: []string{"--projects", "docs/shearbolt.yaml"}}.run(t)
	if code != 2 || len(off(t, dir)) != 0 || !strings.Contains(errb, "docs/shearbolt.yaml is not a shearbolt.yaml that a gate of this repository reads") {
		t.Fatalf("unknown: exit = %d\nstderr: %s", code, errb)
	}
	code, out, errb = enableRun{z: z, env: envSecret, args: []string{"--projects=./apps/api/shearbolt.yaml"}}.run(t)
	if code != 0 || errb != "" || !slices.Equal(off(t, dir), []string{"apps/web/shearbolt.yaml", "crates/cli/shearbolt.yaml"}) || !strings.Contains(out, "+zosia:\n+  send: false") {
		t.Fatalf("one: exit = %d, off %v\nstdout: %s\nstderr: %s", code, off(t, dir), out, errb)
	}
	// Without a secret there is no plan to hold the choice against: it is applied as given.
	z = &fakeZosia{}
	code, _, errb = enableRun{z: z, args: []string{"--projects", "apps/api/shearbolt.yaml,apps/web/shearbolt.yaml"}}.run(t)
	if code != 0 || len(z.plans) != 0 || !slices.Equal(off(t, dir), []string{"crates/cli/shearbolt.yaml"}) {
		t.Fatalf("without a plan: exit = %d, off %v\nstderr: %s", code, off(t, dir), errb)
	}
	code, out, errb = enableRun{args: []string{"--all-projects"}}.run(t)
	if code != 0 || len(off(t, dir)) != 0 || !strings.Contains(out, "-zosia:\n-  send: false") {
		t.Fatalf("--all-projects: exit = %d, off %v\nstdout: %s\nstderr: %s", code, off(t, dir), out, errb)
	}
}

func TestZosiaEnableOneProjectWithoutRoom(t *testing.T) {
	gatedRepo(t)
	if code, _, _ := zosiaCmd(t, "enable"); code != 0 {
		t.Fatal("enable")
	}
	code, out, _ := enableRun{z: &fakeZosia{plan: free(3, false)}, tty: true, env: envSecret}.run(t)
	if code != 0 || strings.Contains(out, "Select the projects") ||
		!strings.Contains(out, "Your Free plan in Zosia allows 3 projects, and all of them are in use. This project's reports are refused until you remove a project in Zosia or change your plan.") {
		t.Errorf("exit = %d\nstdout: %s", code, out)
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
	code, out, errb := zosiaEnable(t, gh, true, "\n")
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
