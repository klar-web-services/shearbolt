// Package npm is the setup plugin for npm, pnpm, Yarn and Bun projects: it finds them (R1) and
// recognizes their build scripts (R2). Their builds produce no artifact Shearbolt scans, so they
// are gate locations only, and Prepare has nothing to do.
package npm

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// lockfiles make a directory with package.json an npm project. Every name is in scan.Lockfiles.
var lockfiles = []string{"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock"}

// Plugin is the npm ecosystem.
type Plugin struct{}

// Name returns "npm".
func (Plugin) Name() string { return "npm" }

// Find returns every directory with package.json and an npm lockfile, sorted by Dir. A workspace
// member has no lockfile of its own, so it is covered by its root and is not a project. A
// directory whose only lockfile is the binary bun.lockb gets a BUN_LOCKB note instead.
func (Plugin) Find(r plan.Repo) ([]plan.Project, []plan.Note, error) {
	var projects []plan.Project
	var notes []plan.Note
	err := r.Walk(func(dir string, names []string) {
		if !slices.Contains(names, "package.json") {
			return
		}
		var locks []string
		for _, l := range lockfiles {
			if slices.Contains(names, l) {
				locks = append(locks, l)
			}
		}
		if len(locks) == 0 {
			if slices.Contains(names, "bun.lockb") {
				notes = append(notes, plan.Note{
					Code: "BUN_LOCKB",
					Message: fmt.Sprintf("%s uses the binary bun.lockb, which Grype cannot read. Run `bun install --save-text-lockfile` there, commit bun.lock, then run setup again.",
						dir),
					File: path.Join(dir, "bun.lockb"),
				})
			}
			return
		}
		name, framework := manifest(r, dir)
		if name == "" {
			name = path.Base(dir)
			if dir == "." {
				name = filepath.Base(filepath.Clean(r.Root))
			}
		}
		projects = append(projects, plan.Project{Ecosystem: "npm", Dir: dir, Name: name, Lockfiles: locks, Framework: framework})
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Dir < projects[j].Dir })
	return projects, notes, nil
}

// manifest returns the name in dir/package.json and "nextjs" when next is a dependency. A
// package.json that cannot be read gives "", "": the lockfile still makes it a project.
func manifest(r plan.Repo, dir string) (name, framework string) {
	b, err := r.Read(path.Join(dir, "package.json"))
	if err != nil {
		return "", ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return "", ""
	}
	if json.Unmarshal(m["name"], &name) != nil {
		name = ""
	}
	for _, k := range []string{"dependencies", "devDependencies"} {
		var deps map[string]json.RawMessage
		if json.Unmarshal(m[k], &deps) == nil {
			if _, ok := deps["next"]; ok {
				framework = "nextjs"
			}
		}
	}
	return strings.TrimSpace(name), framework
}

// valueFlags are each runner's options that take a separate value, so the value is not
// mistaken for a script name.
var valueFlags = map[string][]string{
	"npm":  {"--prefix", "-w", "--workspace", "--loglevel"},
	"pnpm": {"-C", "--dir", "--filter", "-F", "--loglevel", "--reporter"},
	"yarn": {"--cwd"},
	"bun":  {"--cwd", "--filter", "-F"},
	"npx":  {"-p", "--package"},
	"bunx": {"-p", "--package"},
}

// dirFlags name the directory a runner works in: npm --prefix, pnpm -C/--dir, yarn and bun --cwd.
var dirFlags = []string{"--prefix", "-C", "--dir", "--cwd"}

// Recognize returns a BuildOther for an npm build script (`npm run build[:x]`, `pnpm build`,
// `yarn run build`, `bun run build`, ...) or `next build` run directly or through npx, bunx or
// a runner's exec. Its Dir is the runner's directory option joined to ctx.Dir, or "" when that
// is not certain (unknown ctx.Dir, a variable, an absolute path or a path outside the repo).
func (Plugin) Recognize(_ plan.Repo, cmd shell.Command, ctx plan.Context) *plan.Build {
	args := cmd.ArgWords()
	if len(args) == 0 {
		return nil
	}
	ctx.Dir = plan.CleanDir(ctx.Dir)
	prog := path.Base(args[0].Value)
	pos, dirArg := split(args[1:], valueFlags[prog])
	at := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}
	nextBuild := func(i int) bool { return at(i) == "next" && at(i+1) == "build" }
	script := func(s string) bool { return s == "build" || strings.HasPrefix(s, "build:") }
	build := false
	switch prog {
	case "npm":
		switch at(0) {
		case "run", "run-script":
			build = script(at(1))
		case "exec":
			build = nextBuild(1)
		}
	case "pnpm", "yarn":
		switch at(0) {
		case "run":
			build = script(at(1)) || nextBuild(1)
		case "exec", "dlx":
			build = nextBuild(1)
		default:
			build = script(at(0)) || nextBuild(0)
		}
	case "bun":
		switch at(0) {
		case "run":
			build = script(at(1)) || nextBuild(1)
		case "x":
			build = nextBuild(1)
		}
	case "npx", "bunx":
		build = nextBuild(0)
	case "next":
		build = at(0) == "build"
	}
	if !build {
		return nil
	}
	return &plan.Build{Kind: plan.BuildOther, Dir: buildDir(ctx.Dir, dirArg), Cmd: cmd}
}

// split returns the positional argument values (everything after "--" included) and the raw
// value of the last directory option, or nil when there is none.
func split(args []shell.Word, withValue []string) (pos []string, dir *shell.Word) {
	for i := 0; i < len(args); i++ {
		w := args[i]
		v := w.Value
		switch {
		case v == "--":
			for _, a := range args[i+1:] {
				pos = append(pos, a.Value)
			}
			return pos, dir
		case strings.HasPrefix(v, "-") && len(v) > 1:
			name, value, eq := strings.Cut(v, "=")
			takes := slices.Contains(withValue, name) || slices.Contains(dirFlags, name)
			if eq || !takes {
				if eq && slices.Contains(dirFlags, name) {
					dir = &shell.Word{Raw: w.Raw, Value: value, Start: w.Start}
				}
				continue
			}
			if i+1 < len(args) {
				i++
				if slices.Contains(dirFlags, name) {
					dir = &args[i]
				}
			}
		default:
			pos = append(pos, v)
		}
	}
	return pos, dir
}

// buildDir joins a directory option to the command's directory, or returns "" when the result
// is not certain.
func buildDir(cur string, arg *shell.Word) string {
	if arg == nil || cur == "" {
		return cur
	}
	v := arg.Value
	if v == "" || strings.ContainsAny(arg.Raw, "$`~") || strings.ContainsAny(v, "$`") || path.IsAbs(v) {
		return ""
	}
	d := path.Join(cur, v)
	if d == ".." || strings.HasPrefix(d, "../") {
		return ""
	}
	return d
}

// Prepare has nothing to do: npm builds need no rewrite or tool.
func (Plugin) Prepare(plan.Repo, plan.Project, plan.Build) (plan.Prep, []plan.Note) {
	return plan.Prep{}, nil
}
