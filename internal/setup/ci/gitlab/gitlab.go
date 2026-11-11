// Package gitlab is the GitLab CI plugin of shearbolt setup: it reads the jobs of .gitlab-ci.yml
// into the CI-neutral job model, and edits their scripts (through scriptci), variables and caches.
package gitlab

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/scriptci"
	"github.com/klar-web-services/shearbolt/internal/setup/ci/shelledit"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

const (
	ciFile        = ".gitlab-ci.yml"
	cloneDir      = "$CI_PROJECT_DIR"
	cacheVar      = "SHEARBOLT_CACHE_DIR"
	cacheDir      = "$CI_PROJECT_DIR/.shearbolt/cache" // GitLab caches only paths inside the project
	cacheKey      = "shearbolt"
	cachePath     = ".shearbolt/cache"
	maxCaches     = 4 // GitLab's limit of caches per job
	snippetIndent = 4
)

// keywords are the top-level keys that are not jobs.
var keywords = []string{"stages", "variables", "default", "include", "workflow", "image", "services", "cache", "before_script", "after_script"}

// Plugin is the GitLab CI plugin.
type Plugin struct{}

// Name implements plan.CI.
func (Plugin) Name() string { return "gitlab" }

// Files returns .gitlab-ci.yml when the repository has one.
func (Plugin) Files(r plan.Repo) []string {
	if _, err := r.ReadDir(ciFile); err == nil || !r.Exists(ciFile) {
		return nil
	}
	return []string{ciFile}
}

// job is one editable job.
type job struct {
	key, id string
	m       *yaml.Node // the job mapping
	line    int
	extends bool // the job extends a template, whose keys setup does not read
	inherit bool // the job has inherit:, which setup does not evaluate
	env     []string
	script  *scriptci.Script
	before  *scriptci.Script // the before_script it runs (its own, default's or the top-level one), nil when none or unknown
	// unseen: the job has extends or inherit: and no before_script of its own, so it may run
	// any before_script of the file.
	unseen bool
	after  *scriptci.Script // the after_script it runs (its own, default's or the top-level one), when readable
}

// config is a parsed .gitlab-ci.yml.
type config struct {
	file    string
	doc     *yamledit.Doc
	spec    bool // the file starts with a spec: header, so it is not read further
	jobs    []*job
	skipped []string           // jobs that cannot be edited: "key (reason)"
	cache   bool               // a top-level cache: or default.cache, which jobs without their own inherit
	include bool               // the file includes other files (include:), which setup does not read
	befores []*scriptci.Script // every before_script of the file setup can read
	opaque  []string           // where a before_script setup cannot read is: "before_script", ".base"
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

// hasAnchor reports whether n or anything inside it defines an anchor.
func hasAnchor(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	return n.Anchor != "" || slices.ContainsFunc(n.Content, hasAnchor)
}

// specHeader reports whether src is a header document with spec: followed by the content
// document (GitLab CI/CD inputs).
func specHeader(src []byte) bool {
	var first yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(src)).Decode(&first); err != nil || len(first.Content) == 0 {
		return false
	}
	return slices.Contains(keys(first.Content[0]), "spec")
}

// parse reads the jobs of a .gitlab-ci.yml (R4 rule 6, Task 17). A job needs its own block
// script sequence of plain strings; jobs whose script comes from extends, holds !reference or is
// one string, and jobs with a merge key or YAML anchors, are left out and listed in skipped.
// extends, !reference, include and merges are never resolved.
func parse(file string, src []byte) (*config, error) {
	c := &config{file: file}
	d, err := yamledit.Parse(src)
	if errors.Is(err, yamledit.ErrMultiDoc) && specHeader(src) {
		c.spec = true
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	c.doc = d
	root := d.Root()
	if root == nil || root.Kind != yaml.MappingNode {
		return c, nil
	}
	def := rawValue(root, "default")
	c.cache = rawValue(root, "cache") != nil || rawValue(def, "cache") != nil
	c.include = rawValue(root, "include") != nil
	c.befores = c.allBeforeScripts(root)
	errexitOff := c.errexitOffAnywhere()
	global := keys(rawValue(root, "variables"))
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, v := root.Content[i].Value, root.Content[i+1]
		if slices.Contains(keywords, key) || strings.HasPrefix(key, ".") {
			continue
		}
		skip := func(reason string) { c.skipped = append(c.skipped, key+" ("+reason+")") }
		m := resolve(v)
		if m == nil || m.Kind != yaml.MappingNode {
			continue
		}
		j := &job{key: key, id: file + "#" + key, m: m, line: root.Content[i].Line}
		ks := keys(m)
		j.extends, j.inherit = slices.Contains(ks, "extends"), slices.Contains(ks, "inherit")
		sn := rawValue(m, "script")
		switch {
		case v.Kind == yaml.AliasNode || hasAnchor(m):
			skip("it uses a YAML anchor or alias")
			continue
		case slices.Contains(ks, "<<"):
			skip("it uses a YAML merge key")
			continue
		case flow(m):
			skip("flow-style job")
			continue
		case sn == nil && j.extends:
			skip("its script comes from extends")
			continue
		case sn == nil:
			continue // a trigger job, or no script at all: nothing to edit
		case sn.Kind == yaml.AliasNode:
			skip("its script is a YAML alias")
			continue
		case sn.Kind == yaml.ScalarNode:
			skip("its script is one string, not a list")
			continue
		case sn.Tag == "!reference" || slices.ContainsFunc(sn.Content, func(n *yaml.Node) bool { return n.Tag == "!reference" }):
			skip("its script uses !reference")
			continue
		}
		j.env = append(slices.Clone(global), keys(rawValue(m, "variables"))...)
		j.unseen = (j.extends || j.inherit) && rawValue(m, "before_script") == nil
		if j.unseen && len(c.opaque) > 0 {
			skip("it may take its before_script from extends or inherit:, and setup cannot read the before_script of " + strings.Join(c.opaque, ", "))
			continue
		}
		if j.unseen && errexitOff != "" {
			skip("it may take its before_script from extends or inherit:, and " + errexitOff + " in a before_script of this file turns errexit off (set +e)")
			continue
		}
		start, before, err := c.startState(j, root, def)
		if err != nil {
			skip("its before_script cannot be read: " + err.Error())
			continue
		}
		s, err := scriptci.NewScript(d, file, j.id, sn, start, false)
		if err != nil {
			skip(err.Error())
			continue
		}
		if before != nil {
			for _, it := range before.Items {
				s.Prelude = append(s.Prelude, it.Cmds...)
			}
		}
		j.before = before
		j.script = s
		an := rawValue(m, "after_script")
		if an == nil {
			if an = rawValue(def, "after_script"); an == nil {
				an = rawValue(root, "after_script")
			}
		}
		if an != nil {
			if an.Kind == yaml.AliasNode || an.Tag == "!reference" {
				skip("its after_script is a YAML alias or !reference, which setup cannot read")
				continue
			}
			if j.after, err = scriptci.NewScript(d, file, j.id, an, shell.State{Dir: "."}, false); err != nil {
				skip("its after_script cannot be read: " + err.Error())
				continue
			}
		}
		c.jobs = append(c.jobs, j)
	}
	return c, nil
}

// startState returns the shell state the job's script starts in and the before_script it runs
// (its own, default's, or the top-level one), which shares the script's shell. A job with extends
// or inherit: and no own before_script may run one setup cannot see: its directory is unknown and
// before is nil. A before_script setup cannot read is an error.
func (c *config) startState(j *job, root, def *yaml.Node) (shell.State, *scriptci.Script, error) {
	bn := rawValue(j.m, "before_script")
	if bn == nil {
		if j.extends || j.inherit {
			return shell.State{}, nil, nil
		}
		if bn = rawValue(def, "before_script"); bn == nil {
			bn = rawValue(root, "before_script")
		}
	}
	if bn == nil {
		return shell.State{Dir: "."}, nil, nil
	}
	if bn.Kind == yaml.AliasNode || bn.Tag == "!reference" {
		return shell.State{}, nil, fmt.Errorf("it is a YAML alias or !reference")
	}
	bs, err := scriptci.NewScript(c.doc, c.file, j.id, bn, shell.State{Dir: "."}, false)
	if err != nil {
		return shell.State{}, nil, err
	}
	var texts []string
	for _, it := range bs.Items {
		texts = append(texts, it.Text)
	}
	_, end := scriptci.ParseItems(texts, shell.State{Dir: "."})
	return end, bs, nil
}

// allBeforeScripts returns every before_script of the file setup can read: the top-level one,
// default's, and those of every job and template. It records in c.opaque where one cannot be
// read (an alias of something else, !reference, an item that is not a plain string).
func (c *config) allBeforeScripts(root *yaml.Node) []*scriptci.Script {
	type src struct {
		where string
		n     *yaml.Node
	}
	srcs := []src{{"the top level", rawValue(root, "before_script")}}
	for i := 0; i+1 < len(root.Content); i += 2 {
		srcs = append(srcs, src{root.Content[i].Value, rawValue(root.Content[i+1], "before_script")})
	}
	var out []*scriptci.Script
	for _, sc := range srcs {
		n := resolve(sc.n)
		if n == nil {
			continue
		}
		bs, err := scriptci.NewScript(c.doc, c.file, "", n, shell.State{Dir: "."}, false)
		if err != nil || n.Tag == "!reference" {
			c.opaque = append(c.opaque, sc.where)
			continue
		}
		out = append(out, bs)
	}
	return out
}

// errexitOffAnywhere returns the command that turns errexit off in any before_script of the
// file, or "".
func (c *config) errexitOffAnywhere() string {
	for _, bs := range c.befores {
		var cmds []shell.Command
		for _, it := range bs.Items {
			cmds = append(cmds, it.Cmds...)
		}
		if off := shelledit.ErrexitOff(cmds); off != nil {
			return shelledit.CommandText(*off)
		}
	}
	return ""
}

func (c *config) job(id string) *job {
	for _, j := range c.jobs {
		if j.id == id {
			return j
		}
	}
	return nil
}

// Jobs reads the jobs: each script item is a step, and each command sees the top-level and job
// variables. Jobs that cannot be edited are listed in one UNSUPPORTED_EDIT note, and so is a file
// with a spec: header. A push in the after_script a job runs gets PUSH_IN_AFTER_SCRIPT.
func (p Plugin) Jobs(r plan.Repo, file string, src []byte, classify plan.Classifier) ([]plan.Job, []plan.Note, error) {
	c, err := parse(file, src)
	if err != nil {
		return nil, nil, err
	}
	if c.spec {
		return nil, []plan.Note{{
			Code: "UNSUPPORTED_EDIT",
			Message: fmt.Sprintf("%s starts with a spec: header (CI/CD inputs), which setup does not edit. Add these items by hand to the script of the job that builds, after the build and before any push or deploy:\n%s",
				file, p.Snippet(r, file, plan.Gate{})),
			File: file,
		}}, nil
	}
	var jobs []plan.Job
	var notes []plan.Note
	for _, j := range c.jobs {
		// The before_script it runs, or every one of the file when it may take one from extends
		// or inherit: (Batch 5 re-review).
		befores, whose := []*scriptci.Script{j.before}, "its before_script"
		if j.unseen {
			befores, whose = c.befores, "a before_script of this file it may run through extends or inherit:"
		}
		var builds, pushes, stops []scriptci.Found
		for _, bs := range befores {
			if bs != nil {
				builds = append(builds, bs.ArtifactBuilds(classify, j.env)...)
				pushes = append(pushes, bs.Pushes(classify, j.env)...)
				stops = append(stops, bs.Stops(classify, j.env)...)
			}
		}
		if len(builds) > 0 {
			c.skipped = append(c.skipped, fmt.Sprintf("%s (%s builds an image or binary, %s, which no gate can follow)", j.key, whose, builds[0].What))
			continue
		}
		for _, f := range pushes {
			notes = append(notes, plan.Note{
				Code:    "PUSH_BEFORE_GATE",
				Message: fmt.Sprintf("job %s pushes or deploys in %s (%s), which runs before the script and so before the Shearbolt gate; move it into the script, after the gate", j.key, whose, f.What),
				File:    file,
				Line:    f.Line,
			})
		}
		pj := plan.Job{ID: j.id, Label: path.Base(file) + ": " + j.key, Steps: j.script.Steps(classify, j.env)}
		if len(stops) > 0 {
			pj.PreludeStop, pj.PreludeStopCmd = stops[0].Line, stops[0].Name
		}
		jobs = append(jobs, pj)
		if j.after != nil {
			notes = append(notes, scriptci.AfterScriptNotes(j.after, "after_script", "job "+j.key, classify, j.env)...)
		}
	}
	if c.include {
		notes = append(notes, plan.Note{
			Code: "INCLUDE_NOT_READ",
			Message: fmt.Sprintf("%s includes other pipeline files (include:), which setup does not read; jobs there may build or push without a gate. Add these items by hand to the script of each that builds, after the build and before any push:\n%s",
				file, p.Snippet(r, file, plan.Gate{})),
			File: file,
		})
	}
	if len(c.skipped) > 0 {
		notes = append(notes, plan.Note{
			Code: "UNSUPPORTED_EDIT",
			Message: fmt.Sprintf("%s: these jobs cannot be edited automatically: %s. If one of them builds and pushes or deploys, add these items by hand to its script, after the build and before the push:\n%s",
				file, strings.Join(c.skipped, ", "), p.Snippet(r, file, plan.Gate{})),
			File: file,
		})
	}
	return jobs, notes, nil
}

// Snippet renders gate g as the two script items a user adds by hand.
func (Plugin) Snippet(r plan.Repo, file string, g plan.Gate) string {
	return scriptci.Snippet(g, cloneDir, snippetIndent)
}

// Edit applies the gates and preps of the file's jobs through scriptci, and gives every job with
// a gate the cache variable and the shearbolt cache (Task 17).
func (p Plugin) Edit(r plan.Repo, file string, src []byte, jobs []plan.Job, gates []plan.Gate, preps map[string][]plan.Prep) (plan.Edited, error) {
	c, err := parse(file, src)
	if err != nil {
		return plan.Edited{}, err
	}
	if c.spec {
		return plan.Edited{Src: src}, nil
	}
	unit := yamledit.IndentUnit(c.doc)
	var js []scriptci.Job
	for _, pj := range jobs {
		sj := scriptci.Job{Plan: pj, Key: pj.ID}
		if j := c.job(pj.ID); j != nil {
			sj.Script, sj.Key, sj.Line, sj.After = j.script, j.key, j.line, j.after
			sj.Extra = func() ([]plan.Note, error) { return c.cacheEdits(j, unit) }
		}
		js = append(js, sj)
	}
	return scriptci.Edit(c.doc, src, file, js, gates, preps, scriptci.Options{
		CloneDir: cloneDir,
		Snippet:  func(g plan.Gate) string { return p.Snippet(r, file, g) },
	})
}

// cache lines, relative to the job's key column.
func cacheItem(pad string) []string {
	return []string{"key: " + cacheKey, "paths:", pad + "- " + cachePath}
}

func (c *config) notAdded(j *job, why string, withVar bool) []plan.Note {
	snippet := "  cache:\n    - key: " + cacheKey + "\n      paths:\n        - " + cachePath
	if withVar {
		snippet = "  variables:\n    " + cacheVar + ": " + cacheDir + "\n" + snippet
	}
	return []plan.Note{{
		Code: "CACHE_NOT_ADDED",
		Message: fmt.Sprintf("the shearbolt cache was not added to job %s, because %s; add it by hand so each run does not download the vulnerability database again:\n%s",
			j.key, why, snippet),
		File: c.file,
		Line: j.line,
	}}
}

// cacheEdits gives job j, which gets a gate, variables.SHEARBOLT_CACHE_DIR (a new variables:
// goes before the first of before_script and script) and a shearbolt entry in its cache list (a
// mapping becomes a one-item list first; with no cache, a new list goes after variables:). A
// cache inherited from default: or the top level, GitLab's limit of 4 caches, a flow-style or
// shared cache, and a job whose template may hold a cache give CACHE_NOT_ADDED instead.
func (c *config) cacheEdits(j *job, unit int) ([]plan.Note, error) {
	d := c.doc
	pad := strings.Repeat(" ", unit)
	ks := keys(j.m)
	first := slices.IndexFunc(ks, func(k string) bool { return k == "before_script" || k == "script" })
	if first < 0 {
		return c.notAdded(j, "the job has no script: key", true), nil
	}
	// insertAt queues a new key before the first of before_script and script.
	insertAt := func(key, value string) error {
		if first > 0 {
			return d.SetKey(j.m, key, value, ks[first-1])
		}
		k := j.m.Content[0]
		kc := k.Column - 1
		var out []string
		for i, l := range strings.Split(key+":"+value, "\n") {
			if i > 0 && l == "" {
				out = append(out, "")
				continue
			}
			out = append(out, strings.Repeat(" ", kc)+l)
		}
		return d.InsertBefore(k.Line, out)
	}
	entry := cacheVar + ": " + yamledit.Scalar(cacheDir)
	vars := rawValue(j.m, "variables")
	// setVar queues the variable. It runs after an edit at the end of an existing cache, which
	// may be where a new variables: goes too, and before a new cache, which follows variables.
	setVar := func() error {
		switch {
		case vars == nil:
			if err := insertAt("variables", "\n"+pad+entry); err != nil {
				return fmt.Errorf("its variables cannot be added (%v)", err)
			}
		case !slices.Contains(keys(vars), cacheVar):
			if err := d.SetKey(vars, cacheVar, yamledit.Scalar(cacheDir), ""); err != nil {
				return fmt.Errorf("its variables cannot be extended (%v)", err)
			}
		}
		return nil
	}
	if vars != nil && (vars.Kind == yaml.AliasNode || hasAnchor(vars) || flow(vars) || vars.Kind != yaml.MappingNode || len(vars.Content) == 0) {
		return c.notAdded(j, "its variables: is not a block mapping setup can extend", true), nil
	}

	cn := rawValue(j.m, "cache")
	item := cacheItem(pad)
	var notes []plan.Note
	switch {
	case cn == nil && (c.cache || j.extends || j.inherit):
		notes = c.notAdded(j, "it inherits a cache (from default:, the top level or its template), which a job cache would replace", false)
	case cn == nil:
		if err := setVar(); err != nil {
			return nil, err
		}
		value := "\n" + pad + "- " + item[0]
		for _, l := range item[1:] {
			value += "\n" + pad + "  " + l
		}
		var err error
		if vars != nil {
			err = d.SetKey(j.m, "cache", value, "variables")
		} else {
			err = insertAt("cache", value)
		}
		if err != nil {
			return nil, fmt.Errorf("its cache cannot be added (%v)", err)
		}
		return nil, nil
	case cn.Kind == yaml.AliasNode || hasAnchor(cn) || flow(cn):
		notes = c.notAdded(j, "its cache: is flow style or shared through a YAML anchor", false)
	case cn.Kind == yaml.SequenceNode && len(cn.Content) > 0:
		switch {
		case slices.ContainsFunc(cn.Content, func(n *yaml.Node) bool { return scalarAt(n, "key") == cacheKey }):
		case len(cn.Content) >= maxCaches:
			notes = c.notAdded(j, fmt.Sprintf("it already has %d caches, GitLab's limit", maxCaches), false)
		default:
			if err := d.InsertItem(cn, len(cn.Content)-1, item); err != nil {
				return nil, fmt.Errorf("its cache cannot be extended (%v)", err)
			}
		}
	case cn.Kind == yaml.MappingNode && len(cn.Content) > 0 && !j.extends:
		if scalarAt(cn, "key") != cacheKey {
			if err := c.listify(j, cn, item); err != nil {
				return nil, fmt.Errorf("its cache cannot be turned into a list (%v)", err)
			}
		}
	default:
		notes = c.notAdded(j, "its cache: is not a block list or mapping setup can extend", false)
	}
	return notes, setVar()
}

func scalarAt(m *yaml.Node, key string) string {
	if v := resolve(rawValue(m, key)); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// listify turns job j's cache mapping cn into a one-item list (a dash before its first key, the
// rest of the block indented by 2) and appends item.
func (c *config) listify(j *job, cn *yaml.Node, item []string) error {
	d := c.doc
	k0 := cn.Content[0]
	kc := k0.Column - 1
	firstLine := d.Line(k0.Line)
	if strings.TrimSpace(firstLine[:min(kc, len(firstLine))]) != "" {
		return fmt.Errorf("its first key does not start its line")
	}
	last, err := d.EntryEnd(j.m, "cache")
	if err != nil {
		return err
	}
	var out []string
	for l := k0.Line; l <= last; l++ {
		t := d.Line(l)
		switch {
		case l == k0.Line:
			out = append(out, t[:kc]+"- "+t[kc:])
		case strings.TrimSpace(t) == "":
			out = append(out, t)
		default:
			out = append(out, "  "+t)
		}
	}
	pad := strings.Repeat(" ", kc)
	out = append(out, pad+"- "+item[0])
	for _, l := range item[1:] {
		out = append(out, pad+"  "+l)
	}
	return d.ReplaceLines(k0.Line, last, out)
}
