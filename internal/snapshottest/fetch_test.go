package snapshottest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const asset = "grype-db-v6.1.10-20261108T002204Z.tar.zst"

var good = []byte("pinned snapshot bytes")

// fakeGh logs its arguments and, for "release download", copies $FAKE_GH_CONTENT to <dir>/<pattern>.
// Like the real gh it may write to stdout, which the script must keep off its own stdout.
const fakeGh = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_GH_LOG"
echo "fake gh: $1 $2"
[ "$1 $2" = "release download" ] || exit 1
dir= pattern=
while [ $# -gt 0 ]; do
	case $1 in
	--dir) dir=$2; shift ;;
	--pattern) pattern=$2; shift ;;
	esac
	shift
done
[ -z "$FAKE_GH_CONTENT" ] || cp "$FAKE_GH_CONTENT" "$dir/$pattern"
`

type fixture struct {
	repo, log, content string
	env                []string // extra environment variables
}

// newFixture lays out a repository with the script and a pin for good's checksum. content is
// what the fake gh downloads (nil: gh downloads nothing).
func newFixture(t *testing.T, pin string, content []byte) fixture {
	t.Helper()
	f := fixture{repo: t.TempDir(), log: filepath.Join(t.TempDir(), "gh.log")}
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "db-snapshot.sh"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.repo, "scripts", "db-snapshot.sh"), script, 0o755)
	write(t, filepath.Join(f.repo, "testdata", "db-snapshot.sha256"), []byte(pin), 0o644)
	if content != nil {
		f.content = filepath.Join(t.TempDir(), "download")
		write(t, f.content, content, 0o644)
	}
	return f
}

func goodPin() string { return fmt.Sprintf("%x  %s\n", sha256.Sum256(good), asset) }

func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

type result struct {
	code           int
	stdout, stderr string
	gh             []string
}

// run executes the script with a fake gh first on PATH, from cwd.
func (f fixture) run(t *testing.T, cwd string, args ...string) result {
	t.Helper()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "gh"), []byte(fakeGh), 0o755)
	cmd := exec.Command(filepath.Join(f.repo, "scripts", "db-snapshot.sh"), args...)
	cmd.Dir = cwd
	cmd.Env = append([]string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "FAKE_GH_LOG=" + f.log, "FAKE_GH_CONTENT=" + f.content}, f.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r := result{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		r.code = exit.ExitCode()
	}
	r.stdout, r.stderr = stdout.String(), stderr.String()
	if log, err := os.ReadFile(f.log); err == nil {
		r.gh = strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
	}
	return r
}

func wantPath(t *testing.T, r result, path string) {
	t.Helper()
	if r.code != 0 || r.stdout != path+"\n" {
		t.Fatalf("exit %d, stdout %q, want 0 and %q\nstderr:\n%s", r.code, r.stdout, path+"\n", r.stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, good) {
		t.Fatalf("%s holds %q (%v), want the pinned bytes", path, data, err)
	}
}

func TestFetchUsesAVerifiedCopy(t *testing.T) {
	f := newFixture(t, goodPin(), good)
	dir := t.TempDir()
	write(t, filepath.Join(dir, asset), good, 0o644)
	r := f.run(t, t.TempDir(), "fetch", dir)
	wantPath(t, r, filepath.Join(dir, asset))
	if len(r.gh) > 0 {
		t.Fatalf("gh called for a verified copy: %q", r.gh)
	}
}

func TestFetchDownloadsIntoANewDir(t *testing.T) {
	f := newFixture(t, goodPin(), good)
	cwd := t.TempDir()
	r := f.run(t, cwd, "fetch", "cache/db") // relative, created, printed absolute
	dir, err := filepath.EvalSymlinks(filepath.Join(cwd, "cache", "db"))
	if err != nil {
		t.Fatal(err)
	}
	wantPath(t, r, filepath.Join(dir, asset))
	want := "release download test-db --pattern " + asset + " --dir " + dir + " --clobber"
	if len(r.gh) != 1 || r.gh[0] != want {
		t.Fatalf("gh calls = %q, want [%q]", r.gh, want)
	}
	if again := f.run(t, cwd, "fetch", "cache/db"); len(again.gh) != 1 { // the log is shared: no new call
		t.Fatalf("second fetch called gh again: %q", again.gh)
	}
}

// An exported CDPATH must not redirect or echo the cd that resolves DIR.
func TestFetchIgnoresCDPATH(t *testing.T) {
	f := newFixture(t, goodPin(), good)
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "cache", "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = []string{"CDPATH=" + elsewhere}
	cwd := t.TempDir()
	r := f.run(t, cwd, "fetch", "cache/db")
	dir, err := filepath.EvalSymlinks(filepath.Join(cwd, "cache", "db"))
	if err != nil {
		t.Fatal(err)
	}
	wantPath(t, r, filepath.Join(dir, asset))
}

func TestFetchReplacesACorruptCopy(t *testing.T) {
	f := newFixture(t, goodPin(), good)
	dir := t.TempDir()
	write(t, filepath.Join(dir, asset), []byte("truncated"), 0o644)
	r := f.run(t, t.TempDir(), "fetch", dir)
	wantPath(t, r, filepath.Join(dir, asset))
	if len(r.gh) != 1 {
		t.Fatalf("gh calls = %q, want one download", r.gh)
	}
}

func TestFetchRejectsABadDownload(t *testing.T) {
	for name, content := range map[string][]byte{"wrong bytes": []byte("tampered"), "nothing downloaded": nil} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, goodPin(), content)
			dir := t.TempDir()
			r := f.run(t, t.TempDir(), "fetch", dir)
			if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "snapshot "+asset+" failed checksum verification") {
				t.Fatalf("exit %d, stdout %q, stderr:\n%s", r.code, r.stdout, r.stderr)
			}
			if _, err := os.Lstat(filepath.Join(dir, asset)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverified download kept (%v)", err)
			}
		})
	}
}

func TestFetchRejectsAMalformedPin(t *testing.T) {
	sum := fmt.Sprintf("%x", sha256.Sum256(good))
	for name, pin := range map[string]string{
		"glob in the name":    sum + "  grype-db-*.tar.zst\n",
		"path in the name":    sum + "  ../x.tar.zst\n",
		"not a tar.zst":       sum + "  grype-db.tar.gz\n",
		"short hash":          sum[:63] + "  " + asset + "\n",
		"uppercase hash":      strings.ToUpper(sum) + "  " + asset + "\n",
		"extra field":         sum + "  " + asset + " extra\n",
		"empty":               "",
		"name only":           asset + "\n",
		"sha256: prefix kept": "sha256:" + sum + "  " + asset + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, pin, good)
			r := f.run(t, t.TempDir(), "fetch", t.TempDir())
			if r.code != 1 || r.stdout != "" || !(strings.Contains(r.stderr, "malformed pin") || strings.Contains(r.stderr, "cannot read")) {
				t.Fatalf("exit %d, stdout %q, stderr:\n%s", r.code, r.stdout, r.stderr)
			}
			if len(r.gh) > 0 {
				t.Fatalf("gh called with a malformed pin: %q", r.gh)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	f := newFixture(t, goodPin(), good)
	for _, args := range [][]string{nil, {"bogus"}, {"fetch"}, {"fetch", ""}, {"fetch", "a", "b"}, {"refresh"}} {
		r := f.run(t, t.TempDir(), args...)
		if r.code != 2 || !strings.Contains(r.stderr, "usage: scripts/db-snapshot.sh fetch DIR\n") {
			t.Errorf("args %q: exit %d, stderr:\n%s", args, r.code, r.stderr)
		}
		if len(r.gh) > 0 {
			t.Errorf("args %q: gh called: %q", args, r.gh)
		}
	}
}
