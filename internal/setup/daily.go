package setup

import (
	"bytes"
	"errors"
	"io/fs"
	"slices"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

// dailyQuestions asks whether to add a daily run, and for which branch when a CI here makes it
// for one. They are asked only while the repository has no daily run: one that is there is kept.
func dailyQuestions(files []*ciSource, o Options) []plan.Question {
	can, branch := false, false
	for _, f := range files {
		if d, ok := f.ci.(plan.DailyCI); ok && len(f.jobs) > 0 {
			can = true
			branch = branch || d.NeedsBranch()
		}
	}
	if !can {
		return nil
	}
	yesNo := []plan.Option{{Value: "yes", Label: "yes (recommended)"}, {Value: "no", Label: "no"}}
	qs := []plan.Question{{
		ID:      "daily",
		Prompt:  "Add a daily scheduled run? It builds and scans once a day and pushes or deploys nothing, so a dependency that becomes vulnerable is found even when nobody pushes.",
		Kind:    "choice",
		Options: yesNo,
		Default: "yes",
	}}
	if !branch {
		return qs
	}
	def, branches := o.DefaultBranch, slices.Clone(o.Branches)
	if def == "" {
		def = "main"
		if !slices.Contains(branches, "main") && len(branches) > 0 {
			def = branches[0]
		}
	}
	if !slices.Contains(branches, def) {
		branches = append(branches, def)
	}
	slices.Sort(branches)
	q := plan.Question{ID: "daily-branch", Prompt: "Which branch should the daily run build?", Kind: "choice", Default: def, OnlyIf: "daily=yes"}
	for _, b := range slices.Compact(branches) {
		q.Options = append(q.Options, plan.Option{Value: b, Label: b})
	}
	return append(qs, q)
}

// dailyChanges adds the daily run of every CI file that can hold one to changes. A daily run in
// a file of its own is rewritten from its pipeline, so it follows the pipeline's build.
func dailyChanges(r plan.Repo, files []*ciSource, changes []plan.FileChange, branch string, classify plan.Classifier) ([]plan.FileChange, []plan.Note) {
	var notes []plan.Note
	for _, f := range files {
		d, ok := f.ci.(plan.DailyCI)
		if !ok {
			continue
		}
		at := slices.IndexFunc(changes, func(c plan.FileChange) bool { return c.Path == f.path })
		src := f.src
		if at >= 0 {
			src = []byte(changes[at].Content)
		}
		path, content, dn := d.Daily(r, f.path, src, branch, classify)
		changed := false
		switch {
		case path == "":
		case path == f.path:
			if changed = !bytes.Equal(content, src); changed && at >= 0 {
				changes[at].Content = string(content)
			} else if changed {
				changes = append(changes, plan.FileChange{Path: path, Action: "modify", Content: string(content), Base: f.src})
			}
		default:
			cur, err := r.Read(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				changes = append(changes, plan.FileChange{Path: path, Action: "create", Content: string(content)})
				changed = true
			case err == nil && !bytes.Equal(cur, content):
				changes = append(changes, plan.FileChange{Path: path, Action: "modify", Content: string(content), Base: cur})
				changed = true
			}
		}
		for _, n := range dn {
			if changed || n.Code == "DAILY_SKIPPED" {
				notes = append(notes, n)
			}
		}
	}
	return changes, notes
}
