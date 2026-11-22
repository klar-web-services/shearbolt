package golang

import (
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

// In scan.Lockfiles. Grype reads the requirements from go.mod; go.sum only adds checksums.
const lockfile = "go.mod"

var (
	modulePath = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)`)
	requires   = regexp.MustCompile(`(?m)^\s*require\b`)
	majorPart  = regexp.MustCompile(`^v[0-9]+$`)
)

type Plugin struct{}

func (Plugin) Name() string { return "go" }

// A module without requirements is not a project: Grype reads no packages from its go.mod, so a
// source target for it would fail.
func (Plugin) Find(r plan.Repo) ([]plan.Project, []plan.Note, error) {
	var projects []plan.Project
	var notes []plan.Note
	err := r.Walk(func(dir string, names []string) {
		if !slices.Contains(names, lockfile) {
			return
		}
		file := path.Join(dir, lockfile)
		src, err := r.Read(file)
		if err != nil {
			return
		}
		if !requires.Match(src) {
			notes = append(notes, plan.Note{
				Code:    "GO_NO_REQUIREMENTS",
				Message: fmt.Sprintf("%s requires no modules, so there are no dependencies in it to check and setup does not protect it.", file),
				File:    file,
			})
			return
		}
		projects = append(projects, plan.Project{Ecosystem: "go", Dir: dir, Name: moduleName(r, dir, src), Lockfiles: []string{lockfile}})
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Dir < projects[j].Dir })
	return projects, notes, nil
}

// moduleName is the last element of the module path, without a major version suffix; the
// directory's name when go.mod names no module.
func moduleName(r plan.Repo, dir string, src []byte) string {
	if m := modulePath.FindSubmatch(src); m != nil {
		parts := strings.Split(string(m[1]), "/")
		if len(parts) > 1 && majorPart.MatchString(parts[len(parts)-1]) {
			parts = parts[:len(parts)-1]
		}
		if name := parts[len(parts)-1]; name != "" {
			return name
		}
	}
	if dir == "." {
		return filepath.Base(filepath.Clean(r.Root))
	}
	return path.Base(dir)
}

// boolFlags are the go build flags that take no value; every other flag takes one.
var boolFlags = []string{"a", "n", "race", "msan", "asan", "cover", "v", "work", "x", "linkshared", "modcacherw", "trimpath", "buildvcs", "json"}

type invocation struct {
	out      *shell.Word // the -o value
	chdir    *shell.Word // the -C value
	patterns bool        // a package argument holds "..."
}

func parseInvocation(cmd shell.Command) (invocation, bool) {
	var inv invocation
	args := cmd.ArgWords()
	if len(args) < 2 || path.Base(args[0].Value) != "go" || args[1].Value != "build" {
		return inv, false
	}
	rest := args[2:]
	i := 0
	for ; i < len(rest); i++ {
		v := rest[i].Value
		if v == "--" {
			i++
			break
		}
		if len(v) < 2 || v[0] != '-' {
			break
		}
		name, val, eq := strings.Cut(strings.TrimLeft(v, "-"), "=")
		w := &shell.Word{Raw: val, Value: val}
		if !eq {
			if slices.Contains(boolFlags, name) || i+1 >= len(rest) {
				continue
			}
			i++
			w = &rest[i]
		}
		switch name {
		case "o":
			inv.out = w
		case "C":
			inv.chdir = w
		}
	}
	for _, p := range rest[min(i, len(rest)):] {
		inv.patterns = inv.patterns || strings.Contains(p.Value, "...")
	}
	return inv, true
}

func uncertain(w *shell.Word) bool {
	return w.Value == "" || strings.ContainsAny(w.Raw, "$`~") || strings.ContainsAny(w.Value, "$`")
}

func escapes(p string) bool { return p == ".." || strings.HasPrefix(p, "../") }

// Recognize takes every `go build`. One that names its binary with -o, outside a shell
// conditional or loop, is a binary build whose path is known; one that only checks that packages
// compile (`go build ./...`) writes nothing. Notes carry no location: the caller knows where the
// command is.
func (Plugin) Recognize(r plan.Repo, cmd shell.Command, ctx plan.Context) *plan.Build {
	inv, ok := parseInvocation(cmd)
	if !ok {
		return nil
	}
	dir := plan.CleanDir(ctx.Dir)
	if inv.chdir != nil && dir != "" {
		dir = ""
		if !uncertain(inv.chdir) {
			dir = plan.CleanDir(path.Join(plan.CleanDir(ctx.Dir), inv.chdir.Value))
		}
	}
	b := &plan.Build{Kind: plan.BuildBinary, Dir: dir, Cmd: cmd}
	unknown := func(why string) *plan.Build {
		b.Notes = []plan.Note{{
			Code:    "GO_PATH_UNKNOWN",
			Message: fmt.Sprintf("the output path of this go build is not certain (%s), so it gets no binary target; the source target still covers go.mod", why),
		}}
		return b
	}
	switch {
	case cmd.Depth > 0:
		b.Kind = plan.BuildOther
		return unknown("it runs inside a shell conditional or loop")
	case inv.out == nil && inv.patterns:
		b.Kind = plan.BuildOther
		return b
	case inv.out == nil:
		return unknown("it names no output with -o")
	case dir == "":
		return unknown("its directory is not known")
	case uncertain(inv.out):
		return unknown("-o uses a variable")
	case path.IsAbs(inv.out.Value):
		return unknown("-o is an absolute path")
	case strings.HasSuffix(inv.out.Value, "/"):
		return unknown("-o names a directory")
	}
	root := dir
	for !r.Exists(path.Join(root, lockfile)) {
		if root == "." {
			return unknown("no go.mod was found at or above " + dir)
		}
		root = path.Dir(root)
	}
	rel, err := filepath.Rel(filepath.FromSlash(root), filepath.FromSlash(path.Join(dir, inv.out.Value)))
	if rel = filepath.ToSlash(rel); err != nil || escapes(rel) {
		return unknown("-o is outside its module")
	}
	b.Paths = []string{rel}
	return b
}

func (Plugin) Prepare(plan.Repo, plan.Project, plan.Build) (plan.Prep, []plan.Note) {
	return plan.Prep{}, nil
}
