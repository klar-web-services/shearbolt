package scan

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/config"
)

func touch(t *testing.T, root string, rel string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindLockfilesIgnoresExcludedDirectories(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "node_modules/x/package-lock.json")
	touch(t, root, "examples/demo/yarn.lock")
	touch(t, root, "README.md")
	if found, err := FindLockfiles(root, []string{"examples/"}); err != nil || len(found) != 0 {
		t.Fatalf("found = %v, err = %v; want none", found, err)
	}
	touch(t, root, "web/pnpm-lock.yaml")
	if found, err := FindLockfiles(root, []string{"examples/"}); err != nil || !slices.Equal(found, []string{"web/pnpm-lock.yaml"}) {
		t.Fatalf("found = %v, err = %v; want [web/pnpm-lock.yaml]", found, err)
	}
}

func TestFindLockfilesReturnsEveryLockfileInAMonorepo(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "package-lock.json")
	touch(t, root, "web/pnpm-lock.yaml")
	touch(t, root, "README.md")
	found, err := FindLockfiles(root, nil)
	if err != nil || !slices.Equal(found, []string{"package-lock.json", "web/pnpm-lock.yaml"}) {
		t.Fatalf("found = %v, err = %v", found, err)
	}
}

func TestGrypeExcludes(t *testing.T) {
	got := GrypeExcludes([]string{"testdata/", "./vendor"})
	want := []string{"**/node_modules/**", "**/.git/**", "**/target/**", "**/.shearbolt/**", "./testdata/**", "./vendor/**"}
	if !slices.Equal(got, want) {
		t.Fatalf("GrypeExcludes = %q", got)
	}
}

func TestCovers(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	src := config.Target{Name: "repo", Source: root, Exclude: []string{"examples", "web"}}
	cases := map[string]bool{
		touch(t, root, "Cargo.lock"):              true,
		touch(t, root, "crates/x/Cargo.lock"):     true,
		touch(t, root, "webapp/Cargo.lock"):       true,
		touch(t, root, "examples/Cargo.lock"):     false,
		touch(t, root, "web/Cargo.lock"):          false,
		touch(t, root, "target/Cargo.lock"):       false,
		touch(t, outside, "Cargo.lock"):           false,
		touch(t, root, "Cargo.toml"):              false,
		filepath.Join(root, "missing/Cargo.lock"): false,
	}
	for path, want := range cases {
		if got := Covers(src, path); got != want {
			t.Errorf("Covers(%s) = %v, want %v", path, got, want)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "dir", "Cargo.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if Covers(src, filepath.Join(root, "dir", "Cargo.lock")) {
		t.Error("a directory named Cargo.lock is not a lockfile")
	}
}

func TestFindLockfilesFollowsSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	touch(t, real, "Cargo.lock")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if found, err := FindLockfiles(link, nil); err != nil || len(found) == 0 {
		t.Fatalf("found = %v, err = %v; want a lockfile", found, err)
	}
}

func TestFindLockfilesSkipsUnreadableDirectories(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	root := t.TempDir()
	touch(t, root, "a/secret/x")
	locked := filepath.Join(root, "a", "secret")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	touch(t, root, "package-lock.json")
	if found, err := FindLockfiles(root, nil); err != nil || len(found) == 0 {
		t.Fatalf("found = %v, err = %v; want a lockfile", found, err)
	}
}

func TestDevOnlyNodeProjectDirectoryIsNotAPackageJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "package.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if devOnlyNodeProject(dir) {
		t.Error("a package.json directory must not count as dev-only")
	}
}

func TestDevOnlyNodeProject(t *testing.T) {
	cases := []struct {
		name string
		pkg  string // empty means no package.json
		want bool
	}{
		{"only devDependencies", `{"devDependencies":{"a":"1.0.0"}}`, true},
		{"no dependency fields", `{"name":"x"}`, true},
		{"empty dependencies", `{"dependencies":{},"optionalDependencies":{}}`, true},
		{"dependencies", `{"dependencies":{"x":"1.0.0"}}`, false},
		{"optionalDependencies", `{"optionalDependencies":{"x":"1.0.0"}}`, false},
		{"workspaces array", `{"workspaces":["packages/*"]}`, false},
		{"workspaces object", `{"workspaces":{"packages":["a"]}}`, false},
		{"empty workspaces", `{"workspaces":[]}`, false},
		{"not an object dependencies", `{"dependencies":"x"}`, false},
		{"invalid JSON", `{ broken`, false},
		{"JSON null", `null`, false},
		{"missing package.json", "", false},
		{"workspaces null", `{"workspaces":null}`, false},
		{"not an object optionalDependencies", `{"optionalDependencies":["x"]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.pkg != "" {
				if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(c.pkg), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := devOnlyNodeProject(dir); got != c.want {
				t.Errorf("devOnlyNodeProject = %v, want %v", got, c.want)
			}
		})
	}
}

func TestYarnWorkspaceLockfile(t *testing.T) {
	cases := map[string]struct {
		name, pkg string
		want      bool
	}{
		"array":          {"yarn.lock", `{"workspaces":["packages/*"]}`, true},
		"object":         {"yarn.lock", `{"workspaces":{"packages":["a"]}}`, true},
		"empty array":    {"yarn.lock", `{"workspaces":[]}`, true},
		"null value":     {"yarn.lock", `{"workspaces":null}`, true},
		"no workspaces":  {"yarn.lock", `{"dependencies":{"a":"1.0.0"}}`, false},
		"unparseable":    {"yarn.lock", `{ nope`, false},
		"not an object":  {"yarn.lock", `null`, false},
		"missing":        {"yarn.lock", "", false},
		"bun lockfile":   {"bun.lock", `{"workspaces":["a"]}`, false},
		"other lockfile": {"package-lock.json", `{"workspaces":["a"]}`, false},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			if c.pkg != "" {
				if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(c.pkg), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := yarnWorkspaceLockfile(filepath.Join(dir, c.name)); got != c.want {
				t.Errorf("yarnWorkspaceLockfile = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLocalTag(t *testing.T) {
	for ref, want := range map[string]bool{
		"web:local":                   true,
		"floklar.com:local":           true,
		"registry:5000/web:local":     true,
		"web":                         false,
		"web:latest":                  false,
		"web:localhost":               false,
		"localhost:5000/web":          false,
		"local:5000/web":              false,
		"web:local@sha256:0123456789": false,
	} {
		if got := LocalTag(ref); got != want {
			t.Errorf("LocalTag(%q) = %v, want %v", ref, got, want)
		}
	}
}
