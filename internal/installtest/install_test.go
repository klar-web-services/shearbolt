package installtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

const (
	linuxAmd64  = "x86_64-unknown-linux-gnu"
	linuxArm64  = "aarch64-unknown-linux-gnu"
	darwinAmd64 = "x86_64-apple-darwin"
	darwinArm64 = "aarch64-apple-darwin"
)

var allTriples = []string{linuxAmd64, linuxArm64, darwinAmd64, darwinArm64}

// baseTools is install.sh's documented dependency list, without a fetcher and a SHA-256 tool.
// The child PATH holds only these (plus the fetchers a case adds), so a new dependency fails.
var baseTools = []string{"sh", "mktemp", "rm", "mkdir", "tar", "gzip", "awk", "find", "head", "tr", "install", "mv", "cat"}

// toolsWithHash is the base list plus the given SHA-256 tool and fetchers.
func toolsWithHash(hash string, fetchers ...string) []string {
	return append(append(slices.Clone(baseTools), hash), fetchers...)
}

// tools is the base list plus sha256sum (or shasum where sha256sum is missing) and the given fetchers.
func tools(fetchers ...string) []string {
	hash := "sha256sum"
	if _, err := exec.LookPath(hash); err != nil {
		hash = "shasum"
	}
	return toolsWithHash(hash, fetchers...)
}

// needTool skips a case on hosts without the tool, except under CI, where it fails.
func needTool(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is required under CI: %v", tool, err)
		}
		t.Skipf("%s is not installed: %v", tool, err)
	}
}

func needWget(t *testing.T) {
	t.Helper()
	needTool(t, "wget")
}

// eachHash runs f once with only sha256sum and once with only shasum on the child's PATH.
func eachHash(t *testing.T, f func(t *testing.T, hash string)) {
	for _, hash := range []string{"sha256sum", "shasum"} {
		t.Run(hash, func(t *testing.T) {
			needTool(t, hash)
			f(t, hash)
		})
	}
}

// eachFetcher runs f once with curl and once with wget only.
func eachFetcher(t *testing.T, f func(t *testing.T, fetcher string)) {
	for _, fetcher := range []string{"curl", "wget"} {
		t.Run(fetcher, func(t *testing.T) {
			if fetcher == "wget" {
				needWget(t)
			}
			f(t, fetcher)
		})
	}
}

func installScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

// fakeRelease writes latest and v<version>/ with one archive per triple and checksums.txt.
// Each archive holds an executable shearbolt script printing "shearbolt <version> <triple>".
func fakeRelease(t *testing.T, version string, triples ...string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "latest"), []byte(version+"\n"), 0o644)
	addRelease(t, dir, version, triples...)
	return dir
}

// addRelease adds v<version>/ to a fake release without touching latest.
func addRelease(t *testing.T, dir, version string, triples ...string) {
	t.Helper()
	var sums strings.Builder
	for _, triple := range slices.Sorted(slices.Values(triples)) {
		name := "shearbolt-" + triple + ".tar.gz"
		data := archive(t, fmt.Sprintf("#!/bin/sh\necho 'shearbolt %s %s'\n", version, triple))
		writeFile(t, filepath.Join(dir, "v"+version, name), data, 0o644)
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	writeFile(t, filepath.Join(dir, "v"+version, "checksums.txt"), []byte(sums.String()), 0o644)
}

func archive(t *testing.T, script string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "shearbolt", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg, ModTime: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// setChecksums rewrites checksums.txt of a version with the given lines.
func setChecksums(t *testing.T, dir, version string, lines ...string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "v"+version, "checksums.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func sumOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// server serves a fake release on 127.0.0.1 and records every request path.
type server struct {
	URL   string
	mu    sync.Mutex
	paths []string
}

func serve(t *testing.T, dir string) *server {
	t.Helper()
	s := &server{}
	files := http.FileServer(http.Dir(dir))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.mu.Unlock()
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	if !strings.HasPrefix(ts.URL, "http://127.0.0.1:") {
		t.Fatalf("test server is not on 127.0.0.1: %s", ts.URL)
	}
	s.URL = ts.URL
	return s
}

func (s *server) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.paths)
}

func (s *server) archives() []string {
	var out []string
	for _, p := range s.requests() {
		if strings.HasSuffix(p, ".tar.gz") {
			out = append(out, p)
		}
	}
	return out
}

const unameShim = `#!/bin/sh
case "$1" in
-s) printf '%s\n' "$FAKE_UNAME_S" ;;
-m) printf '%s\n' "$FAKE_UNAME_M" ;;
*) printf 'uname shim: unsupported arguments: %s\n' "$*" >&2; exit 1 ;;
esac
`

type result struct {
	code           int // -1 when the shell died from a signal
	stdout, stderr string
	home           string   // empty when HOME was unset
	mktemp         []string // the argument lists mktemp was called with
}

type invocation struct {
	env    map[string]string // only SHEARBOLT_* and FAKE_UNAME_* variables
	tools  []string          // host tools linked into the child's PATH
	path   []string          // extra PATH entries after the tool directory
	stdin  []byte            // when set, the script is piped to sh instead of passed as a file
	tmpdir string            // TMPDIR, absolute or relative to the working directory (default: a new temp dir)
	noHome bool              // leave HOME unset
}

// prepared is a configured installer run that has not started yet.
type prepared struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	home           string
	tmp            string // TMPDIR, absolute
	tmpVar         string // TMPDIR as the child sees it
	mktempLog      string
}

// runInstall runs sh install.sh with the given SHEARBOLT_* / FAKE_UNAME_* variables and host tools.
func runInstall(t *testing.T, env map[string]string, tools []string) result {
	t.Helper()
	return run(t, invocation{env: env, tools: tools})
}

// run executes install.sh to completion; a death by signal fails the test.
func run(t *testing.T, inv invocation) result {
	t.Helper()
	p := prepare(t, inv)
	r := p.finish(t, p.cmd.Run())
	if r.code < 0 {
		t.Fatalf("install.sh died from a signal\nstderr:\n%s", r.stderr)
	}
	return r
}

// prepare builds an installer run with an environment built from scratch: PATH (a directory of
// symlinks to the listed host tools, a uname shim and a recording mktemp shim), HOME, TMPDIR,
// FAKE_UNAME_* and the case's SHEARBOLT_* variables. Nothing is inherited, so no proxy variable
// reaches curl or wget.
func prepare(t *testing.T, inv invocation) *prepared {
	t.Helper()
	p := &prepared{home: t.TempDir(), mktempLog: filepath.Join(t.TempDir(), "mktemp.log")}
	bin, work := t.TempDir(), t.TempDir()
	for _, tool := range inv.tools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("host tool %s: %v", tool, err)
		}
		if tool == "mktemp" {
			shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(p.mktempLog) + "\nexec " + shellQuote(path) + " \"$@\"\n"
			writeFile(t, filepath.Join(bin, tool), []byte(shim), 0o755)
			continue
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(bin, "uname"), []byte(unameShim), 0o755)

	switch {
	case inv.tmpdir == "":
		p.tmp = t.TempDir()
		p.tmpVar = p.tmp
	case filepath.IsAbs(inv.tmpdir):
		p.tmp, p.tmpVar = inv.tmpdir, inv.tmpdir
	default:
		p.tmp, p.tmpVar = filepath.Join(work, inv.tmpdir), inv.tmpdir
	}
	if err := os.MkdirAll(p.tmp, 0o755); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{"FAKE_UNAME_S": "Linux", "FAKE_UNAME_M": "x86_64"}
	for k, v := range inv.env {
		if !strings.HasPrefix(k, "SHEARBOLT_") && !strings.HasPrefix(k, "FAKE_UNAME_") {
			t.Fatalf("variable %s may not reach the installer", k)
		}
		vars[k] = v
	}
	vars["PATH"] = strings.Join(append([]string{bin}, inv.path...), ":")
	vars["TMPDIR"] = p.tmpVar
	if inv.noHome {
		p.home = ""
	} else {
		vars["HOME"] = p.home
	}
	env := make([]string, 0, len(vars))
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		env = append(env, k+"="+vars[k])
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	p.cmd = exec.CommandContext(ctx, sh, installScript(t))
	if inv.stdin != nil {
		p.cmd = exec.CommandContext(ctx, sh)
		p.cmd.Stdin = bytes.NewReader(inv.stdin)
	}
	p.cmd.Env = env
	p.cmd.Dir = work
	p.cmd.WaitDelay = 5 * time.Second
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	return p
}

// finish turns the outcome of Run or Wait into a result. TMPDIR must be empty afterwards, and
// every mktemp call must have created its directory inside TMPDIR.
func (p *prepared) finish(t *testing.T, err error) result {
	t.Helper()
	r := result{home: p.home, stdout: p.stdout.String(), stderr: p.stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running install.sh: %v\nstderr:\n%s", err, r.stderr)
		}
		r.code = exit.ExitCode()
	}
	if left, err := os.ReadDir(p.tmp); err != nil || len(left) > 0 {
		t.Errorf("TMPDIR not cleaned up: %v %v", names(left), err)
	}
	if log, err := os.ReadFile(p.mktempLog); err == nil {
		r.mktemp = strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, args := range r.mktemp {
		if want := "-d " + p.tmpVar + "/shearbolt-install.XXXXXX"; args != want {
			t.Errorf("mktemp called with %q, want %q", args, want)
		}
	}
	return r
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// wantOK checks a successful run: messages on stdout, nothing on stderr (the Action discards
// stdout, so stderr carries only errors).
func wantOK(t *testing.T, r result) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\nstderr:\n%s", r.code, r.stderr)
	}
	if r.stderr != "" {
		t.Fatalf("a successful run wrote to stderr:\n%s", r.stderr)
	}
	if !strings.Contains(r.stdout, "Installed shearbolt ") {
		t.Fatalf("stdout lacks the install line:\n%s", r.stdout)
	}
}

func wantFail(t *testing.T, r result, msg string) {
	t.Helper()
	if r.code == 0 {
		t.Fatalf("exit 0, want failure with %q\nstderr:\n%s", msg, r.stderr)
	}
	if !strings.Contains(r.stderr, msg) {
		t.Fatalf("stderr lacks %q:\n%s", msg, r.stderr)
	}
	if strings.Contains(r.stderr+r.stdout, "Installed shearbolt") {
		t.Fatalf("failed run claims an install:\n%s%s", r.stdout, r.stderr)
	}
	if r.stdout != "" {
		t.Fatalf("a failed run wrote to stdout:\n%s", r.stdout)
	}
}

// wantInstalled checks that path is a 0755 regular file that prints want.
func wantInstalled(t *testing.T, path, want string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("not installed: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("%s has mode %v, want a regular file with 0755", path, info.Mode())
	}
	cmd := exec.Command(path)
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running %s: %v", path, err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("installed binary prints %q, want %q", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("install dir holds %v, want only shearbolt", names(entries))
	}
}

// wantUntouched checks that nothing was installed: dir does not exist when previous is nil,
// otherwise it holds only the previous shearbolt, byte-identical.
func wantUntouched(t *testing.T, dir string, previous []byte) {
	t.Helper()
	if previous == nil {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("install dir %s exists after a failed install (%v)", dir, err)
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(entries); !slices.Equal(got, []string{"shearbolt"}) {
		t.Fatalf("install dir holds %v, want only the previous shearbolt", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "shearbolt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, previous) {
		t.Fatalf("previous shearbolt changed: %q", data)
	}
}

func names(entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// installDir is a new, not yet existing install dir whose path contains a space.
func installDir(t *testing.T) string {
	return filepath.Join(t.TempDir(), "my bin")
}

// previousInstall puts an older shearbolt into a new install dir and returns both.
func previousInstall(t *testing.T) (string, []byte) {
	t.Helper()
	dir := installDir(t)
	old := []byte("#!/bin/sh\necho 'shearbolt 0.9.0 old'\n")
	writeFile(t, filepath.Join(dir, "shearbolt"), old, 0o755)
	return dir, old
}

type platform struct{ s, m, triple string }

var platforms = []platform{
	{"Linux", "x86_64", linuxAmd64},
	{"Linux", "aarch64", linuxArm64},
	{"Darwin", "x86_64", darwinAmd64},
	{"Darwin", "arm64", darwinArm64},
}

func checkPlatform(t *testing.T, fetcher string, p platform) {
	t.Helper()
	srv := serve(t, fakeRelease(t, "1.2.3", allTriples...))
	dir := installDir(t)
	r := runInstall(t, map[string]string{
		"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir,
		"FAKE_UNAME_S": p.s, "FAKE_UNAME_M": p.m,
	}, tools(fetcher))
	wantOK(t, r)
	if got, want := srv.archives(), []string{"/v1.2.3/shearbolt-" + p.triple + ".tar.gz"}; !slices.Equal(got, want) {
		t.Fatalf("archives requested = %v, want %v", got, want)
	}
	wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+p.triple)
	if !strings.Contains(r.stdout, "Installed shearbolt 1.2.3") {
		t.Fatalf("stdout lacks the install line:\n%s", r.stdout)
	}
	if len(r.mktemp) != 1 { // so the empty-TMPDIR check in finish covers a real temp dir
		t.Fatalf("mktemp calls = %q, want exactly one", r.mktemp)
	}
}

// 1. Platforms (curl).
func TestInstallPlatformsCurl(t *testing.T) {
	for _, p := range platforms {
		t.Run(p.s+"-"+p.m, func(t *testing.T) { checkPlatform(t, "curl", p) })
	}
}

// 2. wget only: curl is absent from PATH.
func TestInstallWgetOnly(t *testing.T) {
	needWget(t)
	checkPlatform(t, "wget", platforms[0])
}

// 3. Latest vs pinned.
func TestInstallLatestVsPinned(t *testing.T) {
	release := func(t *testing.T) string {
		dir := fakeRelease(t, "1.2.3", linuxAmd64)
		addRelease(t, dir, "1.0.0", linuxAmd64)
		return dir
	}
	t.Run("latest", func(t *testing.T) {
		srv := serve(t, release(t))
		dir := installDir(t)
		r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL + "/", "SHEARBOLT_INSTALL_DIR": dir}, tools("curl"))
		wantOK(t, r)
		wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64)
		want := []string{"/latest", "/v1.2.3/checksums.txt", "/v1.2.3/shearbolt-" + linuxAmd64 + ".tar.gz"}
		if got := srv.requests(); !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want %v", got, want)
		}
	})
	t.Run("pinned", func(t *testing.T) {
		srv := serve(t, release(t))
		dir := installDir(t)
		r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir, "SHEARBOLT_VERSION": " v1.0.0\n"}, tools("curl"))
		wantOK(t, r)
		wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.0.0 "+linuxAmd64)
		if slices.Contains(srv.requests(), "/latest") {
			t.Fatalf("latest requested for a pinned version: %v", srv.requests())
		}
		if !strings.Contains(r.stdout, "Installed shearbolt 1.0.0 to "+dir+"/shearbolt") {
			t.Fatalf("stdout:\n%s", r.stdout)
		}
	})
	t.Run("latest with v and CRLF", func(t *testing.T) {
		rel := release(t)
		writeFile(t, filepath.Join(rel, "latest"), []byte("v1.0.0\r\n"), 0o644)
		srv := serve(t, rel)
		dir := installDir(t)
		wantOK(t, runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools("curl")))
		wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.0.0 "+linuxAmd64)
	})
}

// 4. Default dir.
func TestInstallDefaultDir(t *testing.T) {
	srv := serve(t, fakeRelease(t, "1.2.3", linuxAmd64))
	r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL}, tools("curl"))
	wantOK(t, r)
	wantInstalled(t, filepath.Join(r.home, ".local", "bin", "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64)
}

// 5. Checksum mismatch.
func TestInstallChecksumMismatch(t *testing.T) {
	eachFetcher(t, func(t *testing.T, fetcher string) {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
				rel := fakeRelease(t, "1.2.3", linuxAmd64)
				setChecksums(t, rel, "1.2.3", fmt.Sprintf("%x  shearbolt-%s.tar.gz", sha256.Sum256([]byte("other bytes")), linuxAmd64))
				srv := serve(t, rel)
				dir, previous := installDir(t), []byte(nil)
				if existing {
					dir, previous = previousInstall(t)
				}
				r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools(fetcher))
				wantFail(t, r, "checksum verification failed")
				wantUntouched(t, dir, previous)
			})
		}
	})
}

// 6. Corrupt download: the archive is truncated after its checksum was computed.
func TestInstallCorruptDownload(t *testing.T) {
	eachFetcher(t, func(t *testing.T, fetcher string) {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
				rel := fakeRelease(t, "1.2.3", linuxAmd64)
				path := filepath.Join(rel, "v1.2.3", "shearbolt-"+linuxAmd64+".tar.gz")
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, info.Size()/2); err != nil {
					t.Fatal(err)
				}
				srv := serve(t, rel)
				dir, previous := installDir(t), []byte(nil)
				if existing {
					dir, previous = previousInstall(t)
				}
				r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools(fetcher))
				wantFail(t, r, "checksum verification failed")
				wantUntouched(t, dir, previous)
			})
		}
	})
}

// 7. No build for the platform.
func TestInstallNoBuild(t *testing.T) {
	srv := serve(t, fakeRelease(t, "1.2.3", linuxAmd64, linuxArm64, darwinAmd64))
	dir := installDir(t)
	r := runInstall(t, map[string]string{
		"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir,
		"FAKE_UNAME_S": "Darwin", "FAKE_UNAME_M": "arm64",
	}, tools("curl"))
	wantFail(t, r, "no build of shearbolt 1.2.3 for aarch64-apple-darwin")
	wantUntouched(t, dir, nil)
	if got := srv.archives(); len(got) > 0 {
		t.Fatalf("archives requested without a checksum line: %v", got)
	}
}

// 8. Unknown version (404).
func TestInstallUnknownVersion(t *testing.T) {
	eachFetcher(t, func(t *testing.T, fetcher string) {
		srv := serve(t, fakeRelease(t, "1.2.3", allTriples...))
		dir := installDir(t)
		r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir, "SHEARBOLT_VERSION": "9.9.9"}, tools(fetcher))
		wantFail(t, r, "shearbolt 9.9.9 not found")
		wantUntouched(t, dir, nil)
		if got := srv.requests(); !slices.Equal(got, []string{"/v9.9.9/checksums.txt"}) {
			t.Fatalf("requests = %v", got)
		}
	})
}

// 9. Unsupported platform: fails before any download.
func TestInstallUnsupportedPlatform(t *testing.T) {
	for _, tc := range []struct{ s, m, msg string }{
		{"FreeBSD", "amd64", "unsupported operating system: FreeBSD"},
		{"Linux", "riscv64", "unsupported architecture: riscv64"},
	} {
		t.Run(tc.s+"-"+tc.m, func(t *testing.T) {
			srv := serve(t, fakeRelease(t, "1.2.3", allTriples...))
			dir := installDir(t)
			r := runInstall(t, map[string]string{
				"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir,
				"FAKE_UNAME_S": tc.s, "FAKE_UNAME_M": tc.m,
			}, tools("curl"))
			wantFail(t, r, tc.msg)
			wantUntouched(t, dir, nil)
			if got := srv.requests(); len(got) > 0 {
				t.Fatalf("requests on an unsupported platform: %v", got)
			}
		})
	}
}

// 10. No fetcher.
func TestInstallNoFetcher(t *testing.T) {
	srv := serve(t, fakeRelease(t, "1.2.3", allTriples...))
	dir := installDir(t)
	r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools())
	wantFail(t, r, "curl or wget is required")
	wantUntouched(t, dir, nil)
	if got := srv.requests(); len(got) > 0 {
		t.Fatalf("requests without a fetcher: %v", got)
	}
}

// 11. Invalid version, pinned or served as latest: refused before anything is downloaded with
// it, and never echoed (it may come from the server).
func TestInstallInvalidVersion(t *testing.T) {
	for _, v := range []string{"1.0;x", "../1.2.3", "1.2.3/../../x", "1.2.3?x=1", "1.2.3#x", "1.2.3:x", "1.2.3%2f", `1.2.3\x`, "1.2\x1b[31m", " \t"} {
		t.Run(fmt.Sprintf("pinned %q", v), func(t *testing.T) {
			srv := serve(t, fakeRelease(t, "1.2.3", allTriples...))
			dir := installDir(t)
			r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir, "SHEARBOLT_VERSION": v}, tools("curl"))
			wantFail(t, r, "invalid version from SHEARBOLT_VERSION")
			wantNotEchoed(t, r.stderr, v)
			wantUntouched(t, dir, nil)
			if got := srv.requests(); len(got) > 0 {
				t.Fatalf("requests for an invalid version: %v", got)
			}
		})
	}
	for _, latest := range []string{"<html>../../x</html>\n", " \n", "1.2.3/../../x?y=1#z\n", "1.2.3\x1b[2J\n"} {
		t.Run(fmt.Sprintf("latest %q", latest), func(t *testing.T) {
			rel := fakeRelease(t, "1.2.3", allTriples...)
			writeFile(t, filepath.Join(rel, "latest"), []byte(latest), 0o644)
			srv := serve(t, rel)
			dir := installDir(t)
			r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools("curl"))
			wantFail(t, r, "invalid version from "+srv.URL+"/latest")
			wantNotEchoed(t, r.stderr, latest)
			wantUntouched(t, dir, nil)
			if got := srv.requests(); !slices.Equal(got, []string{"/latest"}) {
				t.Fatalf("requests = %v", got)
			}
		})
	}
}

// wantNotEchoed checks that stderr neither repeats a rejected value nor carries control characters.
func wantNotEchoed(t *testing.T, stderr, value string) {
	t.Helper()
	if v := strings.TrimSpace(value); v != "" && strings.Contains(stderr, v) {
		t.Fatalf("stderr echoes the rejected value %q:\n%s", v, stderr)
	}
	if strings.ContainsFunc(stderr, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
		t.Fatalf("stderr carries a control character: %q", stderr)
	}
}

// A missing latest file (404) is an error, not a guess.
func TestInstallLatestNotFound(t *testing.T) {
	eachFetcher(t, func(t *testing.T, fetcher string) {
		rel := fakeRelease(t, "1.2.3", allTriples...)
		if err := os.Remove(filepath.Join(rel, "latest")); err != nil {
			t.Fatal(err)
		}
		srv := serve(t, rel)
		dir := installDir(t)
		r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools(fetcher))
		wantFail(t, r, "could not determine the latest version from "+srv.URL+"/latest")
		wantUntouched(t, dir, nil)
		if got := srv.requests(); !slices.Equal(got, []string{"/latest"}) {
			t.Fatalf("requests = %v", got)
		}
	})
}

// Without HOME, the default install dir is unknown: the installer says what to set before any
// download. SHEARBOLT_INSTALL_DIR alone is enough.
func TestInstallWithoutHome(t *testing.T) {
	srv := serve(t, fakeRelease(t, "1.2.3", linuxAmd64))
	r := run(t, invocation{env: map[string]string{"SHEARBOLT_BASE_URL": srv.URL}, tools: tools("curl"), noHome: true})
	wantFail(t, r, "set SHEARBOLT_INSTALL_DIR or HOME")
	if got := srv.requests(); len(got) > 0 {
		t.Fatalf("requests without an install dir: %v", got)
	}
	dir := installDir(t)
	wantOK(t, run(t, invocation{env: map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools: tools("curl"), noHome: true}))
	wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64)
}

// sha256sum and shasum (used where sha256sum is missing) both verify with SHA-256.
func TestInstallHashTools(t *testing.T) {
	eachHash(t, func(t *testing.T, hash string) {
		t.Run("install", func(t *testing.T) {
			srv := serve(t, fakeRelease(t, "1.2.3", linuxAmd64))
			dir := installDir(t)
			wantOK(t, runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, toolsWithHash(hash, "curl")))
			wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64)
		})
		t.Run("checksum mismatch", func(t *testing.T) {
			rel := fakeRelease(t, "1.2.3", linuxAmd64)
			setChecksums(t, rel, "1.2.3", fmt.Sprintf("%x  shearbolt-%s.tar.gz", sha256.Sum256([]byte("other bytes")), linuxAmd64))
			srv := serve(t, rel)
			dir, previous := previousInstall(t)
			r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, toolsWithHash(hash, "curl"))
			wantFail(t, r, "checksum verification failed")
			wantUntouched(t, dir, previous)
		})
	})
}

// TMPDIR paths with a backslash, a space or a colon still install. The archive is hashed and
// unpacked from stdin, so GNU sha256sum's escaped names and GNU tar's host:path archives
// cannot apply.
func TestInstallUnusualTMPDIR(t *testing.T) {
	for _, tc := range []struct {
		name, dir string
		absolute  bool
	}{
		{"backslash, space and colon", `t m\p:x`, true},
		{"relative with a colon", "host:tmp", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachHash(t, func(t *testing.T, hash string) {
				tmpdir := tc.dir
				if tc.absolute {
					tmpdir = filepath.Join(t.TempDir(), tc.dir)
				}
				srv := serve(t, fakeRelease(t, "1.2.3", linuxAmd64))
				dir := installDir(t)
				r := run(t, invocation{
					env:   map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir},
					tools: toolsWithHash(hash, "curl"), tmpdir: tmpdir,
				})
				wantOK(t, r)
				wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64)
				if len(r.mktemp) != 1 {
					t.Fatalf("mktemp calls = %q, want exactly one", r.mktemp)
				}
			})
		})
	}
}

// Only a checksum line naming exactly the asset counts; lines that merely contain its name do not.
func TestInstallChecksumLineMatchesExactly(t *testing.T) {
	rel := fakeRelease(t, "1.2.3", linuxAmd64)
	name := "shearbolt-" + linuxAmd64 + ".tar.gz"
	good := sumOf(t, filepath.Join(rel, "v1.2.3", name))
	wrong := fmt.Sprintf("%x", sha256.Sum256([]byte("decoy")))
	decoys := []string{wrong + "  old/" + name, wrong + "  " + name + ".sig", wrong + "  x" + name, wrong + " *old/" + name}
	srv := serve(t, rel)

	setChecksums(t, rel, "1.2.3", append(slices.Clone(decoys), good+"  "+name)...)
	dir := installDir(t)
	wantOK(t, runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools("curl")))
	wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64)

	setChecksums(t, rel, "1.2.3", decoys...)
	dir = installDir(t)
	r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools("curl"))
	wantFail(t, r, "no build of shearbolt 1.2.3 for x86_64-unknown-linux-gnu")
	wantUntouched(t, dir, nil)
}

// 12. Replace: an older shearbolt is replaced atomically, with no temporary file left.
func TestInstallReplace(t *testing.T) {
	eachFetcher(t, func(t *testing.T, fetcher string) {
		srv := serve(t, fakeRelease(t, "1.2.3", linuxAmd64))
		dir, _ := previousInstall(t)
		wantOK(t, runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools(fetcher)))
		wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64) // also: no .shearbolt.new.* left
	})
}

// A directory where the binary belongs is refused instead of receiving the binary inside it.
func TestInstallRefusesDirectoryAtTarget(t *testing.T) {
	srv := serve(t, fakeRelease(t, "1.2.3", linuxAmd64))
	dir := installDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "shearbolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools("curl"))
	wantFail(t, r, "could not write "+dir+"/shearbolt")
	if entries, err := os.ReadDir(filepath.Join(dir, "shearbolt")); err != nil || len(entries) > 0 {
		t.Fatalf("directory at the target got %v (%v)", names(entries), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("install dir holds %v", names(entries))
	}
}

// 13. PATH hint.
func TestInstallPathHint(t *testing.T) {
	for _, onPath := range []bool{false, true} {
		t.Run(fmt.Sprintf("onPath=%v", onPath), func(t *testing.T) {
			srv := serve(t, fakeRelease(t, "1.2.3", linuxAmd64))
			dir := installDir(t)
			inv := invocation{env: map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools: tools("curl")}
			if onPath {
				inv.path = []string{dir}
			}
			r := run(t, inv)
			wantOK(t, r)
			hint := "Add " + dir + " to your PATH."
			if got := strings.Contains(r.stdout, hint); got == onPath {
				t.Fatalf("PATH hint present = %v with the install dir on PATH = %v:\n%s", got, onPath, r.stdout)
			}
		})
	}
}

// Checksums written in binary mode ("<sha256> *<file>") are accepted.
func TestInstallBinaryModeChecksums(t *testing.T) {
	rel := fakeRelease(t, "1.2.3", linuxAmd64)
	name := "shearbolt-" + linuxAmd64 + ".tar.gz"
	setChecksums(t, rel, "1.2.3", sumOf(t, filepath.Join(rel, "v1.2.3", name))+" *"+name)
	srv := serve(t, rel)
	dir := installDir(t)
	wantOK(t, runInstall(t, map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir}, tools("curl")))
	wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64)
}

// 14. Truncation: install.sh cut at any line boundary inside main and piped to sh exits
// non-zero, downloads nothing and writes nothing. The whole script piped to sh installs.
func TestInstallTruncatedScriptRunsNothing(t *testing.T) {
	script, err := os.ReadFile(installScript(t))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(script), "\n")
	lines = slices.DeleteFunc(lines, func(l string) bool { return l == "" }) // the piece after the last newline
	start, end := slices.Index(lines, "main() {\n"), -1
	for i := start + 1; start >= 0 && i < len(lines); i++ {
		if lines[i] == "}\n" {
			end = i
			break
		}
	}
	if start < 0 || end < 0 || lines[len(lines)-1] != `main "$@"`+"\n" {
		t.Fatalf("install.sh must define main() { ... } and end with main \"$@\" (main at %d, end at %d)", start, end)
	}
	if half := len(lines) / 2; half <= start || half >= end {
		t.Fatalf("the first half of install.sh (%d lines) does not end inside main (lines %d-%d)", half, start+1, end+1)
	}

	piped := func(t *testing.T, script string) (result, *server, string) {
		srv := serve(t, fakeRelease(t, "1.2.3", allTriples...))
		dir := installDir(t)
		r := run(t, invocation{
			env:   map[string]string{"SHEARBOLT_BASE_URL": srv.URL, "SHEARBOLT_INSTALL_DIR": dir},
			tools: tools("curl"), stdin: []byte(script),
		})
		return r, srv, dir
	}
	r, _, dir := piped(t, string(script))
	wantOK(t, r)
	wantInstalled(t, filepath.Join(dir, "shearbolt"), "shearbolt 1.2.3 "+linuxAmd64)

	for cut := start + 1; cut <= end; cut++ { // keep lines[:cut]: main's opening line, never its closing brace
		r, srv, dir := piped(t, strings.Join(lines[:cut], ""))
		if r.code == 0 {
			t.Errorf("cut after line %d: exit 0, want failure", cut)
		}
		if got := srv.requests(); len(got) > 0 {
			t.Errorf("cut after line %d: requests %v", cut, got)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("cut after line %d: install dir exists", cut)
		}
		if entries, _ := os.ReadDir(r.home); len(entries) > 0 {
			t.Errorf("cut after line %d: HOME holds %v", cut, names(entries))
		}
	}
}
