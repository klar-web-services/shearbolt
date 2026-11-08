package plan

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// ErrOutside is returned for a path that resolves outside the repository.
var ErrOutside = errors.New("path is outside the repository")

// Repo is the repository setup works in. Read and Exists take repo-relative, slash-separated
// paths, resolve symlinks, and refuse paths that resolve outside Root.
type Repo struct{ Root string } // absolute

// skipDirs are the directories Walk never enters (R1), besides every name starting with ".".
var skipDirs = []string{"node_modules", "target", "vendor", "dist", "build", "out", "coverage", "testdata"}

// Read returns the content of the regular file at rel.
func (r Repo) Read(rel string) ([]byte, error) {
	p, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return os.ReadFile(p)
}

// ReadDir returns the sorted entry names of the directory at rel.
func (r Repo) ReadDir(rel string) ([]string, error) {
	p, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// Exists reports whether rel exists inside the repository.
func (r Repo) Exists(rel string) bool {
	_, err := r.resolve(rel)
	return err == nil
}

func (r Repo) resolve(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("%q: %w", rel, ErrOutside)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if outside(clean) {
		return "", fmt.Errorf("%s: %w", rel, ErrOutside)
	}
	root, err := filepath.EvalSymlinks(r.Root)
	if err != nil {
		return "", err
	}
	full, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(root, full)
	if err != nil || outside(within) || filepath.IsAbs(within) {
		return "", fmt.Errorf("%s: %w", rel, ErrOutside)
	}
	return full, nil
}

func outside(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Walk visits the repository's directories in lexical order, starting at "." (R1). fn gets each
// visited directory (repo-relative, slash-separated) and the sorted names of its entries.
// Directories in the R1 skip list, and every other directory whose name starts with ".", are not
// entered; skipped gets each of them (only the topmost, since their contents are not walked).
// Symlinked directories are not followed. A directory that cannot be read is an error, so no
// project is silently missed.
func (r Repo) Walk(fn func(dir string, names []string), skipped func(dir string)) error {
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := os.ReadDir(filepath.Join(r.Root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		fn(rel, names)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			child := path.Join(rel, e.Name())
			if strings.HasPrefix(e.Name(), ".") || slices.Contains(skipDirs, e.Name()) {
				if skipped != nil {
					skipped(child)
				}
				continue
			}
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(".")
}
