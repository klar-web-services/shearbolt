package github

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

const (
	dailyPrefix = "shearbolt-daily-"
	dailyCron   = "23 4 * * *"
)

// dailyDropped are the job keys a daily run leaves out: the jobs it would wait for are not in the
// daily workflow, a condition written for pushes would skip the job, an environment records a
// deployment, a concurrency group would queue behind a real deploy, and outputs have no reader.
var dailyDropped = []string{"needs", "if", "environment", "concurrency", "outputs"}

var _ plan.DailyCI = Plugin{}

func (Plugin) IsDaily(file string) bool { return strings.HasPrefix(path.Base(file), dailyPrefix) }

func (Plugin) HasDaily(string, []byte) bool { return false }

// A schedule runs on the default branch only.
func (Plugin) NeedsBranch() bool { return false }

// Daily writes the daily workflow of file beside it: every job with a Shearbolt scan, up to its
// last scan, without the steps before it that only push or deploy.
func (p Plugin) Daily(r plan.Repo, file string, src []byte, _ string, classify plan.Classifier) (string, []byte, []plan.Note) {
	wf, err := parseWorkflow(file, src)
	if err != nil {
		return "", nil, nil
	}
	jobs, _, err := p.Jobs(r, file, src, classify)
	if err != nil || len(jobs) != len(wf.jobs) {
		return "", nil, nil
	}
	d := wf.doc
	var blocks []string
	var notes []plan.Note
	for i, j := range wf.jobs {
		last := lastScan(jobs[i])
		if last < 0 {
			continue
		}
		lines, err := dailyJob(d, j, jobs[i], last)
		if err != nil {
			notes = append(notes, plan.Note{Code: "DAILY_SKIPPED", File: file, Line: j.line,
				Message: fmt.Sprintf("job %s is not in the daily run, because %v. Its scan runs only when this workflow does.", j.key, err)})
			continue
		}
		blocks = append(blocks, lines...)
	}
	if len(blocks) == 0 {
		return "", nil, notes
	}
	root := d.Root()
	if kc, err := d.KeyColumn(root); err != nil || kc != 0 {
		return "", nil, notes
	}
	name := path.Base(file)
	out := []string{
		"# Shearbolt's daily run of " + name + ": its build and its scan, without the steps after the scan.",
		"# `shearbolt setup` writes this file. Run it again after you change " + name + ".",
		"name: " + yamledit.Scalar("Shearbolt daily ("+strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")+")"),
		"on:",
		"  schedule:",
		"    - cron: '" + dailyCron + "'",
		"  workflow_dispatch:",
	}
	for k := 0; k+1 < len(root.Content); k += 2 {
		key := root.Content[k]
		if !slices.Contains([]string{"permissions", "env", "defaults"}, key.Value) {
			continue
		}
		end, err := d.EntryEnd(root, key.Value)
		if err != nil {
			return "", nil, notes
		}
		out = append(out, docLines(d, key.Line, end)...)
	}
	out = append(append(out, "jobs:"), blocks...)
	twin := workflowsDir + "/" + dailyPrefix + name
	notes = append(notes, plan.Note{Code: "DAILY_RUN", File: twin,
		Message: fmt.Sprintf("runs the build and the scan of %s once a day (04:23 UTC) on the default branch, without the steps after the scan; it runs once this file is on the default branch. GitHub turns scheduled workflows off in a public repository after 60 days without activity (Actions → the workflow → Enable workflow turns it on again).", name)})
	return twin, []byte(strings.Join(out, d.EOL()) + d.EOL()), notes
}

func lastScan(j plan.Job) int {
	last := -1
	for s, st := range j.Steps {
		if slices.ContainsFunc(st.Gates, func(g plan.GateCmd) bool { return !g.Install }) {
			last = s
		}
	}
	return last
}

func docLines(d *yamledit.Doc, first, last int) []string {
	var out []string
	for l := first; l <= last; l++ {
		out = append(out, d.Line(l))
	}
	return out
}

// dailyJob returns the lines of job j up to step last, or why the job cannot run on its own
// without pushing.
func dailyJob(d *yamledit.Doc, j *job, pj plan.Job, last int) ([]string, error) {
	if flow(j.node) || len(pj.Steps) != len(j.steps) {
		return nil, fmt.Errorf("it cannot be read line by line")
	}
	omit := map[int]bool{}
	for s := 0; s <= last; s++ {
		ps, st := pj.Steps[s], j.steps[s]
		for _, b := range ps.Builds {
			if b.Image != nil && (b.Image.Push || b.Image.OutputPush) {
				return nil, fmt.Errorf("the build at line %d pushes its image", st.line)
			}
		}
		switch {
		case len(ps.Push) == 0:
		case len(ps.Builds) > 0 || len(ps.Gates) > 0:
			return nil, fmt.Errorf("the step at line %d builds or scans and also pushes or deploys", st.line)
		default:
			omit[s] = true
		}
		if rawValue(j.node, "needs") != nil && actionIs(st.uses, "actions/download-artifact") {
			return nil, fmt.Errorf("the step at line %d downloads what another job built", st.line)
		}
	}
	out := []string{d.Line(j.line)}
	for k := 0; k+1 < len(j.node.Content); k += 2 {
		key := j.node.Content[k]
		switch {
		case slices.Contains(dailyDropped, key.Value):
		case key.Value == "steps" && j.node.Content[k+1] == j.seq:
			out = append(out, d.Line(key.Line))
			for s := 0; s <= last; s++ {
				if omit[s] {
					continue
				}
				first, err := d.ItemStart(j.seq, s)
				if err != nil {
					return nil, err
				}
				end, err := d.ItemEnd(j.seq, s)
				if err != nil {
					return nil, err
				}
				out = append(out, docLines(d, first, end)...)
			}
		default:
			if key.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("it cannot be read line by line")
			}
			end, err := d.EntryEnd(j.node, key.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, docLines(d, key.Line, end)...)
		}
	}
	if rawValue(j.node, "needs") != nil && strings.Contains(strings.Join(out, "\n"), "needs.") {
		return nil, fmt.Errorf("it reads the outputs of the jobs it needs")
	}
	return out, nil
}
