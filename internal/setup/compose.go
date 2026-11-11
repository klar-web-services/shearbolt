package setup

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/compose"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

// findCompose returns the repo-relative paths of the compose files to gate (those that pull at
// least one image) in walk order, and notes for the others and for unset image variables:
// COMPOSE_NOT_READ (unreadable, invalid or unsupported), COMPOSE_NO_IMAGES (pulls nothing),
// COMPOSE_VARIABLE (one per service whose image needs an unset variable; the file is gated).
func findCompose(r plan.Repo) ([]string, []plan.Note, error) {
	var candidates []string
	err := r.Walk(func(dir string, names []string) {
		for _, n := range names {
			if compose.FileName(n) {
				candidates = append(candidates, path.Join(dir, n))
			}
		}
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	noEnv := func(string) (string, bool) { return "", false }
	var files []string
	var notes []plan.Note
	for _, f := range candidates {
		data, err := r.Read(f)
		if err != nil {
			notes = append(notes, plan.Note{Code: "COMPOSE_NOT_READ", Message: fmt.Sprintf("%s was not gated: %v", f, err), File: f})
			continue
		}
		parsed := compose.Parse(data, noEnv, nil)
		if parsed.Fatal != nil {
			notes = append(notes, plan.Note{
				Code:    "COMPOSE_NOT_READ",
				Message: fmt.Sprintf("%s was not gated: %s", f, parsed.Fatal.Message),
				File:    f,
				Line:    parsed.Fatal.Line,
			})
			continue
		}
		if !parsed.Pulls() {
			notes = append(notes, plan.Note{
				Code:    "COMPOSE_NO_IMAGES",
				Message: f + " pulls no image (only build services or overrides), so there is nothing to scan; setup did not gate it",
				File:    f,
			})
			continue
		}
		files = append(files, f)
		for _, m := range parsed.Missing {
			notes = append(notes, plan.Note{Code: "COMPOSE_VARIABLE", Message: m.Message, File: f, Line: m.Line})
		}
	}
	return files, notes, nil
}

// attachCompose gives each compose file to the deepest npm or Rust project whose Dir contains
// it, as a path relative to that Dir, and puts the rest into one compose-only project at "."
// named after the repository directory. projects is sorted by Dir and stays sorted.
func attachCompose(projects []plan.Project, files []string, root string) []plan.Project {
	var rest []string
	for _, f := range files {
		best := -1
		rel := ""
		for i, p := range projects {
			var r string
			if p.Dir == "." {
				r = f
			} else if cut, ok := strings.CutPrefix(f, p.Dir+"/"); ok {
				r = cut
			} else {
				continue
			}
			if best < 0 || projects[best].Dir == "." || (p.Dir != "." && len(p.Dir) > len(projects[best].Dir)) {
				best, rel = i, r
			}
		}
		if best < 0 {
			rest = append(rest, f)
			continue
		}
		projects[best].Compose = append(projects[best].Compose, rel)
	}
	if len(rest) > 0 {
		projects = append(projects, plan.Project{
			Ecosystem: plan.EcosystemDocker,
			Dir:       ".",
			Name:      filepath.Base(root),
			Lockfiles: []string{},
			Compose:   rest,
		})
		slices.SortStableFunc(projects, func(a, b plan.Project) int { return strings.Compare(a.Dir, b.Dir) })
	}
	return projects
}

// attachImageBuilds returns projects with a root project (ecosystem docker, Dir ".", named after
// the repository directory, no lockfiles) when no project is npm or Rust and jobs hold at least
// one non-RewriteOnly image build. An existing root project is kept as it is. projects is
// sorted by Dir and stays sorted.
func attachImageBuilds(projects []plan.Project, jobs []plan.Job, root string) []plan.Project {
	// An npm or Rust project owns the builds; a root project is already there.
	if slices.ContainsFunc(projects, func(p plan.Project) bool { return p.Ecosystem != plan.EcosystemDocker || p.Dir == "." }) {
		return projects
	}
	built := slices.ContainsFunc(jobs, func(j plan.Job) bool {
		return slices.ContainsFunc(j.Steps, func(s plan.Step) bool {
			return slices.ContainsFunc(s.Builds, func(b plan.Build) bool { return b.Kind == plan.BuildImage && !b.RewriteOnly })
		})
	})
	if !built {
		return projects
	}
	projects = append(projects, plan.Project{
		Ecosystem: plan.EcosystemDocker,
		Dir:       ".",
		Name:      filepath.Base(root),
		Lockfiles: []string{},
	})
	slices.SortStableFunc(projects, func(a, b plan.Project) int { return strings.Compare(a.Dir, b.Dir) })
	return projects
}
