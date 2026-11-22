package setup

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/scan"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

// handGates finds the projects that an existing gate already checks through a config other than
// the project's own shearbolt.yaml (one written by hand, as for a release gate): a config with a
// source target whose tree holds the project. It returns that gate per project dir and job ID,
// for every job that runs together with the gate: the jobs of its file (a GitHub workflow, the
// GitLab pipeline), or on Bitbucket, where one file holds independent pipelines, its own step.
func handGates(r plan.Repo, projects []plan.Project, files []*ciSource) map[string]map[string]plan.GateCmd {
	out := map[string]map[string]plan.GateCmd{}
	for _, f := range files {
		for _, job := range f.jobs {
			with := f.jobs
			if f.ci.Name() == "bitbucket" {
				with = []plan.Job{job}
			}
			for _, g := range scanGates(job) {
				for _, p := range projects {
					if p.Ecosystem == plan.EcosystemDocker || g.Config == "" || g.Config == configFile(p) || !checksSource(r, g.Config, p.Dir) {
						continue
					}
					if out[p.Dir] == nil {
						out[p.Dir] = map[string]plan.GateCmd{}
					}
					for _, j := range with {
						if _, ok := out[p.Dir][j.ID]; !ok {
							out[p.Dir][j.ID] = g
						}
					}
				}
			}
		}
	}
	return out
}

func scanGates(job plan.Job) []plan.GateCmd {
	var out []plan.GateCmd
	for _, st := range job.Steps {
		for _, g := range st.Gates {
			if !g.Install {
				out = append(out, g)
			}
		}
	}
	return out
}

// checksSource reports whether the config at file has a source target whose tree holds dir and
// does not exclude it. Target paths are relative to the config file.
func checksSource(r plan.Repo, file, dir string) bool {
	src, err := r.Read(file)
	if err != nil {
		return false
	}
	_, _, targets, reason := editableTargets(src)
	if reason != "" {
		return false
	}
	for _, t := range targets {
		root, ok := relPath(path.Dir(file), t.Source)
		if !ok {
			continue
		}
		rel := "."
		if dir != root {
			if rel, ok = insideDir(root, dir); !ok {
				continue
			}
		}
		if !slices.ContainsFunc(t.Exclude, func(e string) bool {
			e = strings.Trim(e, "/")
			return rel == e || strings.HasPrefix(rel, e+"/")
		}) {
			return true
		}
	}
	return false
}

func alreadyGatedNote(p plan.Project, g plan.GateCmd) plan.Note {
	return plan.Note{
		Code: "ALREADY_GATED",
		Message: fmt.Sprintf("this Shearbolt gate already checks the lockfiles of %s through %s, a config setup did not write, so setup adds no gate for %s to this workflow or pipeline and changes nothing in it. Setup did not check what else that config scans or where the gate runs: make sure it covers what is built here and that every push or deploy waits for it.",
			projectLabel(p), g.Config, projectLabel(p)),
		File: g.Pos.File,
		Line: g.Pos.Line,
	}
}

const maxListedLockfiles = 5

// noProjectHelp is what NO_PROJECT adds about a repository setup cannot set up but Shearbolt can
// still check: the gates CI already runs, and the lockfiles `shearbolt scan` reads. A lockfile
// that a note of notes names is left out: that note says why it cannot be checked.
func noProjectHelp(root string, files []*ciSource, notes []plan.Note) string {
	var msg string
	var gates []string
	for _, f := range files {
		for _, job := range f.jobs {
			for _, g := range scanGates(job) {
				gates = append(gates, fmt.Sprintf("%s:%d", g.Pos.File, g.Pos.Line))
			}
		}
	}
	if len(gates) > 0 {
		msg += "\nCI already runs Shearbolt at " + strings.Join(gates, ", ") + "; setup leaves what is there as it is."
	}
	found, err := scan.FindLockfiles(root, nil)
	if err != nil {
		return msg
	}
	found = slices.DeleteFunc(found, func(l string) bool {
		return slices.ContainsFunc(notes, func(n plan.Note) bool { return n.File == l })
	})
	if len(found) == 0 {
		return msg
	}
	slices.Sort(found)
	list := strings.Join(found[:min(len(found), maxListedLockfiles)], ", ")
	if more := len(found) - maxListedLockfiles; more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return msg + "\nShearbolt can still check what setup cannot set up: `shearbolt scan --source .` reads these lockfiles: " + list +
		". To gate them in CI, write shearbolt.yaml and the scan step by hand (the README's reference shows both)."
}
