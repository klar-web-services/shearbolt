package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
)

// Lockfiles are dependency files Grype reads. A source target must contain at least one.
// npm, pnpm, Yarn, Bun and Cargo have setup support; the others are scanned but not yet set up.
var Lockfiles = []string{
	"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock",
	"Cargo.lock",
	"go.mod", "poetry.lock", "Pipfile.lock", "uv.lock", "pdm.lock", "requirements.txt",
	"Gemfile.lock", "composer.lock", "pom.xml", "gradle.lockfile", "packages.lock.json",
	"mix.lock", "pubspec.lock", "Podfile.lock", "Package.resolved", "conan.lock",
}

// defaultExcludes are directory names never scanned as part of a source target.
var defaultExcludes = []string{"node_modules", ".git", "target", ".shearbolt"}

// DockerInspector reports whether an image exists in the local Docker daemon.
type DockerInspector func(ctx context.Context, ref string) bool

// DockerHasImage asks the docker CLI, when installed, whether the image exists locally.
func DockerHasImage(ctx context.Context, ref string) bool {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, docker, "image", "inspect", "--", ref).Run() == nil
}

// GrypeExcludes turns a source target's exclude list into Grype --exclude globs.
func GrypeExcludes(user []string) []string {
	out := make([]string, 0, len(defaultExcludes)+len(user))
	for _, d := range defaultExcludes {
		out = append(out, "**/"+d+"/**")
	}
	for _, u := range user {
		if p := cleanExclude(u); p != "" {
			out = append(out, "./"+p+"/**")
		}
	}
	return out
}

// FindLockfiles returns the slash-separated paths, relative to root, of every known lockfile
// outside excluded paths, in walk order.
func FindLockfiles(root string, exclude []string) ([]string, error) {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			// Grype skips unreadable paths too.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != "." && excluded(rel, exclude) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && slices.Contains(Lockfiles, d.Name()) {
			found = append(found, rel)
		}
		return nil
	})
	return found, err
}

// Covers reports whether a source target's tree, after excludes, contains the lockfile.
func Covers(source config.Target, lockfile string) bool {
	if !slices.Contains(Lockfiles, filepath.Base(lockfile)) {
		return false
	}
	if info, err := os.Stat(lockfile); err != nil || !info.Mode().IsRegular() {
		return false
	}
	rel, err := filepath.Rel(source.Source, lockfile)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return false
	}
	return !excluded(rel, source.Exclude)
}

// excluded reports whether rel (slash-separated, relative to the source root) is excluded.
func excluded(rel string, user []string) bool {
	for _, part := range strings.Split(rel, "/") {
		if slices.Contains(defaultExcludes, part) {
			return true
		}
	}
	for _, u := range user {
		if p := cleanExclude(u); p != "" && (rel == p || strings.HasPrefix(rel, p+"/")) {
			return true
		}
	}
	return false
}

func cleanExclude(p string) string {
	c := strings.Trim(filepath.ToSlash(filepath.Clean(p)), "/")
	if c == "." {
		return ""
	}
	return c
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// devOnlyNodeProject reports whether dir holds a package.json that ships nothing: it parses,
// has no non-empty dependencies or optionalDependencies, and has no workspaces field. Grype
// skips devDependencies for yarn and bun projects, so a lockfile next to such a file yields no
// packages. Anything unexpected returns false, which keeps the lockfile fail-closed.
func devOnlyNodeProject(dir string) bool {
	path := filepath.Join(dir, "package.json")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var pkg map[string]any
	if err := json.Unmarshal(data, &pkg); err != nil || pkg == nil {
		return false
	}
	if _, ok := pkg["workspaces"]; ok {
		return false
	}
	for _, key := range []string{"dependencies", "optionalDependencies"} {
		switch v := pkg[key].(type) {
		case nil:
		case map[string]any:
			if len(v) > 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// yarnWorkspaceLockfile reports whether lockfile is a yarn.lock whose sibling package.json
// parses and has a workspaces field, whatever its value. Syft drops every package reachable
// from such a root's devDependencies, even when a workspace member ships it. A missing or
// unparseable package.json returns false, so the ordinary lockfile rules apply.
func yarnWorkspaceLockfile(lockfile string) bool {
	if filepath.Base(lockfile) != "yarn.lock" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(lockfile), "package.json"))
	if err != nil {
		return false
	}
	var pkg map[string]any
	if err := json.Unmarshal(data, &pkg); err != nil || pkg == nil {
		return false
	}
	_, ok := pkg["workspaces"]
	return ok
}
