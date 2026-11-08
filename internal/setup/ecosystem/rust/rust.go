// Package rust is the setup plugin for Cargo projects. It finds them (R1), recognizes release
// cargo builds and the binaries they write (R2, R6), and prepares those builds to embed their
// crate list with cargo-auditable.
package rust

import (
	"bytes"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

const lockfile = "Cargo.lock"

// Plugin is the Rust ecosystem. It keeps no state: every call reads the manifests it needs, which
// is cheap, so one value can serve every run.
type Plugin struct{}

// Name returns "rust".
func (Plugin) Name() string { return "rust" }

// Find returns every directory with a Cargo.toml ([package] or [workspace]) and a Cargo.lock,
// sorted by Dir. A Cargo.toml without a lockfile below a workspace root is a member and is
// covered by that root; any other gets a NO_CARGO_LOCK note.
func (Plugin) Find(r plan.Repo) ([]plan.Project, []plan.Note, error) {
	type found struct {
		dir  string
		m    cargoManifest
		lock bool
	}
	var all []found
	err := r.Walk(func(dir string, names []string) {
		if !slices.Contains(names, "Cargo.toml") {
			return
		}
		m, ok := readManifest(r, dir)
		if !ok || !m.pkg && !m.workspace {
			return
		}
		all = append(all, found{dir: dir, m: m, lock: slices.Contains(names, lockfile)})
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	var projects []plan.Project
	var notes []plan.Note
	for _, f := range all {
		if f.lock {
			name := f.m.name
			if !f.m.pkg || name == "" {
				name = dirName(r, f.dir)
			}
			ws := loadWorkspace(r, f.dir)
			projects = append(projects, plan.Project{Ecosystem: "rust", Dir: f.dir, Name: name, Lockfiles: []string{lockfile}, Binaries: ws.binaries()})
			continue
		}
		if slices.ContainsFunc(all, func(g found) bool { return g.m.workspace && under(f.dir, g.dir) }) {
			continue
		}
		notes = append(notes, plan.Note{
			Code:    "NO_CARGO_LOCK",
			Message: fmt.Sprintf("%s has a Cargo.toml but no Cargo.lock, so its dependencies cannot be checked; run `cargo generate-lockfile` there, commit Cargo.lock, then run setup again.", f.dir),
			File:    path.Join(f.dir, "Cargo.toml"),
		})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Dir < projects[j].Dir })
	return projects, notes, nil
}

func readManifest(r plan.Repo, dir string) (cargoManifest, bool) {
	src, err := r.Read(path.Join(dir, "Cargo.toml"))
	if err != nil {
		return cargoManifest{}, false
	}
	return parseManifest(string(src)), true
}

func dirName(r plan.Repo, dir string) string {
	if dir == "." {
		return filepath.Base(filepath.Clean(r.Root))
	}
	return path.Base(dir)
}

// under reports whether dir is strictly inside parent.
func under(dir, parent string) bool {
	if parent == "." {
		return dir != "."
	}
	return strings.HasPrefix(dir, parent+"/")
}

func escapes(p string) bool { return p == ".." || strings.HasPrefix(p, "../") }

// top reports a directory where an upward walk stops: the repository root, or anything that is
// not inside it (path.Dir never leaves "/" or "..").
func top(d string) bool { return d == "." || d == "/" || d == "" || escapes(d) || path.IsAbs(d) }

// ---- workspaces and binaries ----

// crate is one package and the binaries Cargo discovers for it.
type crate struct {
	dir, name string
	bins      []string
}

// workspace is the package or workspace at a Cargo.lock root.
type workspace struct {
	root     string
	pkg      *crate // the root package; nil for a virtual manifest
	members  []crate
	defaults []string // default-members dirs; nil when not set
}

func loadWorkspace(r plan.Repo, root string) workspace {
	ws := workspace{root: root}
	m, ok := readManifest(r, root)
	if !ok {
		return ws
	}
	if m.pkg {
		ws.pkg = &crate{dir: root, name: crateName(r, root, m), bins: binaries(r, root, m)}
	}
	if !m.workspace {
		return ws
	}
	for _, d := range expandMembers(r, root, m.members) {
		mm, ok := readManifest(r, d)
		if d == root || !ok || !mm.pkg {
			continue
		}
		ws.members = append(ws.members, crate{dir: d, name: crateName(r, d, mm), bins: binaries(r, d, mm)})
	}
	if m.defaultMembers != nil {
		ws.defaults = append([]string{}, expandMembers(r, root, m.defaultMembers)...)
	}
	return ws
}

func (ws workspace) crates() []crate {
	var out []crate
	if ws.pkg != nil {
		out = append(out, *ws.pkg)
	}
	return append(out, ws.members...)
}

// binaries returns every binary of the root package and the members.
func (ws workspace) binaries() []string {
	var out []string
	for _, c := range ws.crates() {
		out = append(out, c.bins...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func crateName(r plan.Repo, dir string, m cargoManifest) string {
	if m.name != "" {
		return m.name
	}
	return dirName(r, dir)
}

// expandMembers resolves workspace member patterns (dir/* is expanded with filepath.Glob) to the
// repo-relative directories that hold a Cargo.toml, sorted.
func expandMembers(r plan.Repo, root string, patterns []string) []string {
	var out []string
	add := func(d string) {
		if !escapes(d) && r.Exists(path.Join(d, "Cargo.toml")) {
			out = append(out, d)
		}
	}
	for _, p := range patterns {
		rel := path.Join(root, p)
		if escapes(rel) {
			continue
		}
		if !strings.ContainsAny(p, "*?[") {
			add(rel)
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(r.Root, filepath.FromSlash(rel)))
		for _, m := range matches {
			if d, err := filepath.Rel(r.Root, m); err == nil {
				add(filepath.ToSlash(d))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// binaries follows Cargo's target auto-discovery: every [[bin]] without required-features, plus,
// unless discovery is off, the package name for src/main.rs, the stem of each src/bin/*.rs and
// the directory of each src/bin/*/main.rs. An inferred binary is dropped when a [[bin]] has its
// name or path. Discovery is off with autobins = false, and in edition 2015 (the default) once a
// [[bin]] is declared unless autobins = true.
func binaries(r plan.Repo, dir string, m cargoManifest) []string {
	names, paths := map[string]bool{}, map[string]bool{}
	var out []string
	for _, b := range m.bins {
		if b.name == "" {
			continue
		}
		names[b.name] = true
		if b.path != "" {
			paths[path.Clean(b.path)] = true
		}
		if !b.requiredFeatures {
			out = append(out, b.name)
		}
	}
	auto := len(m.bins) == 0 || m.edition != "" && m.edition != "2015"
	if m.autobins != nil {
		auto = *m.autobins
	}
	if auto {
		type bin struct{ name, path string }
		var inferred []bin
		if r.Exists(path.Join(dir, "src/main.rs")) {
			inferred = append(inferred, bin{crateName(r, dir, m), "src/main.rs"})
		}
		entries, _ := r.ReadDir(path.Join(dir, "src/bin"))
		for _, e := range entries {
			if stem, ok := strings.CutSuffix(e, ".rs"); ok && stem != "" {
				inferred = append(inferred, bin{stem, "src/bin/" + e})
			} else if r.Exists(path.Join(dir, "src/bin", e, "main.rs")) {
				inferred = append(inferred, bin{e, "src/bin/" + e + "/main.rs"})
			}
		}
		for _, b := range inferred {
			if !names[b.name] && !paths[b.path] {
				out = append(out, b.name)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ---- recognition ----

// invocation is a parsed `cargo build` or `cross build`.
type invocation struct {
	cross, auditable bool
	sub              shell.Word // the build subcommand word; "auditable " is inserted before it
	profile          string     // output directory: "release", or a custom profile's name
	uncertain        string     // why the output path is not certain; "" when it is
	triples          []string
	targetDir        string
	manifest         string // --manifest-path as written, "" when not given
	packages         []string
	excludes         []string
	bins             []string
	workspace        bool
	allBins          bool
}

var cargoValueOpts = []string{
	"-p", "--package", "--exclude", "--bin", "--example", "--test", "--bench", "-F", "--features",
	"--target", "--target-dir", "--profile", "--manifest-path", "-j", "--jobs", "--message-format",
	"--color", "--config", "-Z", "--artifact-dir", "--out-dir", "--lockfile-path",
}

func variable(s string) bool { return strings.ContainsAny(s, "$`") }

// parseInvocation recognizes a release `cargo [+tc] [auditable] build` (or `b`), and any
// `cross build`. `$(CARGO)`, `${CARGO}` and `$CARGO` count as cargo. A debug build, and one whose
// only target selectors are --lib, --example(s), --test(s) or --bench(es), is not a build.
func parseInvocation(cmd shell.Command) (invocation, bool) {
	var inv invocation
	args := cmd.ArgWords()
	if len(args) == 0 {
		return inv, false
	}
	switch prog := args[0].Value; {
	case path.Base(prog) == "cargo" || prog == "$(CARGO)" || prog == "${CARGO}" || prog == "$CARGO":
	case path.Base(prog) == "cross":
		inv.cross = true
	default:
		return inv, false
	}
	i := 1
	if i < len(args) && strings.HasPrefix(args[i].Value, "+") {
		i++
	}
	for i < len(args) && strings.HasPrefix(args[i].Value, "-") {
		name, _, eq := strings.Cut(args[i].Value, "=")
		if name == "--config" || name == "-C" {
			inv.uncertain = "it passes " + name
		}
		if !eq && slices.Contains([]string{"--color", "--config", "-Z", "-C"}, name) {
			i++
		}
		i++
	}
	if i < len(args) && !inv.cross && args[i].Value == "auditable" {
		inv.auditable = true
		i++
	}
	if i >= len(args) || args[i].Value != "build" && args[i].Value != "b" {
		return inv, false
	}
	inv.sub = args[i]
	release, debug, nonBin, binSel := false, false, false, false
	rest := args[i+1:]
	for j := 0; j < len(rest); j++ {
		v := rest[j].Value
		if v == "--" {
			break
		}
		if len(v) < 2 || v[0] != '-' {
			continue
		}
		name, val, eq := strings.Cut(v, "=")
		switch {
		case v[1] != '-' && len(v) > 2 && strings.IndexByte("pFj", v[1]) >= 0:
			name, val, eq = v[:2], strings.TrimPrefix(v[2:], "="), true
		case v[1] != '-' && strings.Trim(v[1:], "rvq") == "":
			if strings.Contains(v, "r") {
				release, inv.profile = true, "release"
			}
			continue
		}
		if !eq && slices.Contains(cargoValueOpts, name) {
			if j+1 >= len(rest) {
				break
			}
			j++
			val = rest[j].Value
		}
		switch name {
		case "--release":
			release, inv.profile = true, "release"
		case "--profile":
			switch {
			case variable(val):
				release, inv.uncertain = true, "--profile uses a variable"
			case val == "dev" || val == "test":
				debug = true
			case val == "release" || val == "bench":
				release, inv.profile = true, "release"
			default:
				release, inv.profile = true, val
			}
		case "--target":
			switch {
			case variable(val):
				inv.uncertain = "--target uses a variable"
			case strings.HasSuffix(val, ".json"):
				inv.uncertain = "--target names a target specification file"
			default:
				inv.triples = append(inv.triples, val)
			}
		case "--target-dir":
			if variable(val) || val == "" || path.IsAbs(val) || strings.HasPrefix(val, "~") {
				inv.uncertain = "--target-dir is a variable or an absolute path"
			}
			inv.targetDir = val
		case "--manifest-path":
			inv.manifest = val
		case "-p", "--package":
			inv.packages = append(inv.packages, val)
		case "--exclude":
			inv.excludes = append(inv.excludes, val)
		case "--workspace", "--all":
			inv.workspace = true
		case "--bin":
			inv.bins, binSel = append(inv.bins, val), true
		case "--bins", "--all-targets":
			inv.allBins, binSel = true, true
		case "--lib", "--example", "--examples", "--test", "--tests", "--bench", "--benches":
			nonBin = true
		case "--config":
			inv.uncertain = "it passes --config"
		}
	}
	if !inv.cross && (!release || debug) || nonBin && !binSel {
		return inv, false
	}
	return inv, true
}

// dir is the build's directory for attribution (R3): the --manifest-path directory, otherwise
// the command's; "" when it is not certain.
func (inv invocation) dir(cur string) string {
	m := inv.manifest
	if m == "" || cur == "" {
		return cur
	}
	if variable(m) || path.IsAbs(m) || strings.HasPrefix(m, "~") {
		return ""
	}
	d := path.Join(cur, path.Dir(m))
	if escapes(d) {
		return ""
	}
	return d
}

// Recognize classifies a cargo or cross build (R2). A release cargo build is a BuildBinary with
// the paths it writes, relative to its Cargo.lock root (R6), or with nil Paths and a
// RUST_PATH_UNKNOWN note when they are not certain. At Depth > 0 it is a BuildOther with
// RUST_CONDITIONAL_BUILD. A cross build is a BuildOther. Notes carry no location: the caller
// knows where the command is.
func (Plugin) Recognize(r plan.Repo, cmd shell.Command, ctx plan.Context) *plan.Build {
	inv, ok := parseInvocation(cmd)
	if !ok {
		return nil
	}
	ctx.Dir = plan.CleanDir(ctx.Dir)
	b := &plan.Build{Dir: inv.dir(ctx.Dir), Cmd: cmd}
	switch {
	case inv.cross:
		b.Kind = plan.BuildOther
	case cmd.Depth > 0:
		b.Kind = plan.BuildOther
		b.Notes = []plan.Note{{
			Code:    "RUST_CONDITIONAL_BUILD",
			Message: "this cargo build runs inside a shell conditional or loop, so it was rewritten but gets no binary target; the source target covers Cargo.lock",
		}}
	default:
		b.Kind = plan.BuildBinary
		paths, why := inv.paths(r, cmd, ctx, b.Dir)
		if why != "" {
			b.Notes = []plan.Note{pathUnknown(why, "", 0)}
		} else {
			b.Paths = paths
		}
	}
	return b
}

func pathUnknown(why, file string, line int) plan.Note {
	return plan.Note{
		Code:    "RUST_PATH_UNKNOWN",
		Message: fmt.Sprintf("the output path of this cargo build is not certain (%s), so it gets no binary target; the source target still covers Cargo.lock", why),
		File:    file,
		Line:    line,
	}
}

// paths returns the binaries the build writes, relative to its Cargo.lock root, or a reason why
// they are not certain.
func (inv invocation) paths(r plan.Repo, cmd shell.Command, ctx plan.Context, dir string) ([]string, string) {
	if dir == "" || ctx.Dir == "" {
		return nil, "its directory is not known"
	}
	if inv.uncertain != "" {
		return nil, inv.uncertain
	}
	for _, n := range []string{"CARGO_TARGET_DIR", "CARGO_BUILD_TARGET", "CARGO_BUILD_TARGET_DIR"} {
		if ctx.Env[n] || slices.Contains(cmd.Prefix(), n) || slices.Contains(cmd.Before.Env, n) {
			return nil, n + " is set"
		}
	}
	for d := ctx.Dir; ; d = path.Dir(d) {
		for _, f := range []string{".cargo/config.toml", ".cargo/config"} {
			if src, err := r.Read(path.Join(d, f)); err == nil && setsTarget(string(src)) {
				return nil, path.Join(d, f) + " sets [build] target or target-dir"
			}
		}
		if top(d) {
			break
		}
	}
	root := ""
	for d := dir; root == ""; d = path.Dir(d) {
		if r.Exists(path.Join(d, lockfile)) {
			root = d
		} else if top(d) {
			return nil, "no Cargo.lock was found at or above " + dir
		}
	}
	ws := loadWorkspace(r, root)
	manifestDir := dir
	if inv.manifest == "" {
		manifestDir = nearestManifest(r, ctx.Dir, root)
	}
	crates, why := ws.selected(inv, manifestDir)
	if why != "" {
		return nil, why
	}
	var union []string
	for _, c := range crates {
		union = append(union, c.bins...)
	}
	slices.Sort(union)
	bins := slices.Compact(union)
	if len(inv.bins) > 0 && !inv.allBins {
		bins = nil
		for _, b := range inv.bins {
			switch {
			case variable(b):
				return nil, "--bin uses a variable"
			case strings.ContainsAny(b, "*?["):
				for _, u := range union {
					if ok, _ := path.Match(b, u); ok {
						bins = append(bins, u)
					}
				}
			default:
				bins = append(bins, b)
			}
		}
		slices.Sort(bins)
		bins = slices.Compact(bins)
	}
	td := "target"
	if inv.targetDir != "" {
		full := path.Join(ctx.Dir, inv.targetDir)
		switch {
		case root == "." && !escapes(full):
			td = full
		case full == root:
			td = "."
		case strings.HasPrefix(full, root+"/"):
			td = strings.TrimPrefix(full, root+"/")
		default:
			return nil, "--target-dir is outside the project"
		}
	}
	triples := inv.triples
	if len(triples) == 0 {
		triples = []string{""}
	}
	out := []string{}
	for _, t := range triples {
		for _, b := range bins {
			out = append(out, path.Join(td, t, inv.profile, b))
		}
	}
	return out, ""
}

// nearestManifest returns the closest directory at or above from (and not above root) with a
// Cargo.toml: the package Cargo builds when no package is selected.
func nearestManifest(r plan.Repo, from, root string) string {
	for d := from; ; d = path.Dir(d) {
		if r.Exists(path.Join(d, "Cargo.toml")) || d == root || top(d) {
			if !under(d, root) {
				return root
			}
			return d
		}
	}
}

// selected returns the packages a build selects: -p, --workspace, the package in the manifest
// directory, or for the workspace root its default members, its root package, or every member.
func (ws workspace) selected(inv invocation, manifestDir string) ([]crate, string) {
	all := ws.crates()
	switch {
	case len(inv.packages) > 0:
		var out []crate
		for _, spec := range inv.packages {
			if variable(spec) || strings.ContainsAny(spec, "/#") {
				return nil, "-p " + spec + " is not a plain package name"
			}
			name, _, _ := strings.Cut(spec, "@")
			name, _, _ = strings.Cut(name, ":")
			n := len(out)
			for _, c := range all {
				if ok, _ := path.Match(name, c.name); ok || c.name == name {
					out = append(out, c)
				}
			}
			if len(out) == n {
				return nil, "package " + name + " is not in the workspace at " + ws.root
			}
		}
		return out, ""
	case inv.workspace:
		var out []crate
		for _, c := range all {
			if !slices.Contains(inv.excludes, c.name) {
				out = append(out, c)
			}
		}
		return out, ""
	case manifestDir == ws.root && ws.defaults != nil:
		var out []crate
		for _, d := range ws.defaults {
			i := slices.IndexFunc(all, func(c crate) bool { return c.dir == d })
			if i < 0 {
				return nil, "default member " + d + " is not a package"
			}
			out = append(out, all[i])
		}
		return out, ""
	case manifestDir == ws.root && ws.pkg != nil:
		return []crate{*ws.pkg}, ""
	case manifestDir == ws.root:
		return ws.members, ""
	}
	for _, c := range ws.members {
		if c.dir == manifestDir {
			return []crate{c}, ""
		}
	}
	return nil, manifestDir + " is not a member of the workspace at " + ws.root
}

// ---- preparation ----

// dockerCargo finds a cargo build in a Dockerfile, in shell or exec form.
var dockerCargo = regexp.MustCompile(`\bcargo\s+(\+\S+\s+)?(auditable\s+)?(build|b)\b|"cargo"\s*,\s*("auditable"\s*,\s*)?"(build|b)"`)

// Prepare returns, for a release cargo build that is not yet auditable, the insert of
// "auditable " before its build subcommand (in the CI script or in its Via file), and for any
// release cargo build that is not RewriteOnly the cargo-auditable install. It notes cross builds
// (RUST_CROSS), binary builds with unknown paths that carry no RUST_PATH_UNKNOWN yet, and image
// builds whose Dockerfile runs cargo build (RUST_DOCKERFILE). RewriteOnly builds get no notes.
func (Plugin) Prepare(r plan.Repo, _ plan.Project, b plan.Build) (plan.Prep, []plan.Note) {
	var prep plan.Prep
	var notes []plan.Note
	if b.Image != nil {
		if df := b.Image.Dockerfile; df != "" && !b.RewriteOnly {
			if src, err := r.Read(df); err == nil && dockerCargo.Match(src) {
				notes = append(notes, plan.Note{
					Code:    "RUST_DOCKERFILE",
					Message: fmt.Sprintf("cargo build inside %s is not rewritten, so the image's binary carries no crate list; the source target still covers Cargo.lock", df),
					File:    df,
				})
			}
		}
		return prep, notes
	}
	inv, ok := parseInvocation(b.Cmd)
	if !ok {
		return prep, nil
	}
	if b.RewriteOnly && inv.cross {
		return prep, nil
	}
	file, line := location(r, b)
	if inv.cross {
		return prep, []plan.Note{{
			Code:    "RUST_CROSS",
			Message: "cross builds run in a container and are not rewritten; the source target covers Cargo.lock",
			File:    file,
			Line:    line,
		}}
	}
	if !inv.auditable {
		prep.Inserts = []plan.Insert{{Pos: b.Pos, Via: b.Via, Offset: inv.sub.Start, Text: "auditable "}}
	}
	if b.RewriteOnly {
		return prep, nil
	}
	prep.Install = &plan.Install{
		Name:   "Install cargo-auditable",
		Uses:   "taiki-e/install-action@v2",
		With:   map[string]string{"tool": "cargo-auditable"},
		Run:    "cargo install cargo-auditable --locked",
		Marker: "cargo-auditable",
	}
	if b.Kind == plan.BuildBinary && b.Paths == nil && !slices.ContainsFunc(b.Notes, func(n plan.Note) bool { return n.Code == "RUST_PATH_UNKNOWN" }) {
		notes = append(notes, pathUnknown("its output path is not known", file, line))
	}
	return prep, notes
}

// location returns the Via file and line of a build found in a called file; for a build in the
// CI file it returns "", 0, and the caller locates it from Pos.
func location(r plan.Repo, b plan.Build) (string, int) {
	if b.Via == "" {
		return "", 0
	}
	src, err := r.Read(b.Via)
	if err != nil || b.Cmd.Start > len(src) {
		return b.Via, 0
	}
	return b.Via, 1 + bytes.Count(src[:b.Cmd.Start], []byte("\n"))
}
