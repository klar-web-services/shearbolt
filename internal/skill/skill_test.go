package skill

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/skills"
)

// onDisk is the skill's source folder in the repository.
const onDisk = "../../skills/shearbolt"

// embedded returns every embedded file by its path below the skill folder.
func embedded(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := fs.WalkDir(skills.Files, Name, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := fs.ReadFile(skills.Files, p)
		out[strings.TrimPrefix(p, Name+"/")] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEmbedHoldsEveryFileOnDisk(t *testing.T) {
	emb := embedded(t)
	n := 0
	err := filepath.WalkDir(onDisk, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		if !e.Type().IsRegular() {
			t.Errorf("%s is not a regular file", p)
			return nil
		}
		n++
		rel, _ := filepath.Rel(onDisk, p)
		want, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if got, ok := emb[filepath.ToSlash(rel)]; !ok {
			t.Errorf("%s is not embedded (dotfiles and _files need an explicit embed pattern)", rel)
		} else if !bytes.Equal(got, want) {
			t.Errorf("%s: embedded bytes differ from the file on disk", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 || n != len(emb) {
		t.Errorf("%d files on disk, %d embedded", n, len(emb))
	}
}

// frontmatter splits SKILL.md into its YAML frontmatter and its body.
func frontmatter(t *testing.T, src string) (map[string]any, string) {
	t.Helper()
	rest, ok := strings.CutPrefix(src, "---\n")
	if !ok {
		t.Fatal("SKILL.md does not start with a --- frontmatter line")
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("SKILL.md frontmatter is not closed by a --- line")
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		t.Fatalf("frontmatter is not YAML: %v", err)
	}
	return fm, body
}

func TestSkillFrontmatter(t *testing.T) {
	src := string(embedded(t)["SKILL.md"])
	fm, body := frontmatter(t, src)
	name, _ := fm["name"].(string)
	if name != Name || !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(name) || name != path.Base(onDisk) {
		t.Errorf("name = %q, want %q, the folder's name", name, Name)
	}
	desc, _ := fm["description"].(string)
	if strings.TrimSpace(desc) == "" || len(desc) > 1024 {
		t.Errorf("description has %d characters, want 1 to 1024", len(desc))
	}
	meta, _ := fm["metadata"].(map[string]any)
	if meta["source"] != "shearbolt" {
		t.Errorf("metadata.source = %v, want shearbolt", meta["source"])
	}
	if !fromShearbolt([]byte(src)) {
		t.Error("Install would not recognize the shipped SKILL.md as its own")
	}
	if n := len(strings.Split(body, "\n")); n >= 500 {
		t.Errorf("body has %d lines, want fewer than 500", n)
	}
}

// The skill brings the CLI (and itself) up to date before it runs any other shearbolt
// command, so an old install never meets newer instructions or misses newer checks.
func TestSkillUpdatesTheCLIBeforeUsingIt(t *testing.T) {
	fm, body := frontmatter(t, string(embedded(t)["SKILL.md"]))
	const latest = "curl -fsSL https://tools.klar.ws/shearbolt/latest"
	allowed, _ := fm["allowed-tools"].(string)
	if !strings.Contains(allowed, "Bash("+latest+")") {
		t.Errorf("allowed-tools = %q, want the read-only version check %q", allowed, latest)
	}
	steps := []string{
		"command -v shearbolt && shearbolt version",
		latest,
		`SHEARBOLT_INSTALL_DIR="$(dirname "$(command -v shearbolt)")" sh`,
		"shearbolt skill install",
		"shearbolt setup --plan --json",
		"shearbolt scan --json",
	}
	at := -1
	for _, s := range steps {
		i := strings.Index(body, s)
		if i < 0 {
			t.Fatalf("SKILL.md does not contain %q", s)
		}
		if i < at {
			t.Errorf("SKILL.md mentions %q before the step that comes ahead of it", s)
		}
		at = i
	}
}

func TestSkillReferenceLinksExist(t *testing.T) {
	emb := embedded(t)
	links := regexp.MustCompile(`references/[A-Za-z0-9._/-]+`).FindAllString(string(emb["SKILL.md"]), -1)
	if len(links) == 0 {
		t.Fatal("SKILL.md links no reference file")
	}
	for _, l := range links {
		if _, ok := emb[l]; !ok {
			t.Errorf("SKILL.md links %s, which does not exist", l)
		}
	}
}

func TestDir(t *testing.T) {
	if got, want := Dir("/base"), filepath.Join("/base", ".claude", "skills", "shearbolt"); got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

// installed returns the files below dir, by slash path.
func installed(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInstallUpdateUnchanged(t *testing.T) {
	dir := Dir(t.TempDir())
	emb := embedded(t)

	out, err := Install(dir)
	if err != nil || out != Installed {
		t.Fatalf("first install: %q, %v", out, err)
	}
	got := installed(t, dir)
	if len(got) != len(emb) {
		t.Errorf("installed %d files, want %d", len(got), len(emb))
	}
	for rel, want := range emb {
		if !bytes.Equal(got[rel], want) {
			t.Errorf("%s: installed content differs", rel)
		}
	}
	checkModes(t, dir)

	if out, err := Install(dir); err != nil || out != Unchanged {
		t.Fatalf("second install: %q, %v", out, err)
	}

	// A changed reference file is restored; a file the skill does not ship is left alone.
	ref := filepath.Join(dir, "references", "fixing.md")
	if err := os.WriteFile(ref, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(extra, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := Install(dir); err != nil || out != Updated {
		t.Fatalf("install over an edited file: %q, %v", out, err)
	}
	if b, _ := os.ReadFile(ref); !bytes.Equal(b, emb["references/fixing.md"]) {
		t.Errorf("fixing.md was not restored")
	}
	if b, err := os.ReadFile(extra); err != nil || string(b) != "mine\n" {
		t.Errorf("notes.md was touched: %q, %v", b, err)
	}
}

func TestInstallRecreatesMissingSkillFile(t *testing.T) {
	dir := Dir(t.TempDir())
	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if out, err := Install(dir); err != nil || out != Installed {
		t.Fatalf("install without SKILL.md: %q, %v", out, err)
	}
}

func TestInstallRefusesForeignSkill(t *testing.T) {
	foreign := []byte("---\nname: shearbolt\ndescription: someone else's skill\n---\n\nNot ours.\n")
	cases := map[string]func(t *testing.T, dir string){
		"foreign SKILL.md": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), foreign, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		// Install never writes a symlink, so one is not its own, even if it points at its skill.
		"symlinked SKILL.md": func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(target, embedded(t)["SKILL.md"], 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "SKILL.md")); err != nil {
				t.Skip(err)
			}
		},
		"SKILL.md is a directory": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "SKILL.md"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			dir := Dir(t.TempDir())
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			prepare(t, dir)
			before := installed(t, dir)
			_, err := Install(dir)
			if !errors.Is(err, ErrForeign) {
				t.Fatalf("err = %v, want ErrForeign", err)
			}
			after := installed(t, dir)
			if len(after) != len(before) {
				t.Errorf("files changed: before %d, after %d", len(before), len(after))
			}
			for rel, b := range before {
				if !bytes.Equal(after[rel], b) {
					t.Errorf("%s changed", rel)
				}
			}
			if _, err := os.Lstat(filepath.Join(dir, "references")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("references/ was written next to a foreign skill (%v)", err)
			}
		})
	}
}

func TestInstallReportsWriteErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	base := t.TempDir()
	if err := os.Chmod(base, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(base, 0o755) })
	if _, err := Install(Dir(base)); err == nil || errors.Is(err, ErrForeign) {
		t.Fatalf("err = %v, want a write error", err)
	}
}

// checkModes requires mode 0644 for every file below dir and 0755 for dir and its folders.
func checkModes(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o644)
		if e.IsDir() {
			want = 0o755
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInstallRefusesLookalikeSkills(t *testing.T) {
	for name, content := range map[string]string{
		"source only in the body":     "---\nname: shearbolt\ndescription: x\n---\n\nUnlike metadata.source: shearbolt, this is mine.\nsource: shearbolt\n",
		"another source":              "---\nname: shearbolt\nmetadata:\n  source: shearbolt-community\n---\nbody\n",
		"source outside metadata":     "---\nname: shearbolt\nsource: shearbolt\n---\nbody\n",
		"no frontmatter":              "metadata:\n  source: shearbolt\n",
		"unclosed frontmatter":        "---\nname: shearbolt\nmetadata:\n  source: shearbolt\n",
		"frontmatter that is no YAML": "---\nmetadata: [source: shearbolt\n---\nbody\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := Dir(t.TempDir())
			skillMD := filepath.Join(dir, "SKILL.md")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(skillMD, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Install(dir); !errors.Is(err, ErrForeign) {
				t.Fatalf("err = %v, want ErrForeign", err)
			}
			if b, _ := os.ReadFile(skillMD); string(b) != content {
				t.Errorf("SKILL.md changed")
			}
		})
	}
}

func TestInstallUpdatesItsOwnSkillWithOtherContent(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// An older shearbolt SKILL.md, saved by an editor with CRLF line endings.
	old := "---\r\nname: shearbolt\r\ndescription: older\r\nmetadata:\r\n  source: shearbolt\r\n---\r\nold body\r\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := Install(dir); err != nil || out != Updated {
		t.Fatalf("install over an older SKILL.md: %q, %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); !bytes.Equal(b, embedded(t)["SKILL.md"]) {
		t.Error("SKILL.md was not replaced")
	}
}

func TestInstallWritesSkillFileLast(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// references is a file, so writing the reference files fails.
	if err := os.WriteFile(filepath.Join(dir, "references"), []byte("in the way\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir); err == nil {
		t.Fatal("install succeeded although references is a file")
	}
	if _, err := os.Lstat(filepath.Join(dir, "SKILL.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SKILL.md was written although its references were not (%v)", err)
	}
}

// outsideSetup returns a project root and a directory outside it that holds a reference file.
func outsideSetup(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root, outside = filepath.Join(base, "project"), filepath.Join(base, "outside")
	for _, d := range []string{root, filepath.Join(outside, "references")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "references", "fixing.md"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
}

func TestInstallInsideRefusesSymlinksOutside(t *testing.T) {
	cases := map[string]func(root, outside string){
		"skill folder":     func(root, outside string) { symlink(t, outside, Dir(root)) },
		"relative symlink": func(root, outside string) { symlink(t, "../../../outside", Dir(root)) },
		".claude":          func(root, outside string) { symlink(t, filepath.Dir(outside), filepath.Join(root, ".claude")) },
		"references folder": func(root, outside string) {
			symlink(t, filepath.Join(outside, "references"), filepath.Join(Dir(root), "references"))
		},
		"reference file": func(root, outside string) {
			symlink(t, filepath.Join(outside, "references", "fixing.md"), filepath.Join(Dir(root), "references", "fixing.md"))
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			root, outside := outsideSetup(t)
			prepare(root, outside)
			_, err := InstallInside(Dir(root), root)
			if !errors.Is(err, ErrOutside) {
				t.Fatalf("err = %v, want ErrOutside", err)
			}
			if b, _ := os.ReadFile(filepath.Join(outside, "references", "fixing.md")); string(b) != "keep\n" {
				t.Errorf("the file outside the project changed: %q", b)
			}
			if _, err := os.Lstat(filepath.Join(outside, "SKILL.md")); err == nil {
				t.Error("SKILL.md was written outside the project")
			}
		})
	}
}

func TestInstallInsideFollowsSymlinksWithin(t *testing.T) {
	root, _ := outsideSetup(t)
	shared := filepath.Join(root, "tools", "claude")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, "tools/claude", filepath.Join(root, ".claude"))
	// The root itself may be reached through a symlink.
	alias := filepath.Join(t.TempDir(), "alias")
	symlink(t, root, alias)
	if out, err := InstallInside(Dir(alias), alias); err != nil || out != Installed {
		t.Fatalf("install: %q, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(shared, "skills", "shearbolt", "SKILL.md")); err != nil {
		t.Error(err)
	}
}

func TestInstallFollowsSymlinksAnywhere(t *testing.T) {
	// A global install keeps dotfiles layouts working: ~/.claude -> ~/dotfiles/claude.
	home, outside := outsideSetup(t)
	symlink(t, outside, filepath.Join(home, ".claude"))
	if out, err := Install(Dir(home)); err != nil || out != Installed {
		t.Fatalf("install: %q, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "skills", "shearbolt", "SKILL.md")); err != nil {
		t.Error(err)
	}
}
