package bitbucket

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

const (
	dailyName = "shearbolt-daily"
	dailyPath = "custom/" + dailyName
)

var _ plan.DailyCI = Plugin{}

func (Plugin) IsDaily(string) bool { return false }

func (Plugin) HasDaily(_ string, src []byte) bool {
	d, err := yamledit.Parse(src)
	return err == nil && rawValue(rawValue(rawValue(d.Root(), "pipelines"), "custom"), dailyName) != nil
}

// A Bitbucket schedule names the branch it runs.
func (Plugin) NeedsBranch() bool { return true }

// Daily adds the custom pipeline shearbolt-daily: the pipeline that runs for branch, up to its
// last Shearbolt scan, without the steps and commands that push or deploy. Bitbucket keeps
// schedules outside the file, so a note says how to schedule it. A daily pipeline that is there
// already is kept as it is.
func (p Plugin) Daily(r plan.Repo, file string, src []byte, branch string, classify plan.Classifier) (string, []byte, []plan.Note) {
	pl, err := parse(file, src)
	if err != nil {
		return "", nil, nil
	}
	d := pl.doc
	top := rawValue(d.Root(), "pipelines")
	if top == nil || top.Kind != yaml.MappingNode || flow(top) || rawValue(rawValue(top, "custom"), dailyName) != nil {
		return "", nil, nil
	}
	skipped := func(format string, args ...any) (string, []byte, []plan.Note) {
		return "", nil, []plan.Note{{Code: "DAILY_SKIPPED", File: file,
			Message: "no daily pipeline was added, because " + fmt.Sprintf(format, args...) + ". The scan runs only when the pipeline does."}}
	}
	name, list := pipelineFor(top, branch)
	seq := resolve(list)
	if seq == nil || seq.Kind != yaml.SequenceNode || flow(seq) || len(seq.Content) == 0 {
		return skipped("no pipeline that setup can read runs for branch %s", branch)
	}
	unit := yamledit.IndentUnit(d)
	kc, err := d.KeyColumn(top)
	if err != nil {
		return "", nil, nil
	}
	custom := rawValue(top, "custom")
	var out []string
	after, err := d.EntryEnd(d.Root(), "pipelines")
	switch {
	case custom == nil:
		out = append(out, strings.Repeat(" ", kc)+"custom:")
		kc += unit
	case custom.Kind != yaml.MappingNode || flow(custom) || len(custom.Content) == 0:
		return skipped("its custom pipelines cannot be edited in place")
	default:
		kc = custom.Content[0].Column - 1
		after, err = d.EntryEnd(top, "custom")
	}
	if err != nil {
		return "", nil, nil
	}
	items, err := pl.dailyItems(seq, kc+unit, classify)
	if err != nil {
		return skipped("%v", err)
	}
	if items == nil {
		return skipped("the pipeline that runs for branch %s (%s) has no Shearbolt scan", branch, name)
	}
	pad := strings.Repeat(" ", kc)
	out = append(out,
		pad+"# Shearbolt's daily run for "+branch+": the build and the scan of "+name+", without the push or deploy.",
		pad+"# Schedule it under Pipelines > Schedules.",
		pad+dailyName+":")
	if err := d.InsertAfter(after, append(out, items...)); err != nil {
		return skipped("the file cannot be edited there (%v)", err)
	}
	res, err := d.Result()
	if err != nil || !p.HasDaily(file, res) {
		return skipped("the edited file would not be valid YAML")
	}
	return file, res, []plan.Note{{Code: "DAILY_SCHEDULE", File: file,
		Message: fmt.Sprintf("setup added the pipeline custom: %s, which builds and scans like %s and pushes or deploys nothing. Bitbucket keeps schedules outside this file: once this is on branch %s, open Pipelines → Schedules → New schedule, choose branch %s and the pipeline custom: %s, and set it to run daily.", dailyName, name, branch, branch, dailyName)}}
}

// pipelineFor returns the pipeline Bitbucket runs for a push to branch: the branches entry with
// that name, else the first whose pattern matches, else default.
func pipelineFor(top *yaml.Node, branch string) (string, *yaml.Node) {
	if m := resolve(rawValue(top, "branches")); m != nil && m.Kind == yaml.MappingNode {
		for _, match := range []func(string) bool{func(k string) bool { return k == branch }, func(k string) bool { return globMatch(k, branch) }} {
			for k := 0; k+1 < len(m.Content); k += 2 {
				if match(m.Content[k].Value) {
					return "branches/" + m.Content[k].Value, m.Content[k+1]
				}
			}
		}
	}
	if v := rawValue(top, "default"); v != nil {
		return "default", v
	}
	return "", nil
}

// globMatch matches a Bitbucket branch pattern: * stays inside one path segment, ** crosses
// them, and {a,b} is either.
func globMatch(pattern, name string) bool {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '{':
			b.WriteString("(?:")
			depth++
		case c == '}' && depth > 0:
			b.WriteString(")")
			depth--
		case c == ',' && depth > 0:
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re, err := regexp.Compile("^(?:" + b.String() + ")$")
	return err == nil && re.MatchString(name)
}

// dailyStep is what the daily pipeline needs to know about one step.
type dailyStep struct {
	st     *step
	scan   int  // the script item of its last Shearbolt scan, -1 without one
	pushes bool // it pushes, deploys or waits for a manual start
	early  bool // a script item up to the scan pushes
	after  bool // its after-script pushes
}

func (pl *pipelines) dailyStep(m *yaml.Node, classify plan.Classifier) (*dailyStep, error) {
	i := slices.IndexFunc(pl.steps, func(s *step) bool { return s.m == m })
	if i < 0 {
		return nil, fmt.Errorf("the step at line %d cannot be read", m.Line)
	}
	ds := &dailyStep{st: pl.steps[i], scan: -1}
	steps := ds.st.script.Steps(classify, nil)
	for s, ps := range steps {
		if slices.ContainsFunc(ps.Gates, func(g plan.GateCmd) bool { return !g.Install }) {
			ds.scan = s
		}
	}
	for s, ps := range steps {
		if len(ps.Push) > 0 || slices.ContainsFunc(ps.Builds, func(b plan.Build) bool { return b.Image != nil && (b.Image.Push || b.Image.OutputPush) }) {
			ds.pushes = true
			ds.early = ds.early || s <= ds.scan
		}
	}
	ds.after = ds.st.after != nil && len(ds.st.after.Pushes(classify, nil)) > 0
	ds.pushes = ds.pushes || ds.after || rawValue(m, "deployment") != nil || scalarValue(rawValue(m, "trigger")) == "manual"
	return ds, nil
}

// dailyItems returns the lines of the pipeline's items up to the last one with a Shearbolt scan,
// with their dashes at column dash; nil when no item has a scan. A step that only pushes or
// deploys is left out, and the step with the scan is cut after it when it pushes later.
func (pl *pipelines) dailyItems(seq *yaml.Node, dash int, classify plan.Classifier) ([]string, error) {
	d := pl.doc
	type item struct {
		steps  []*dailyStep
		single bool
		skip   bool
	}
	items := make([]item, len(seq.Content))
	last := -1
	for i, n := range seq.Content {
		it := &items[i]
		var members []*yaml.Node
		group := resolve(rawValue(n, "parallel"))
		if group != nil && group.Kind == yaml.MappingNode {
			group = resolve(rawValue(group, "steps"))
		}
		if st := rawValue(n, "stage"); st != nil && group == nil {
			group = resolve(rawValue(st, "steps"))
		}
		switch {
		case rawValue(n, "step") != nil:
			members, it.single = []*yaml.Node{rawValue(n, "step")}, true
		case group != nil && group.Kind == yaml.SequenceNode:
			for _, sub := range group.Content {
				if s := rawValue(sub, "step"); s != nil {
					members = append(members, s)
				}
			}
		case rawValue(n, "variables") != nil:
			it.skip = true
			continue
		default:
			return nil, fmt.Errorf("the item at line %d is not a step, a parallel group or a stage", n.Line)
		}
		for _, m := range members {
			ds, err := pl.dailyStep(resolve(m), classify)
			if err != nil {
				return nil, err
			}
			it.steps = append(it.steps, ds)
			if ds.scan >= 0 {
				last = i
			}
		}
	}
	if last < 0 {
		return nil, nil
	}
	from, err := d.DashColumn(seq)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := 0; i <= last; i++ {
		it, n := items[i], seq.Content[i]
		scans := slices.ContainsFunc(it.steps, func(s *dailyStep) bool { return s.scan >= 0 })
		pushes := slices.ContainsFunc(it.steps, func(s *dailyStep) bool { return s.pushes })
		switch {
		case it.skip || pushes && !scans && it.single:
			continue
		case pushes && !it.single:
			return nil, fmt.Errorf("the parallel group or stage at line %d holds a step that pushes or deploys", n.Line)
		case pushes:
			lines, err := pl.cutStep(it.steps[0], dash)
			if err != nil {
				return nil, err
			}
			out = append(out, lines...)
			continue
		}
		if anchored(n) {
			s := rawValue(n, "step")
			if !it.single || s.Anchor == "" || slices.ContainsFunc(s.Content, anchored) {
				return nil, fmt.Errorf("the item at line %d defines a YAML anchor, which a copy would define twice", n.Line)
			}
			out = append(out, strings.Repeat(" ", dash)+"- step: *"+s.Anchor)
			continue
		}
		first, err := d.ItemStart(seq, i)
		if err != nil {
			return nil, err
		}
		end, err := d.ItemEnd(seq, i)
		if err != nil {
			return nil, err
		}
		out = append(out, shifted(d, first, end, dash-from)...)
	}
	return out, nil
}

// cutStep writes the step without what pushes: its script ends with the scan, and its
// deployment, its manual trigger and an after-script that pushes are left out.
func (pl *pipelines) cutStep(ds *dailyStep, dash int) ([]string, error) {
	d, m := pl.doc, ds.st.m
	switch {
	case ds.early:
		return nil, fmt.Errorf("the step at line %d pushes or deploys before its scan", ds.st.line)
	case slices.ContainsFunc(m.Content, anchored):
		return nil, fmt.Errorf("the step at line %d defines a YAML anchor, which a copy would define twice", ds.st.line)
	}
	delta := dash + 4 - (m.Content[0].Column - 1)
	out := []string{strings.Repeat(" ", dash) + "- step:"}
	for k := 0; k+1 < len(m.Content); k += 2 {
		key := m.Content[k]
		switch {
		case key.Value == "deployment" || key.Value == "trigger" || key.Value == "after-script" && ds.after:
		case key.Value == "script":
			seq := ds.st.script.Seq
			first, err := d.ItemStart(seq, 0)
			if err != nil {
				return nil, err
			}
			end, err := d.ItemEnd(seq, ds.scan)
			if err != nil {
				return nil, err
			}
			out = append(out, shifted(d, key.Line, key.Line, delta)...)
			out = append(out, shifted(d, first, end, delta)...)
		default:
			end, err := d.EntryEnd(m, key.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, shifted(d, key.Line, end, delta)...)
		}
	}
	return out, nil
}

func anchored(n *yaml.Node) bool {
	return n != nil && (n.Anchor != "" || slices.ContainsFunc(n.Content, anchored))
}

// shifted returns lines first..last moved by delta columns.
func shifted(d *yamledit.Doc, first, last, delta int) []string {
	var out []string
	for l := first; l <= last; l++ {
		t := d.Line(l)
		ind := len(t) - len(strings.TrimLeft(t, " "))
		if strings.TrimSpace(t) == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Repeat(" ", max(ind+delta, 0))+t[ind:])
	}
	return out
}
