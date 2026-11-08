package rust

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/scan"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// demo copies testdata/rust/demo into a temp dir.
func demo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join("..", "..", "..", "..", "testdata", "rust", "demo")
	files := map[string]string{}
	for _, f := range []string{"Cargo.toml", "Cargo.lock", "src/main.rs"} {
		b, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		files[f] = string(b)
	}
	write(t, root, files)
	return root
}

// virtualWorkspace is a virtual workspace root with members crates/*.
func virtualWorkspace(t *testing.T, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"Cargo.toml":              "[workspace]\nresolver = \"2\"\nmembers = [\n  \"crates/*\", # all\n]\n",
		"Cargo.lock":              "version = 3\n",
		"crates/cli/Cargo.toml":   "[package]\nname = \"cli\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"crates/cli/src/main.rs":  "fn main() {}\n",
		"crates/core/Cargo.toml":  "[package]\nname = \"core\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"crates/core/src/lib.rs":  "",
		"crates/README.md":        "not a crate",
		"crates/notes/readme.txt": "",
	}
	for k, v := range extra {
		files[k] = v
	}
	write(t, root, files)
	return root
}

func find(t *testing.T, root string) ([]plan.Project, []plan.Note) {
	t.Helper()
	projects, notes, err := Plugin{}.Find(plan.Repo{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return projects, notes
}

func TestFindDemo(t *testing.T) {
	projects, notes := find(t, demo(t))
	want := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: "demo", Lockfiles: []string{"Cargo.lock"}, Binaries: []string{"demo"}}}
	if !reflect.DeepEqual(projects, want) || len(notes) != 0 {
		t.Fatalf("projects = %+v, notes = %+v", projects, notes)
	}
}

func TestFindBinaries(t *testing.T) {
	root := demo(t)
	toml, _ := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	write(t, root, map[string]string{
		"Cargo.toml":            string(toml) + "\n[[bin]]\nname = \"tool\"\npath = \"src/tool.rs\"\n\n[[bin]]\nname = \"gui\"\nrequired-features = [\"gui\"]\n",
		"src/bin/worker.rs":     "fn main() {}\n",
		"src/bin/multi/main.rs": "fn main() {}\n",
		"src/bin/notes.txt":     "",
	})
	projects, _ := find(t, root)
	if len(projects) != 1 || !slices.Equal(projects[0].Binaries, []string{"demo", "multi", "tool", "worker"}) {
		t.Fatalf("projects = %+v", projects)
	}

	// autobins = false turns off all inference, src/main.rs included, as in Cargo.
	write(t, root, map[string]string{
		"Cargo.toml": strings.Replace(string(toml), "[package]\n", "[package]\nautobins = false\n", 1) + "\n[[bin]]\nname = \"tool\"\npath = \"src/tool.rs\"\n",
	})
	if projects, _ := find(t, root); !slices.Equal(projects[0].Binaries, []string{"tool"}) {
		t.Fatalf("autobins = false: %q", projects[0].Binaries)
	}

	// A [[bin]] that claims src/main.rs replaces the package-named binary.
	write(t, root, map[string]string{"Cargo.toml": string(toml) + "\n[[bin]]\nname = \"renamed\"\npath = \"src/main.rs\"\n"})
	if projects, _ := find(t, root); !slices.Equal(projects[0].Binaries, []string{"multi", "renamed", "worker"}) {
		t.Fatalf("[[bin]] at src/main.rs: %q", projects[0].Binaries)
	}

	// Edition 2015 (the default) infers nothing once a [[bin]] is declared, unless autobins is set.
	write(t, root, map[string]string{"Cargo.toml": "[package]\nname = \"old\"\n\n[[bin]]\nname = \"tool\"\npath = \"src/tool.rs\"\n"})
	if projects, _ := find(t, root); !slices.Equal(projects[0].Binaries, []string{"tool"}) {
		t.Fatalf("edition 2015: %q", projects[0].Binaries)
	}
}

func TestFindWorkspace(t *testing.T) {
	root := virtualWorkspace(t, nil)
	projects, notes := find(t, root)
	want := []plan.Project{{Ecosystem: "rust", Dir: ".", Name: filepath.Base(root), Lockfiles: []string{"Cargo.lock"}, Binaries: []string{"cli"}}}
	if !reflect.DeepEqual(projects, want) || len(notes) != 0 {
		t.Fatalf("projects = %+v, notes = %+v", projects, notes)
	}
}

func TestFindNoCargoLock(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"tools/x/Cargo.toml":  "[package]\nname = \"x\"\n",
		"tools/x/src/main.rs": "",
		"other/Cargo.toml":    "[dependencies]\n",
	})
	projects, notes := find(t, root)
	if len(projects) != 0 || len(notes) != 1 || notes[0].Code != "NO_CARGO_LOCK" || notes[0].File != "tools/x/Cargo.toml" ||
		!strings.Contains(notes[0].Message, "cargo generate-lockfile") {
		t.Fatalf("projects = %+v, notes = %+v", projects, notes)
	}

	// A member below a workspace root without a lockfile gets no note of its own.
	write(t, root, map[string]string{"tools/Cargo.toml": "[workspace]\nmembers = [\"x\"]\n"})
	if _, notes := find(t, root); len(notes) != 1 || notes[0].File != "tools/Cargo.toml" {
		t.Fatalf("notes = %+v", notes)
	}

	// A Cargo.toml with its own Cargo.lock is a project even inside a workspace.
	root = virtualWorkspace(t, map[string]string{"crates/cli/Cargo.lock": ""})
	if projects, _ := find(t, root); len(projects) != 2 || projects[1].Dir != "crates/cli" || projects[1].Name != "cli" {
		t.Fatalf("projects = %+v", projects)
	}
}

// recognize parses script from dir, and recognizes its first command that mentions build (or
// its last command).
func recognize(t *testing.T, root, script, dir string, env ...string) *plan.Build {
	t.Helper()
	cmds, _ := shell.Parse(script, shell.State{Dir: dir})
	if len(cmds) == 0 {
		t.Fatalf("%q has no command", script)
	}
	c := cmds[len(cmds)-1]
	if i := slices.IndexFunc(cmds, func(c shell.Command) bool {
		return slices.Contains(c.Argv(), "build") || slices.Contains(c.Argv(), "b")
	}); i >= 0 {
		c = cmds[i]
	}
	e := map[string]bool{}
	for _, n := range env {
		e[n] = true
	}
	return Plugin{}.Recognize(plan.Repo{Root: root}, c, plan.Context{Dir: c.Before.Dir, Env: e})
}

func hasNote(b *plan.Build, code string) bool {
	return slices.ContainsFunc(b.Notes, func(n plan.Note) bool { return n.Code == code })
}

func TestRecognizePaths(t *testing.T) {
	root := demo(t)
	for script, want := range map[string][]string{
		"cargo build --release --locked":                                    {"target/release/demo"},
		"cargo build -r --bin demo":                                         {"target/release/demo"},
		"cargo +nightly build --release --target x86_64-unknown-linux-musl": {"target/x86_64-unknown-linux-musl/release/demo"},
		"cargo build --profile dist":                                        {"target/dist/demo"},
		"cargo build --profile=bench":                                       {"target/release/demo"},
		"$(CARGO) build --release":                                          {"target/release/demo"},
		"${CARGO} build --release":                                          {"target/release/demo"},
		`"$CARGO" build --release`:                                          {"target/release/demo"},
		"cargo auditable build --release":                                   {"target/release/demo"},
		"cargo b -r":                                                        {"target/release/demo"},
		"cargo --locked build --release":                                    {"target/release/demo"},
		"cargo build --release --target-dir out":                            {"out/release/demo"},
		"cargo build --release --target a-b-c --target x-y-z":               {"target/a-b-c/release/demo", "target/x-y-z/release/demo"},
		"cargo build --release --bin other":                                 {"target/release/other"},
		"cargo build --release --all-targets":                               {"target/release/demo"},
		"FOO=1 sudo cargo build --release":                                  {"target/release/demo"},
		"cargo build --release --manifest-path Cargo.toml":                  {"target/release/demo"},
	} {
		b := recognize(t, root, script, ".")
		if b == nil || b.Kind != plan.BuildBinary || !slices.Equal(b.Paths, want) || b.Dir != "." || len(b.Notes) != 0 {
			t.Errorf("%q: build = %+v, want paths %q", script, b, want)
		}
	}
}

func TestRecognizeUnknownPaths(t *testing.T) {
	root := demo(t)
	check := func(name string, b *plan.Build) {
		t.Helper()
		if b == nil || b.Kind != plan.BuildBinary || b.Paths != nil || !hasNote(b, "RUST_PATH_UNKNOWN") {
			t.Errorf("%s: build = %+v", name, b)
		}
	}
	for _, s := range []string{
		"cargo build --release --target ${{ matrix.target }}",
		"cargo build --release --target $TARGET",
		"cargo build --release --target-dir $DIR",
		"cargo build --release --target-dir /tmp/t",
		"cargo build --profile $P",
		"cargo build --release --target custom.json",
		"cargo build --release --config build.target-dir=\"x\"",
		"CARGO_BUILD_TARGET=x cargo build --release",
		"export CARGO_TARGET_DIR=/tmp/t\ncargo build --release",
		"cd $X && cargo build --release",
		"cargo build --release -p nope",
	} {
		check(s, recognize(t, root, s, "."))
	}
	for _, env := range []string{"CARGO_TARGET_DIR", "CARGO_BUILD_TARGET_DIR", "CARGO_BUILD_TARGET"} {
		check(env, recognize(t, root, "cargo build --release", ".", env))
	}
	if b := recognize(t, root, "cd $X && cargo build --release", "."); b == nil || b.Dir != "" {
		t.Errorf("unknown directory: build = %+v", b)
	}

	for _, cfg := range []string{".cargo/config.toml", ".cargo/config"} {
		r := demo(t)
		write(t, r, map[string]string{cfg: "[build]\ntarget = \"x86_64-unknown-linux-musl\"\n"})
		check(cfg, recognize(t, r, "cargo build --release", "."))
	}
	r := demo(t)
	write(t, r, map[string]string{"sub/x.txt": "", ".cargo/config.toml": "build.target-dir = \"out\"\n"})
	check("ancestor config", recognize(t, r, "cd sub && cargo build --release --manifest-path ../Cargo.toml", "."))
	r = demo(t)
	write(t, r, map[string]string{".cargo/config.toml": "[target.x86_64-unknown-linux-gnu]\nlinker = \"clang\"\n[alias]\nb = \"build\"\n"})
	if b := recognize(t, r, "cargo build --release", "."); b == nil || !slices.Equal(b.Paths, []string{"target/release/demo"}) {
		t.Errorf("unrelated config: build = %+v", b)
	}

	r = t.TempDir()
	write(t, r, map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n"})
	check("no Cargo.lock", recognize(t, r, "cargo build --release", "."))
}

func TestRecognizeNonBuilds(t *testing.T) {
	root := demo(t)
	for _, s := range []string{
		"cargo build", "cargo test --release", "cargo build --release --lib", "cargo build --release --examples",
		"cargo build --release --example x", "cargo build --release --tests --benches", "cargo build --profile dev",
		"cargo build --profile test", "cargo check --release", "cargo run --release", "echo cargo build --release",
		"cargo auditable build", "cargo publish",
	} {
		if b := recognize(t, root, s, "."); b != nil {
			t.Errorf("%q: recognized as %+v", s, b)
		}
	}
}

func TestRecognizeConditionalAndCross(t *testing.T) {
	root := demo(t)
	b := recognize(t, root, "if [ -n \"$RELEASE\" ]; then\n  cargo build --release\nfi", ".")
	if b == nil || b.Kind != plan.BuildOther || b.Paths != nil || !hasNote(b, "RUST_CONDITIONAL_BUILD") || hasNote(b, "RUST_PATH_UNKNOWN") {
		t.Fatalf("conditional: build = %+v", b)
	}
	prep, _ := Plugin{}.Prepare(plan.Repo{Root: root}, plan.Project{Dir: "."}, *b)
	if len(prep.Inserts) != 1 || prep.Install == nil {
		t.Fatalf("a conditional build is still rewritten: %+v", prep)
	}

	b = recognize(t, root, "cross build --release --target aarch64-unknown-linux-gnu", ".")
	if b == nil || b.Kind != plan.BuildOther || b.Paths != nil || len(b.Notes) != 0 || b.Dir != "." {
		t.Fatalf("cross: build = %+v", b)
	}
	prep, notes := Plugin{}.Prepare(plan.Repo{Root: root}, plan.Project{Dir: "."}, *b)
	if len(prep.Inserts) != 0 || prep.Install != nil || len(notes) != 1 || notes[0].Code != "RUST_CROSS" {
		t.Fatalf("cross: Prepare = %+v, %+v", prep, notes)
	}
}

func TestRecognizeWorkspace(t *testing.T) {
	root := virtualWorkspace(t, map[string]string{
		"crates/srv/Cargo.toml":   "[package]\nname = \"srv\"\nedition = \"2021\"\n",
		"crates/srv/src/main.rs":  "",
		"crates/srv/src/bin/x.rs": "",
	})
	for script, want := range map[string]struct {
		dir   string
		paths []string
	}{
		"cargo build --release": {".", []string{"target/release/cli", "target/release/srv", "target/release/x"}},
		"cargo build --release --manifest-path crates/cli/Cargo.toml": {"crates/cli", []string{"target/release/cli"}},
		"cd crates/cli && cargo build --release":                      {"crates/cli", []string{"target/release/cli"}},
		"cd crates/cli/src && cargo build --release":                  {"crates/cli/src", []string{"target/release/cli"}},
		"cd crates/cli && cargo build --release --target-dir out":     {"crates/cli", []string{"crates/cli/out/release/cli"}},
		"cargo build --release -p srv":                                {".", []string{"target/release/srv", "target/release/x"}},
		"cargo build --release -p srv --bin x":                        {".", []string{"target/release/x"}},
		"cargo build --release --package=cli -p srv@0.1.0":            {".", []string{"target/release/cli", "target/release/srv", "target/release/x"}},
		"cargo build --release --workspace --exclude srv":             {".", []string{"target/release/cli"}},
		"cargo build --release -p core":                               {".", []string{}},
	} {
		b := recognize(t, root, script, ".")
		if b == nil || b.Dir != want.dir || !slices.Equal(b.Paths, want.paths) || b.Paths == nil {
			t.Errorf("%q: build = %+v, want %+v", script, b, want)
		}
	}

	toml := "[workspace]\nmembers = [\"crates/*\"]\ndefault-members = [\"crates/srv\"]\n"
	write(t, root, map[string]string{"Cargo.toml": toml})
	if b := recognize(t, root, "cargo build --release", "."); b == nil || !slices.Equal(b.Paths, []string{"target/release/srv", "target/release/x"}) {
		t.Fatalf("default-members: build = %+v", b)
	}
	if projects, _ := find(t, root); len(projects) != 1 || !slices.Equal(projects[0].Binaries, []string{"cli", "srv", "x"}) {
		t.Fatalf("projects = %+v", projects)
	}

	// A workspace root that is also a package builds only itself by default.
	write(t, root, map[string]string{
		"Cargo.toml":  "[package]\nname = \"app\"\nedition = \"2021\"\n\n[workspace]\nmembers = [\"crates/cli\"]\n",
		"src/main.rs": "",
	})
	if b := recognize(t, root, "cargo build --release", "."); b == nil || !slices.Equal(b.Paths, []string{"target/release/app"}) {
		t.Fatalf("root package: build = %+v", b)
	}
	if b := recognize(t, root, "cargo build --release --workspace", "."); b == nil || !slices.Equal(b.Paths, []string{"target/release/app", "target/release/cli"}) {
		t.Fatalf("--workspace: build = %+v", b)
	}
	if b := recognize(t, root, "cd crates/srv && cargo build --release", "."); b == nil || b.Paths != nil || !hasNote(b, "RUST_PATH_UNKNOWN") {
		t.Fatalf("a package that is not a member: build = %+v", b)
	}
}

func apply(text string, ins []plan.Insert) string {
	slices.SortFunc(ins, func(a, b plan.Insert) int { return b.Offset - a.Offset })
	for _, in := range ins {
		text = text[:in.Offset] + in.Text + text[in.Offset:]
	}
	return text
}

func TestPrepare(t *testing.T) {
	root := demo(t)
	r := plan.Repo{Root: root}
	line := "\t$(CARGO) build --release --locked"
	b := recognize(t, root, line, ".")
	b.Via = "Makefile"
	b.Pos = plan.Pos{File: ".github/workflows/release.yml", Job: "j", Step: 2, Command: 0}
	prep, notes := Plugin{}.Prepare(r, plan.Project{Dir: "."}, *b)
	if len(notes) != 0 || len(prep.Inserts) != 1 {
		t.Fatalf("Prepare = %+v, %+v", prep, notes)
	}
	in := prep.Inserts[0]
	if in.Via != "Makefile" || in.Pos != b.Pos || in.Text != "auditable " {
		t.Fatalf("insert = %+v", in)
	}
	if got := apply(line, prep.Inserts); got != "\t$(CARGO) auditable build --release --locked" {
		t.Fatalf("applied: %q", got)
	}
	want := &plan.Install{Name: "Install cargo-auditable", Uses: "taiki-e/install-action@v2", With: map[string]string{"tool": "cargo-auditable"},
		Run: "cargo install cargo-auditable --locked", Marker: "cargo-auditable"}
	if !reflect.DeepEqual(prep.Install, want) {
		t.Fatalf("install = %+v", prep.Install)
	}

	b.RewriteOnly = true
	if prep, notes := (Plugin{}).Prepare(r, plan.Project{Dir: "."}, *b); len(prep.Inserts) != 1 || prep.Install != nil || len(notes) != 0 {
		t.Fatalf("RewriteOnly: %+v, %+v", prep, notes)
	}

	for _, s := range []string{"cargo +nightly build -r", "cargo b --release"} {
		b := recognize(t, root, s, ".")
		prep, _ := Plugin{}.Prepare(r, plan.Project{Dir: "."}, *b)
		if got := apply(s, prep.Inserts); !strings.Contains(got, "auditable b") {
			t.Errorf("%q: applied %q", s, got)
		}
	}

	// Already auditable: no insert, but the install is still asked for (the CI skips it when
	// an earlier step has cargo-auditable).
	b = recognize(t, root, "cargo auditable build --release", ".")
	if prep, _ := (Plugin{}).Prepare(r, plan.Project{Dir: "."}, *b); len(prep.Inserts) != 0 || prep.Install == nil {
		t.Fatalf("auditable: %+v", prep)
	}

	// A binary build with unknown paths gets RUST_PATH_UNKNOWN once.
	b = recognize(t, root, "cargo build --release --target $T", ".")
	if _, notes := (Plugin{}).Prepare(r, plan.Project{Dir: "."}, *b); len(notes) != 0 {
		t.Fatalf("note repeated: %+v", notes)
	}
	b.Notes = nil
	if _, notes := (Plugin{}).Prepare(r, plan.Project{Dir: "."}, *b); len(notes) != 1 || notes[0].Code != "RUST_PATH_UNKNOWN" {
		t.Fatalf("notes = %+v", notes)
	}

	// npm builds of a Rust project are left alone.
	cmds, _ := shell.Parse("npm run build", shell.State{Dir: "."})
	if prep, notes := (Plugin{}).Prepare(r, plan.Project{Dir: "."}, plan.Build{Kind: plan.BuildOther, Dir: ".", Cmd: cmds[0]}); !reflect.DeepEqual(prep, plan.Prep{}) || notes != nil {
		t.Fatalf("npm build: %+v, %+v", prep, notes)
	}
}

func TestPrepareDockerfile(t *testing.T) {
	root := demo(t)
	write(t, root, map[string]string{
		"Dockerfile":      "FROM rust:1 AS build\nRUN cargo build --release --locked\nFROM scratch\n",
		"docker/plain.Df": "FROM alpine\nCOPY target/release/demo /demo\n",
	})
	r := plan.Repo{Root: root}
	img := plan.Build{Kind: plan.BuildImage, Dir: ".", Image: &plan.Image{Tool: "docker", Dockerfile: "Dockerfile", Context: "."}}
	prep, notes := Plugin{}.Prepare(r, plan.Project{Dir: "."}, img)
	if len(prep.Inserts) != 0 || len(notes) != 1 || notes[0].Code != "RUST_DOCKERFILE" || !strings.Contains(notes[0].Message, "Dockerfile") {
		t.Fatalf("Prepare = %+v, %+v", prep, notes)
	}
	img.Image = &plan.Image{Tool: "docker", Dockerfile: "docker/plain.Df", Context: "."}
	if _, notes := (Plugin{}).Prepare(r, plan.Project{Dir: "."}, img); len(notes) != 0 {
		t.Fatalf("a Dockerfile without cargo build: %+v", notes)
	}
}

func TestReadTOML(t *testing.T) {
	m := parseManifest(strings.Join([]string{
		`# comment [workspace]`,
		`[ package ]`,
		`name = "x" # trailing`,
		`description = """`,
		`[workspace]`,
		`members = ["not"]`,
		`"""`,
		`"autobins" = false`,
		`metadata = { a = [1, "b"] }`,
		`[[bin]]`,
		`name = 'lit\eral'`,
		`required-features = []`,
		`[[bin]]`,
		`name = "esc\"aped"`,
		`[workspace]`,
		`members = [`,
		`  "a", # first`,
		`  # "commented",`,
		`  'b',`,
		`]`,
		`exclude = ["c"]`,
	}, "\n"))
	if !m.pkg || !m.workspace || m.name != "x" || m.autobins == nil || *m.autobins {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.bins) != 2 || m.bins[0].name != `lit\eral` || m.bins[0].requiredFeatures || m.bins[1].name != `esc"aped` {
		t.Fatalf("bins = %+v", m.bins)
	}
	if !slices.Equal(m.members, []string{"a", "b"}) || m.defaultMembers != nil {
		t.Fatalf("members = %q, default = %q", m.members, m.defaultMembers)
	}
}

func TestLockfilesAreScanned(t *testing.T) {
	if !slices.Contains(scan.Lockfiles, lockfile) || (Plugin{}).Name() != "rust" {
		t.Fatal("Cargo.lock is not in scan.Lockfiles")
	}
}
