package skill

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/skills"
)

const Name = "shearbolt"

const source = "shearbolt"

type Outcome string

const (
	Installed Outcome = "installed"
	Updated   Outcome = "updated"
	Unchanged Outcome = "unchanged"
)

var ErrForeign = errors.New("a different skill named shearbolt is already installed there")

var ErrOutside = errors.New("the skill folder leads outside the project through a symlink")

func Dir(base string) string { return filepath.Join(base, ".claude", "skills", Name) }

// Install follows symlinks anywhere, so a global install works with dotfiles layouts
// (~/.claude -> ~/dotfiles/claude).
//
// Each file is replaced through a temporary file and a rename, and SKILL.md goes last, so a
// failed install never leaves a new SKILL.md pointing at missing references.
func Install(dir string) (Outcome, error) { return install(dir, nil) }

func InstallInside(dir, root string) (Outcome, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	return install(dir, func(target string) error { return inside(target, realRoot) })
}

func install(dir string, confine func(target string) error) (Outcome, error) {
	files, err := skillFiles()
	if err != nil {
		return "", err
	}
	if confine != nil {
		targets := []string{dir}
		for _, f := range files {
			for d := path.Dir(f.rel); d != "."; d = path.Dir(d) {
				targets = append(targets, filepath.Join(dir, filepath.FromSlash(d)))
			}
			targets = append(targets, filepath.Join(dir, filepath.FromSlash(f.rel)))
		}
		for _, t := range targets {
			if err := confine(t); err != nil {
				return "", err
			}
		}
	}
	existed, err := ours(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", err
	}
	wrote := false
	for _, f := range files {
		changed, err := put(dir, f.rel, f.data)
		if err != nil {
			return "", err
		}
		wrote = wrote || changed
	}
	switch {
	case !existed:
		return Installed, nil
	case wrote:
		return Updated, nil
	}
	return Unchanged, nil
}

func inside(target, realRoot string) error {
	a := target
	for {
		if _, err := os.Lstat(a); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(a)
		if parent == a {
			break
		}
		a = parent
	}
	resolved, err := filepath.EvalSymlinks(a)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(realRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %s resolves to %s, outside %s", ErrOutside, a, resolved, realRoot)
	}
	return nil
}

func ours(skill string) (bool, error) {
	info, err := os.Lstat(skill)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, ErrForeign
	}
	b, err := os.ReadFile(skill)
	if err != nil {
		return false, err
	}
	if !fromShearbolt(b) {
		return false, ErrForeign
	}
	return true, nil
}

// Line endings may be CRLF, since an editor may have saved the file.
func fromShearbolt(b []byte) bool {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return false
	}
	head, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return false
	}
	var fm struct {
		Metadata struct {
			Source string `yaml:"source"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		return false
	}
	return fm.Metadata.Source == source
}

type file struct {
	rel  string // slash path below the skill folder
	data []byte
}

func skillFiles() ([]file, error) {
	var out []file
	err := fs.WalkDir(skills.Files, Name, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := fs.ReadFile(skills.Files, p)
		out = append(out, file{rel: strings.TrimPrefix(p, Name+"/"), data: b})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the embedded skill: %w", err)
	}
	slices.SortStableFunc(out, func(a, b file) int {
		switch {
		case a.rel == "SKILL.md":
			return 1
		case b.rel == "SKILL.md":
			return -1
		}
		return 0
	})
	return out, nil
}

func put(dir, rel string, data []byte) (bool, error) {
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if info, err := os.Lstat(target); err == nil && info.Mode().IsRegular() {
		if old, err := os.ReadFile(target); err == nil && bytes.Equal(old, data) {
			return false, nil
		}
	}
	if err := mkdirs(dir, path.Dir(rel)); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".shearbolt-skill-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // fails harmlessly once renamed
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), target)
	}
	if err != nil {
		return false, fmt.Errorf("write %s: %w", target, err)
	}
	return true, nil
}

// Parents of dir get os.MkdirAll's 0755 under the umask; dir and sub, when created here, get
// exactly 0755.
func mkdirs(dir, sub string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	p := dir
	parts := []string{""}
	if sub != "." {
		parts = append(parts, strings.Split(sub, "/")...)
	}
	for _, part := range parts {
		p = filepath.Join(p, part)
		err := os.Mkdir(p, 0o755)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err == nil {
			err = os.Chmod(p, 0o755)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
