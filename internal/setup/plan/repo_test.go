package plan

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoReadIsConfined(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/b.txt", "hello")
	r := Repo{Root: root}
	if b, err := r.Read("a/b.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if !r.Exists("a/b.txt") || !r.Exists("a") || r.Exists("a/missing") {
		t.Fatal("Exists is wrong")
	}
	if err := os.Symlink("b.txt", filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if b, err := r.Read("a/link"); err != nil || string(b) != "hello" {
		t.Fatalf("symlink inside: %q, %v", b, err)
	}
	if _, err := r.Read("../x"); !errors.Is(err, ErrOutside) {
		t.Fatalf("../x: err = %v", err)
	}
	if _, err := r.Read("/etc/passwd"); !errors.Is(err, ErrOutside) {
		t.Fatalf("absolute: err = %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "passwd")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read("passwd"); !errors.Is(err, ErrOutside) {
		t.Fatalf("symlink outside: err = %v", err)
	}
	if r.Exists("passwd") || r.Exists("../x") {
		t.Fatal("Exists accepts a path outside the repository")
	}
	if _, err := r.Read("a/missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: err = %v", err)
	}
	if _, err := r.Read("a"); err == nil {
		t.Fatal("reading a directory: no error")
	}
}

func TestRepoWalkSkipsDirectories(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"package.json", "node_modules/x/package.json", ".git/config", "testdata/p/package-lock.json", ".foo/bar", "target/release/x", "apps/web/package.json", "apps/web/dist/x.js", ".github/workflows/ci.yml"} {
		write(t, root, f, "{}")
	}
	var visited, skipped []string
	names := map[string][]string{}
	err := Repo{Root: root}.Walk(func(dir string, n []string) {
		visited = append(visited, dir)
		names[dir] = n
	}, func(dir string) { skipped = append(skipped, dir) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".", "apps", "apps/web"}; !slices.Equal(visited, want) {
		t.Fatalf("visited = %q, want %q", visited, want)
	}
	if want := []string{".foo", ".git", ".github", "apps/web/dist", "node_modules", "target", "testdata"}; !slices.Equal(skipped, want) {
		t.Fatalf("skipped = %q, want %q", skipped, want)
	}
	if want := []string{".foo", ".git", ".github", "apps", "node_modules", "package.json", "target", "testdata"}; !slices.Equal(names["."], want) {
		t.Fatalf("names = %q", names["."])
	}
}

func TestRepoReadDirIsConfined(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/bin/b.rs", "")
	write(t, root, "src/bin/a/main.rs", "")
	r := Repo{Root: root}
	if names, err := r.ReadDir("src/bin"); err != nil || !slices.Equal(names, []string{"a", "b.rs"}) {
		t.Fatalf("ReadDir = %q, %v", names, err)
	}
	if err := os.Symlink(os.TempDir(), filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadDir("out"); !errors.Is(err, ErrOutside) {
		t.Fatalf("symlink outside: err = %v", err)
	}
	if _, err := r.ReadDir("../"); !errors.Is(err, ErrOutside) {
		t.Fatalf("../: err = %v", err)
	}
	if _, err := r.ReadDir("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: err = %v", err)
	}
}

func TestCleanDir(t *testing.T) {
	for in, want := range map[string]string{
		".": ".", "": "", "a/b": "a/b", "./a/": "a", "a/../b": "b", "a/..": ".",
		"/srv/app": "", "/": "", "..": "", "../x": "", "a/../../x": "",
	} {
		if got := CleanDir(in); got != want {
			t.Errorf("CleanDir(%q) = %q, want %q", in, got, want)
		}
	}
}
