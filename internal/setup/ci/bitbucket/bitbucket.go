// Package bitbucket is the Bitbucket Pipelines plugin of shearbolt setup: it reads the steps of
// bitbucket-pipelines.yml into the CI-neutral job model, and edits their scripts (through
// scriptci) and caches.
package bitbucket

import (
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/scriptci"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

const (
	pipelinesFile = "bitbucket-pipelines.yml"
	cloneDir      = "$BITBUCKET_CLONE_DIR"
	cacheName     = "shearbolt"
	cachePath     = "~/.cache/shearbolt" // shearbolt's cache when SHEARBOLT_CACHE_DIR and XDG_CACHE_HOME are unset
	snippetIndent = 12                   // script items of a step in a branch pipeline
)

// sections are the pipelines whose steps are jobs: default is a list, the others map a name or
// pattern to a list.
var sections = []string{"default", "branches", "tags", "pull-requests", "custom"}

// Plugin is the Bitbucket Pipelines plugin.
type Plugin struct{}

// Name implements plan.CI.
func (Plugin) Name() string { return "bitbucket" }

// Files returns bitbucket-pipelines.yml when the repository has one.
func (Plugin) Files(r plan.Repo) []string {
	if _, err := r.ReadDir(pipelinesFile); err == nil || !r.Exists(pipelinesFile) {
		return nil
	}
	return []string{pipelinesFile}
}

// step is one editable step.
type step struct {
	path   string // "branches/main/0"
	id     string
	label  string
	name   string
	m      *yaml.Node // the step mapping
	line   int
	script *scriptci.Script
	after  *scriptci.Script // its after-script, nil when it has none that setup can read
}

// pipelines is a parsed bitbucket-pipelines.yml.
type pipelines struct {
	file    string
	doc     *yamledit.Doc
	steps   []*step
	skipped []string // steps that cannot be edited: "label (reason)"
	imports []string // pipelines imported from another repository (import:), which setup does not read
}

func resolve(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i < 100; i++ {
		n = n.Alias
	}
	return n
}

// rawValue returns the value node of key in mapping m as written (aliases not resolved).
func rawValue(m *yaml.Node, key string) *yaml.Node {
	m = resolve(m)
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func keys(m *yaml.Node) []string {
	var out []string
	if m = resolve(m); m != nil && m.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(m.Content); i += 2 {
			out = append(out, m.Content[i].Value)
		}
	}
	return out
}

func flow(n *yaml.Node) bool { return n != nil && n.Style&yaml.FlowStyle != 0 }

func scalarValue(n *yaml.Node) string {
	if n = resolve(n); n != nil && n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return ""
}

// parse reads the steps of every pipeline (R4 rule 6, Task 16). A step reached through an alias
// is one job, at the path where it is first found. Steps with a merge key, a shared or flow-style
// script, or script items that are not commands or pipes are left out and listed in skipped.
func parse(file string, src []byte) (*pipelines, error) {
	d, err := yamledit.Parse(src)
	if err != nil {
		return nil, err
	}
	pl := &pipelines{file: file, doc: d}
	root := d.Root()
	if root == nil || root.Kind != yaml.MappingNode {
		return pl, nil
	}
	top := resolve(rawValue(root, "pipelines"))
	if top == nil || top.Kind != yaml.MappingNode {
		return pl, nil
	}
	seen := map[*yaml.Node]bool{}
	for i := 0; i+1 < len(top.Content); i += 2 {
		section, v := top.Content[i].Value, top.Content[i+1]
		if !slices.Contains(sections, section) {
			continue
		}
		if section == "default" {
			pl.imported(v, "default")
			pl.list(v, "default", seen)
			continue
		}
		if m := resolve(v); m != nil && m.Kind == yaml.MappingNode {
			for k := 0; k+1 < len(m.Content); k += 2 {
				p := section + "/" + m.Content[k].Value
				pl.imported(m.Content[k+1], p)
				pl.list(m.Content[k+1], p, seen)
			}
		}
	}
	return pl, nil
}

// imported records a pipeline that is imported from elsewhere (import: repo:branch:name).
func (pl *pipelines) imported(n *yaml.Node, path string) {
	if rawValue(n, "import") != nil {
		pl.imports = append(pl.imports, path)
	}
}

// list reads a pipeline's items: steps, parallel groups (a list, or a mapping with steps) and
// stages (with steps).
func (pl *pipelines) list(n *yaml.Node, prefix string, seen map[*yaml.Node]bool) {
	seq := resolve(n)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return
	}
	for i, item := range seq.Content {
		p := fmt.Sprintf("%s/%d", prefix, i)
		if s := rawValue(item, "step"); s != nil {
			pl.add(s, p, seen)
		}
		group := resolve(rawValue(item, "parallel"))
		if group != nil && group.Kind == yaml.MappingNode {
			group = resolve(rawValue(group, "steps"))
		}
		if st := rawValue(item, "stage"); st != nil && group == nil {
			group = resolve(rawValue(st, "steps"))
		}
		if group != nil && group.Kind == yaml.SequenceNode {
			for j, sub := range group.Content {
				if s := rawValue(sub, "step"); s != nil {
					pl.add(s, fmt.Sprintf("%s/%d", p, j), seen)
				}
			}
		}
	}
}

// add reads one step.
func (pl *pipelines) add(n *yaml.Node, path string, seen map[*yaml.Node]bool) {
	m := resolve(n)
	if m == nil || m.Kind != yaml.MappingNode || seen[m] {
		return
	}
	seen[m] = true
	st := &step{path: path, id: pl.file + "#" + path, m: m, name: scalarValue(rawValue(m, "name")), line: m.Line}
	if len(m.Content) > 0 {
		st.line = m.Content[0].Line
	}
	st.label = path
	if st.name != "" {
		st.label = st.name + " (" + path + ")"
	}
	skip := func(reason string) { pl.skipped = append(pl.skipped, st.label+" ("+reason+")") }
	switch sn := rawValue(m, "script"); {
	case windows(rawValue(m, "runs-on")):
		skip("it runs on a Windows runner, where the script is not a POSIX shell")
	case slices.Contains(keys(m), "<<"):
		skip("it uses a YAML merge key")
	case flow(m):
		skip("flow-style step")
	case sn == nil:
		// Bitbucket requires a script; a step without one has nothing to edit.
	case sn.Kind == yaml.AliasNode || sn.Anchor != "":
		skip("its script is shared through a YAML anchor")
	default:
		s, err := scriptci.NewScript(pl.doc, pl.file, st.id, sn, shell.State{Dir: "."}, true)
		if err != nil {
			skip(err.Error())
			return
		}
		st.script = s
		if an := rawValue(m, "after-script"); an != nil {
			if an.Kind == yaml.AliasNode {
				skip("its after-script is a YAML alias, which setup cannot read")
				return
			}
			if st.after, err = scriptci.NewScript(pl.doc, pl.file, st.id, an, shell.State{Dir: "."}, true); err != nil {
				skip("its after-script cannot be read: " + err.Error())
				return
			}
		}
		pl.steps = append(pl.steps, st)
	}
}

// windows reports whether a runs-on value (a label or a list of labels) names a Windows runner.
func windows(n *yaml.Node) bool {
	n = resolve(n)
	if n == nil {
		return false
	}
	if n.Kind == yaml.ScalarNode {
		return strings.Contains(strings.ToLower(n.Value), "windows")
	}
	return slices.ContainsFunc(n.Content, windows)
}

func (pl *pipelines) step(id string) *step {
	for _, st := range pl.steps {
		if st.id == id {
			return st
		}
	}
	return nil
}

// Jobs reads the steps into editable jobs: each script item is a step, a pipe a step that pushes.
// Steps that cannot be edited are listed in one UNSUPPORTED_EDIT note. A push in an after-script
// gets PUSH_IN_AFTER_SCRIPT: after-script runs even when the script, and so the gate, fails.
func (p Plugin) Jobs(r plan.Repo, file string, src []byte, classify plan.Classifier) ([]plan.Job, []plan.Note, error) {
	pl, err := parse(file, src)
	if err != nil {
		return nil, nil, err
	}
	var jobs []plan.Job
	var notes []plan.Note
	for _, st := range pl.steps {
		jobs = append(jobs, plan.Job{ID: st.id, Label: st.label, Steps: st.script.Steps(classify, nil)})
		if st.after != nil {
			notes = append(notes, scriptci.AfterScriptNotes(st.after, "after-script", st.label, classify, nil)...)
		}
	}
	if len(pl.imports) > 0 {
		notes = append(notes, plan.Note{
			Code: "INCLUDE_NOT_READ",
			Message: fmt.Sprintf("%s imports pipelines from other repositories (import: in %s), which setup does not read; their steps may build or push without a gate. Add these items by hand to the script of each step that builds, after the build and before any push:\n%s",
				file, strings.Join(pl.imports, ", "), p.Snippet(r, file, plan.Gate{})),
			File: file,
		})
	}
	if len(pl.skipped) > 0 {
		notes = append(notes, plan.Note{
			Code: "UNSUPPORTED_EDIT",
			Message: fmt.Sprintf("%s: these steps cannot be edited automatically: %s. If one of them builds and pushes or deploys, add these items by hand to its script, after the build and before the push:\n%s",
				file, strings.Join(pl.skipped, ", "), p.Snippet(r, file, plan.Gate{})),
			File: file,
		})
	}
	return jobs, notes, nil
}

// ---- Snippet and Edit ----

// Snippet renders gate g as the two script items a user adds by hand.
func (Plugin) Snippet(r plan.Repo, file string, g plan.Gate) string {
	return scriptci.Snippet(g, cloneDir, snippetIndent)
}

// cacheSnippet is what a user adds by hand for the shearbolt cache.
const cacheSnippet = "definitions:\n  caches:\n    " + cacheName + ": " + cachePath + "\n...\n      - step:\n          caches:\n            - " + cacheName

// Edit applies the gates and preps of the file's jobs through scriptci, and gives every step
// with a gate the shearbolt cache: an entry in its caches list (a new list before script:) and
// one definitions.caches entry for the file (Task 16).
func (p Plugin) Edit(r plan.Repo, file string, src []byte, jobs []plan.Job, gates []plan.Gate, preps map[string][]plan.Prep) (plan.Edited, error) {
	pl, err := parse(file, src)
	if err != nil {
		return plan.Edited{}, err
	}
	c := &caches{pl: pl, unit: yamledit.IndentUnit(pl.doc), refs: map[string]bool{}}
	c.check()
	var js []scriptci.Job
	for _, pj := range jobs {
		j := scriptci.Job{Plan: pj, Key: pj.ID}
		if st := pl.step(pj.ID); st != nil {
			j.Script, j.Key, j.Line, j.After = st.script, st.label, st.line, st.after
			j.Extra = func() ([]plan.Note, error) { return c.step(st) }
		}
		js = append(js, j)
	}
	return scriptci.Edit(pl.doc, src, file, js, gates, preps, scriptci.Options{
		CloneDir: cloneDir,
		Snippet:  func(g plan.Gate) string { return p.Snippet(r, file, g) },
		Finish:   c.define,
	})
}

// caches edits the shearbolt cache of a file's steps.
type caches struct {
	pl     *pipelines
	unit   int
	reason string          // why definitions.caches cannot get the entry ("" when it can)
	exists bool            // definitions.caches already defines shearbolt
	refs   map[string]bool // job IDs whose step lists the shearbolt cache after its edit
}

// check decides whether definitions.caches can hold the shearbolt entry: definitions and its
// caches must be block mappings that are not shared through an alias.
func (c *caches) check() {
	root := c.pl.doc.Root()
	defs := rawValue(root, "definitions")
	switch {
	case root == nil || root.Kind != yaml.MappingNode:
		c.reason = "the file is not a mapping"
		return
	case defs == nil:
		if rawValue(root, "pipelines") == nil {
			c.reason = "the file has no pipelines"
		}
		return
	case defs.Kind == yaml.AliasNode || defs.Kind != yaml.MappingNode || flow(defs) || len(defs.Content) == 0:
		c.reason = "definitions is not a block mapping"
		return
	}
	cs := rawValue(defs, "caches")
	switch {
	case cs == nil:
	case cs.Kind == yaml.AliasNode || cs.Kind != yaml.MappingNode || flow(cs) || len(cs.Content) == 0:
		c.reason = "definitions.caches is not a block mapping"
	default:
		c.exists = slices.Contains(keys(cs), cacheName)
	}
}

func (c *caches) notAdded(st *step, why string) []plan.Note {
	return []plan.Note{{
		Code: "CACHE_NOT_ADDED",
		Message: fmt.Sprintf("the shearbolt cache was not added to %s, because %s; add it by hand so each run does not download the vulnerability database again:\n%s",
			st.label, why, cacheSnippet),
		File: c.pl.file,
		Line: st.line,
	}}
}

// step adds shearbolt to the caches of st, the step of a job that gets a gate. A step whose
// caches are flow style, or shared through an anchor, gets CACHE_NOT_ADDED instead.
func (c *caches) step(st *step) ([]plan.Note, error) {
	d := c.pl.doc
	if c.reason != "" {
		return c.notAdded(st, c.reason), nil
	}
	cs := rawValue(st.m, "caches")
	pad := strings.Repeat(" ", c.unit)
	switch {
	case cs == nil:
		ks := keys(st.m)
		i := slices.Index(ks, "script")
		if i < 0 || flow(st.m) {
			return c.notAdded(st, "the step has no block script: key to put caches before"), nil
		}
		var err error
		if i > 0 {
			err = d.SetKey(st.m, "caches", "\n"+pad+"- "+cacheName, ks[i-1])
		} else {
			k := st.m.Content[0]
			kc := strings.Repeat(" ", k.Column-1)
			err = d.InsertBefore(k.Line, []string{kc + "caches:", kc + pad + "- " + cacheName})
		}
		if err != nil {
			return nil, fmt.Errorf("the step's caches cannot be added (%v)", err)
		}
	case cs.Kind == yaml.AliasNode || cs.Anchor != "":
		return c.notAdded(st, "its caches are shared through a YAML anchor"), nil
	case cs.Kind != yaml.SequenceNode || flow(cs) || len(cs.Content) == 0:
		return c.notAdded(st, "its caches: is not a block list"), nil
	case !slices.ContainsFunc(cs.Content, func(n *yaml.Node) bool { return scalarValue(n) == cacheName }):
		if err := d.InsertItem(cs, len(cs.Content)-1, []string{cacheName}); err != nil {
			return nil, fmt.Errorf("the step's caches cannot be extended (%v)", err)
		}
	}
	c.refs[st.id] = true
	return nil, nil
}

// define adds the shearbolt entry to definitions.caches once a landed job's step lists it: a new
// caches mapping at the end of definitions, or new definitions before pipelines and its head
// comment.
func (c *caches) define(gated []string) error {
	if c.exists || c.reason != "" || !slices.ContainsFunc(gated, func(id string) bool { return c.refs[id] }) {
		return nil
	}
	d := c.pl.doc
	root := d.Root()
	pad := strings.Repeat(" ", c.unit)
	entry := cacheName + ": " + yamledit.Scalar(cachePath)
	defs := rawValue(root, "definitions")
	if defs == nil {
		var pk *yaml.Node
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "pipelines" {
				pk = root.Content[i]
			}
		}
		line := pk.Line
		for line > 1 && strings.HasPrefix(d.Line(line-1), "#") {
			line--
		}
		kc := strings.Repeat(" ", pk.Column-1)
		return d.InsertBefore(line, []string{kc + "definitions:", kc + pad + "caches:", kc + pad + pad + entry, ""})
	}
	if cs := rawValue(defs, "caches"); cs != nil {
		return d.SetKey(cs, cacheName, yamledit.Scalar(cachePath), "")
	}
	return d.SetKey(defs, "caches", "\n"+pad+entry, "")
}
