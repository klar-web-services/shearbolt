// Package scriptci is the part of setup's Bitbucket Pipelines and GitLab CI plugins that works on
// a job's script: a block sequence of shell items. It parses the items into plan steps and edits
// them (R4 rule 6, R5, R8).
//
// The script item is the unit. Every item is one plan.Step (a Bitbucket pipe is a step without
// commands), and all items run in one shell, so cd and exported variables carry from one item to
// the next. A gate is two items, the install and the scan, after the gated build's item. An item
// in which another command follows the gated build is split there into separate items (P1); the
// rest goes after the gate. A --push build loads its image instead, and `docker push <tag>` items
// after the gate push its original tags. Each job is edited completely or not at all.
package scriptci

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/shelledit"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

const (
	installURL = "https://tools.klar.ws/shearbolt/install.sh"
	program    = `"$HOME/.local/bin/shearbolt"`
)

// Item is one script item.
type Item struct {
	Node   *yaml.Node      // the item as written in the sequence (an alias when it is one)
	Value  *yaml.Node      // resolved: the command scalar, or the pipe mapping
	Text   string          // the command text; "" for a pipe
	Pipe   string          // the pipe a Bitbucket pipe item runs; "" for a command
	Cmds   []shell.Command // the parsed commands, with offsets in Text
	Before shell.State     // the shell state before the item
}

// Script is one job's script.
type Script struct {
	Doc   *yamledit.Doc
	File  string
	Job   string     // the job ID
	Seq   *yaml.Node // the block sequence of items
	Items []Item
	// Start is the shell state before the first item (Dir "" when it is unknown).
	Start shell.State
	// Prelude are the commands that run in the job's shell before the first item (GitLab's
	// before_script): a set +e there fails the job.
	Prelude []shell.Command
}

// ParseItems parses item texts in order, each starting in the previous one's end state, since
// the items of a job share one shell. It returns each item's commands and the final state.
func ParseItems(items []string, start shell.State) ([][]shell.Command, shell.State) {
	out := make([][]shell.Command, len(items))
	st := start
	for i, text := range items {
		out[i], st = shell.Parse(text, st)
	}
	return out, st
}

// NewScript reads the items of seq, a block sequence. Plain strings are commands; with pipes, a
// mapping with a pipe key is a Bitbucket pipe. Anything else (a flow-style or empty sequence, a
// tagged item such as !reference, a number) is an error: the job cannot be edited.
func NewScript(d *yamledit.Doc, file, job string, seq *yaml.Node, start shell.State, pipes bool) (*Script, error) {
	switch {
	case seq == nil || seq.Kind != yaml.SequenceNode:
		return nil, fmt.Errorf("the script is not a list")
	case seq.Style&yaml.FlowStyle != 0:
		return nil, fmt.Errorf("the script is a flow-style list")
	case len(seq.Content) == 0:
		return nil, fmt.Errorf("the script is empty")
	}
	s := &Script{Doc: d, File: file, Job: job, Seq: seq, Start: start}
	st := start
	for i, n := range seq.Content {
		it := Item{Node: n, Value: resolve(n), Before: st}
		v := it.Value
		switch {
		case v != nil && v.Kind == yaml.ScalarNode && v.Tag == "!!str":
			it.Text = v.Value
			it.Cmds, st = shell.Parse(it.Text, st)
		case pipes && v != nil && v.Kind == yaml.MappingNode && pipeName(d, v) != "":
			it.Pipe = pipeName(d, v)
		default:
			return nil, fmt.Errorf("script item %d is not a plain string", i+1)
		}
		s.Items = append(s.Items, it)
	}
	return s, nil
}

func pipeName(d *yamledit.Doc, m *yaml.Node) string {
	if p := d.Get(m, "pipe"); p != nil && p.Kind == yaml.ScalarNode {
		return p.Value
	}
	return ""
}

func resolve(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i < 100; i++ {
		n = n.Alias
	}
	return n
}

// line returns the file line where command c of the item starts.
func (it *Item) line(c shell.Command) int {
	n := it.Value
	if it.Node.Kind == yaml.AliasNode {
		return it.Node.Line
	}
	nl := strings.Count(it.Text[:min(c.Start, len(it.Text))], "\n")
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return n.Line + 1 + nl
	}
	return n.Line + nl
}

// itemLine returns the line where the item starts.
func (it *Item) itemLine() int {
	if it.Node.Kind == yaml.AliasNode || it.Value == nil {
		return it.Node.Line
	}
	return it.Value.Line
}

// Steps classifies the script into plan steps, one per item (R2, R4 rule 6). env are the CI's
// YAML variables for the job; each command also sees the variables set before it and its own
// VAR=x prefix. A pipe is a step that pushes.
func (s *Script) Steps(classify plan.Classifier, env []string) []plan.Step {
	var steps []plan.Step
	for i := range s.Items {
		it := &s.Items[i]
		st := plan.Step{Dir: it.Before.Dir}
		if it.Pipe != "" {
			st.Name, st.Uses, st.Push = it.Pipe, it.Pipe, []int{-1}
			steps = append(steps, st)
			continue
		}
		st.Commands = it.Cmds
		for c, cmd := range it.Cmds {
			pos := plan.Pos{File: s.File, Job: s.Job, Step: i, Command: c, Line: it.line(cmd)}
			vars := map[string]bool{}
			for _, list := range [][]string{env, cmd.Before.Env, cmd.Prefix()} {
				for _, n := range list {
					vars[n] = true
				}
			}
			cl := classify(cmd, plan.Context{Dir: cmd.Before.Dir, Env: vars})
			for _, b := range cl.Builds {
				b.Pos = pos
				st.Builds = append(st.Builds, b)
			}
			if cl.Push {
				st.Push = append(st.Push, c)
			}
			if cl.Stops {
				st.Stops = append(st.Stops, c)
			}
			if cl.Gate != nil {
				g := *cl.Gate
				g.Pos = pos
				st.Gates = append(st.Gates, g)
			}
		}
		// The sh an install item pipes into belongs to the install gate, so a re-run does not
		// list it as a command that runs before the gate (Batch 5 review).
		if last := len(it.Cmds) - 1; installOnly(*it, st) && !slices.ContainsFunc(st.Gates, func(g plan.GateCmd) bool { return g.Pos.Command == last }) {
			st.Gates = append(st.Gates, plan.GateCmd{Install: true, Pos: plan.Pos{File: s.File, Job: s.Job, Step: i, Command: last, Line: it.line(it.Cmds[last])}})
		}
		steps = append(steps, st)
	}
	return steps
}

// Found is a command or pipe of a script that setup reports.
type Found struct {
	What string // the command as in messages, or "the pipe <name>"
	Line int
	Name string // Stops only: the command's name, "exit", "exec" or "source" (also for .)
}

// Pushes returns the pushes in s: commands that push, image builds that push, and pipes.
func (s *Script) Pushes(classify plan.Classifier, env []string) []Found {
	var out []Found
	for i, st := range s.Steps(classify, env) {
		it := &s.Items[i]
		pushes := slices.Clone(st.Push)
		for _, b := range st.Builds {
			if b.Image != nil && (b.Image.Push || b.Image.OutputPush) {
				pushes = append(pushes, b.Pos.Command)
			}
		}
		slices.Sort(pushes)
		for _, c := range slices.Compact(pushes) {
			out = append(out, it.found(c))
		}
	}
	return out
}

// Stops returns the commands in s that can stop the job's shell early (design §3.2, §3.5), with
// their names.
func (s *Script) Stops(classify plan.Classifier, env []string) []Found {
	var out []Found
	for i, st := range s.Steps(classify, env) {
		it := &s.Items[i]
		for _, c := range st.Stops {
			f := it.found(c)
			f.Name = shelledit.StopName(it.Cmds[c])
			out = append(out, f)
		}
	}
	return out
}

// ArtifactBuilds returns the image and binary builds in s (in called files too).
func (s *Script) ArtifactBuilds(classify plan.Classifier, env []string) []Found {
	var out []Found
	for i, st := range s.Steps(classify, env) {
		for _, b := range st.Builds {
			if b.Kind == plan.BuildImage || b.Kind == plan.BuildBinary {
				out = append(out, s.Items[i].found(b.Pos.Command))
			}
		}
	}
	return out
}

// found describes command c of the item (-1: the pipe).
func (it *Item) found(c int) Found {
	if c < 0 || c >= len(it.Cmds) {
		return Found{What: "the pipe " + it.Pipe, Line: it.itemLine()}
	}
	return Found{What: shelledit.CommandText(it.Cmds[c]), Line: it.line(it.Cmds[c])}
}

// AfterScriptNotes returns PUSH_IN_AFTER_SCRIPT for each push in after, a job's after-script
// (key is its YAML key). It runs even when the script fails, so the gate does not stop it; setup
// reports it and does not edit it.
func AfterScriptNotes(after *Script, key, job string, classify plan.Classifier, env []string) []plan.Note {
	var out []plan.Note
	for _, f := range after.Pushes(classify, env) {
		out = append(out, plan.Note{
			Code:    "PUSH_IN_AFTER_SCRIPT",
			Message: fmt.Sprintf("%s pushes or deploys in its %s (%s), which runs even when the script fails, so the Shearbolt gate does not stop it; move it into the script, after the gate", job, key, f.What),
			File:    after.File,
			Line:    f.Line,
		})
	}
	return out
}

// ---- gate items ----

// pinned reports whether an install pins version: every version except a development build.
func pinned(version string) bool { return version != "" && version != "dev" }

// InstallLine is the item that installs shearbolt: version pinned, or the latest release for a
// development build (R9).
func InstallLine(version string) string {
	l := "(curl -fsSL " + installURL + " || wget -qO- " + installURL + ") | "
	if pinned(version) {
		l += "SHEARBOLT_VERSION=" + shellWord(version) + " "
	}
	return l + "sh"
}

// ScanLine is the item that runs gate g (R5, Task 15): its image variables as a prefix, then
// `"$HOME/.local/bin/shearbolt" scan --ci --config "<clone dir>/<dir>/shearbolt.yaml"` and a
// --target for each target the gate lists. The config is always named by the clone directory
// (Batch 5 review): a sourced file, eval or an included before_script can change the directory.
func ScanLine(g plan.Gate, cloneDir string) string {
	var w []string
	for _, iv := range g.Images {
		w = append(w, iv.Var+"="+iv.ScanTag)
	}
	w = append(w, program, "scan", "--ci")
	return strings.Join(append(w, managedArgs(g, cloneDir)[1:]...), " ")
}

// managedArgs are the scan arguments setup manages: --ci, --config and the --target list.
func managedArgs(g plan.Gate, cloneDir string) []string {
	out := []string{"--ci", "--config", configWord(g, cloneDir)}
	for _, t := range g.Targets {
		out = append(out, "--target", shellWord(t))
	}
	return out
}

// configWord is the gate's config path by the clone directory, double-quoted.
func configWord(g plan.Gate, cloneDir string) string {
	cfg := g.Config
	if cfg == "" {
		cfg = path.Join(dirOf(g), "shearbolt.yaml")
	}
	var b strings.Builder
	for _, c := range cfg {
		if strings.ContainsRune("\\\"$`", c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return `"` + cloneDir + "/" + b.String() + `"`
}

func dirOf(g plan.Gate) string {
	if g.Project.Dir == "" {
		return "."
	}
	return g.Project.Dir
}

// shellWord returns s as one shell word: as it is when it holds only safe characters, otherwise
// single-quoted.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._+-/:@=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Snippet renders gate g as the two script items a user adds by hand, each indent spaces in. The
// image variables carry the scan tags the images must also be tagged with.
func Snippet(g plan.Gate, cloneDir string, indent int) string {
	pad := strings.Repeat(" ", indent) + "- "
	return pad + yamledit.Scalar(InstallLine(g.Version)) + "\n" + pad + yamledit.Scalar(ScanLine(g, cloneDir))
}

// ---- editing ----

// Job is one job for Edit.
type Job struct {
	Plan   plan.Job
	Script *Script
	Key    string  // the job's name in messages
	Line   int     // where the job starts, for notes
	After  *Script // the job's after-script, when it has one setup can read

	// Extra queues the plugin's own edits of a job that has a gate (its caches) and returns notes
	// about them. It runs after the script edits, inside the job's all-or-nothing edit.
	Extra func() ([]plan.Note, error)
}

// Options are what Edit needs from the plugin.
type Options struct {
	CloneDir string                   // $BITBUCKET_CLONE_DIR or $CI_PROJECT_DIR
	Snippet  func(g plan.Gate) string // the plugin's snippet, for UNSUPPORTED_EDIT
	// Finish queues file-level edits once the jobs are edited, given the IDs of the jobs that
	// were edited with a gate (Bitbucket's cache definition). An error fails the whole file.
	Finish func(gated []string) error
}

// Edit applies gates and preps (keyed by job ID) to jobs, whose scripts are in d (R4 rule 6, R5,
// R8). Each job's edits are queued, the result is checked, and a job whose edit fails is rolled
// back completely, listed in Failed and given UNSUPPORTED_EDIT with the snippet of its gates.
func Edit(d *yamledit.Doc, src []byte, file string, jobs []Job, gates []plan.Gate, preps map[string][]plan.Prep, o Options) (plan.Edited, error) {
	out := plan.Edited{Src: src}
	var tags []string
	var gated []string
	attest, unpinned := false, false
	for _, j := range jobs {
		var jg []plan.Gate
		for _, g := range gates {
			if g.Job == j.Plan.ID {
				jg = append(jg, g)
			}
		}
		jp := preps[j.Plan.ID]
		if len(jg) == 0 && len(jp) == 0 {
			continue
		}
		e := &jobEditor{d: d, s: j.Script, after: j.After, pj: j.Plan, gates: jg, preps: jp, clone: o.CloneDir}
		cp := d.Checkpoint()
		err := shelledit.Errorf(0, "the job's script is not the one setup read")
		if j.Script != nil && len(j.Script.Items) == len(j.Plan.Steps) {
			err = e.apply()
		}
		if err == nil && len(jg) > 0 && j.Extra != nil {
			var notes []plan.Note
			notes, err = j.Extra()
			e.notes = append(e.notes, notes...)
		}
		if err == nil {
			if _, rerr := d.Result(); rerr != nil {
				err = shelledit.Errorf(0, "the edited file would not be valid YAML (%v)", rerr)
			}
		}
		if err != nil {
			d.Rollback(cp)
			out.Failed = append(out.Failed, j.Plan.ID)
			out.Notes = append(out.Notes, failNote(file, j, jg, err, o.Snippet))
			for _, g := range jg {
				unpinned = unpinned || !pinned(g.Version)
			}
			continue
		}
		out.Notes = append(out.Notes, e.notes...)
		tags = append(tags, e.tags...)
		attest = attest || e.attest
		unpinned = unpinned || e.unpinned
		if len(jg) > 0 {
			gated = append(gated, j.Plan.ID)
		}
	}
	if o.Finish != nil {
		if err := o.Finish(gated); err != nil {
			return plan.Edited{}, err
		}
	}
	res, err := d.Result()
	if err != nil {
		return plan.Edited{}, err
	}
	if !bytes.Equal(res, src) {
		out.Src = res
	}
	out.Notes = append(out.Notes, FileNotes(file, tags, attest, unpinned)...)
	return out, nil
}

// FileNotes are the once-per-file notes: TAG_ADDED for the tags added, ATTESTATIONS_DROPPED when
// a --push build was converted, UNPINNED_VERSION when an install line without a pin was written.
func FileNotes(file string, tags []string, attest, unpinned bool) []plan.Note {
	var out []plan.Note
	if len(tags) > 0 {
		tags = slices.Clone(tags)
		slices.Sort(tags)
		out = append(out, plan.Note{
			Code: "TAG_ADDED",
			Message: fmt.Sprintf("setup adds the local tag %s to the gated image builds and the gate scans it; it is never pushed, so a scan that falls back to the registry fails instead of reading an older image",
				strings.Join(slices.Compact(tags), ", ")),
			File: file,
		})
	}
	if attest {
		out = append(out, plan.Note{
			Code:    "ATTESTATIONS_DROPPED",
			Message: "the --push image builds now load the image instead, and docker push sends their original tags after the gate; pushing with docker push drops the provenance and SBOM attestations BuildKit attaches when it pushes directly",
			File:    file,
		})
	}
	if unpinned {
		out = append(out, plan.UnpinnedNote(file))
	}
	return out
}

// failNote is UNSUPPORTED_EDIT for a job that was left unchanged, with the snippet of each of its gates.
func failNote(file string, j Job, gates []plan.Gate, err error, snippet func(plan.Gate) string) plan.Note {
	key, line := j.Key, j.Line
	if key == "" {
		key = j.Plan.ID
	}
	if ee, ok := err.(*shelledit.Error); ok && ee.Line > 0 {
		line = ee.Line
	}
	msg := fmt.Sprintf("%s: job %s was left unchanged: %s.", file, key, err.Error())
	if len(gates) > 0 {
		msg += " Add these items by hand after the build and before any push or deploy (and tag each image with its SHEARBOLT_IMAGE value):"
		for _, g := range gates {
			if snippet != nil {
				msg += "\n" + snippet(g)
			}
		}
	} else {
		msg += " Its cargo builds were not rewritten to cargo auditable build, so their binaries carry no crate list; the source target still checks Cargo.lock."
	}
	return plan.Note{Code: "UNSUPPORTED_EDIT", Message: msg, File: file, Line: line}
}

// jobEditor queues one job's edits.
type jobEditor struct {
	d     *yamledit.Doc
	s     *Script
	after *Script
	pj    plan.Job
	gates []plan.Gate
	preps []plan.Prep
	clone string

	items    map[int]*itemPlan
	installs []int // insertion slots of the install items this edit adds
	notes    []plan.Note
	tags     []string
	attest   bool
	unpinned bool
}

// itemPlan is what happens to one script item.
type itemPlan struct {
	i         int
	it        *Item
	gate      int // index of the gate whose builds are in this item; -1 for none
	multi     bool
	builds    []plan.Build // gated builds of the item, by command
	owners    []int        // per build: the index of the gate it belongs to
	edits     []shelledit.Edit
	added     []string // items setup adds right after this item (its first part): railpack tags
	hasPush   bool
	pushes    []string // docker push items for its --push builds, after the gate
	split     bool
	cut, rest int    // the item keeps Text[:cut]; Text[rest:] becomes the remainder
	remainder string // the remainder item's text, after the gate ("" for none)
}

// at is a position in the job: an item and a command in it.
type at struct{ step, cmd int }

func (a at) less(b at) bool { return a.step < b.step || a.step == b.step && a.cmd < b.cmd }

func samePos(a, b plan.Pos) bool {
	return a.File == b.File && a.Job == b.Job && a.Step == b.Step && a.Command == b.Command
}

func buildAt(builds []plan.Build, pos plan.Pos) bool {
	return slices.ContainsFunc(builds, func(b plan.Build) bool { return samePos(b.Pos, pos) })
}

func imageVarOf(g plan.Gate, pos plan.Pos) *plan.ImageVar {
	for i := range g.Images {
		if samePos(g.Images[i].Build.Pos, pos) {
			return &g.Images[i]
		}
	}
	return nil
}

func (e *jobEditor) planFor(i int) *itemPlan {
	if sp, ok := e.items[i]; ok {
		return sp
	}
	sp := &itemPlan{i: i, it: &e.s.Items[i], gate: -1}
	e.items[i] = sp
	return sp
}

func (e *jobEditor) sortedPlans() []*itemPlan {
	var out []*itemPlan
	for _, sp := range e.items {
		out = append(out, sp)
	}
	slices.SortFunc(out, func(a, b *itemPlan) int { return a.i - b.i })
	return out
}

func (e *jobEditor) apply() error {
	e.items = map[int]*itemPlan{}
	items := e.s.Items
	inRange := func(p plan.Pos) bool {
		return p.Step >= 0 && p.Step < len(items) && p.Command < len(items[p.Step].Cmds)
	}

	// 1. The gated builds of each item.
	for gi, g := range e.gates {
		if g.Existing != nil && (!inRange(g.Existing.Pos) || g.Existing.Pos.Command < 0) || g.Existing == nil && !inRange(g.At) {
			return shelledit.Errorf(0, "the gate position is not in the job")
		}
		for _, b := range g.Builds {
			if !inRange(b.Pos) || b.Pos.Command < 0 {
				return shelledit.Errorf(0, "a gated build is not in the job")
			}
			sp := e.planFor(b.Pos.Step)
			if sp.gate >= 0 && sp.gate != gi {
				sp.multi = true
			}
			sp.gate = gi
			sp.builds = append(sp.builds, b)
			sp.owners = append(sp.owners, gi)
		}
		for _, iv := range g.Images {
			if !buildAt(g.Builds, iv.Build.Pos) {
				return shelledit.Errorf(0, "an image to scan is not among the gate's builds")
			}
		}
	}
	// Each scanned image needs its own local tag, or one image would be scanned twice.
	tagAt := map[string]plan.Pos{}
	for _, g := range e.gates {
		for _, iv := range g.Images {
			v := shelledit.ShellValue(iv.ScanTag)
			if p, ok := tagAt[v]; ok && !samePos(p, iv.Build.Pos) {
				return shelledit.Errorf(iv.Build.Pos.Line, "two image builds in this job share the scan tag %s, so the gate cannot tell their images apart; give each build its own shearbolt- tag or remove them and run setup again", v)
			}
			tagAt[v] = iv.Build.Pos
		}
	}
	// docker push --all-tags of a scanned repository would push its scan tag.
	var scanTags []string
	for _, g := range e.gates {
		for _, iv := range g.Images {
			scanTags = append(scanTags, iv.ScanTag)
		}
	}
	for _, sc := range []*Script{e.s, e.after} {
		for i := 0; sc != nil && i < len(sc.Items); i++ {
			it := &sc.Items[i]
			if err := shelledit.AllTagsPushes(it.Cmds, scanTags, it.line); err != nil {
				return err
			}
		}
	}
	for _, sp := range e.items {
		idx := make([]int, len(sp.builds))
		for i := range idx {
			idx[i] = i
		}
		slices.SortStableFunc(idx, func(a, b int) int { return sp.builds[a].Pos.Command - sp.builds[b].Pos.Command })
		builds, owners := make([]plan.Build, len(idx)), make([]int, len(idx))
		for k, i := range idx {
			builds[k], owners[k] = sp.builds[i], sp.owners[i]
		}
		sp.builds, sp.owners = builds, owners
	}

	// 2. Image builds: scan tag, --load, --push.
	for _, sp := range e.sortedPlans() {
		for i, b := range sp.builds {
			if b.Via == "" && b.Kind == plan.BuildImage && b.Image != nil {
				if err := e.imageEdits(sp, e.gates[sp.owners[i]], b); err != nil {
					return err
				}
			}
		}
	}

	// 3. Rust rewrites in the CI file's own items.
	for _, pr := range e.preps {
		for _, in := range pr.Inserts {
			if in.Via != "" {
				continue
			}
			if !inRange(in.Pos) || items[in.Pos.Step].Pipe != "" || in.Offset < 0 || in.Offset > len(items[in.Pos.Step].Text) {
				return shelledit.Errorf(0, "a cargo build to rewrite is not in the job")
			}
			sp := e.planFor(in.Pos.Step)
			sp.edits = append(sp.edits, shelledit.Edit{At: in.Offset, End: in.Offset, Text: in.Text})
		}
	}

	// 4. Splits.
	for _, sp := range e.sortedPlans() {
		if err := e.decideSplit(sp); err != nil {
			return err
		}
	}

	// 5. Existing gates: their scan commands and the install items before them (R8).
	for _, g := range e.gates {
		if g.Existing != nil {
			if err := e.updateExisting(g); err != nil {
				return err
			}
		}
	}
	if len(e.gates) > 0 {
		if err := e.updateInstalls(); err != nil {
			return err
		}
	}

	// 6. Changed items: in place, plus the items setup adds right after them.
	for _, sp := range e.sortedPlans() {
		if err := e.renderItem(sp); err != nil {
			return err
		}
	}

	// 7. The Rust install, before the first build that needs it.
	if err := e.install(); err != nil {
		return err
	}

	// 8. Gates, then the push items and remainders that follow them.
	for gi, g := range e.gates {
		if err := e.placeGate(gi, g); err != nil {
			return err
		}
	}
	return nil
}

// imageEdits plans the changes to gated image build b in item sp (R2, R5): the scan tag (or a
// docker tag item for railpack), --load for buildx, and for --push the conversion to --load
// with docker push items for the original tags. A build whose image the gate scans must run
// unconditionally at the top level of its item, so the gate can follow it.
func (e *jobEditor) imageEdits(sp *itemPlan, g plan.Gate, b plan.Build) error {
	it := sp.it
	c := b.Pos.Command
	cmd := it.Cmds[c]
	line := it.line(cmd)
	if cmd.Start != b.Cmd.Start {
		return shelledit.Errorf(line, "the build command could not be located in its item")
	}
	img := b.Image
	var added []string
	if iv := imageVarOf(g, b.Pos); iv != nil {
		if err := shelledit.Insertable(cmd, line, "run the gate that scans its image after it"); err != nil {
			return err
		}
		switch img.Tool {
		case "docker", "buildx":
			ed, needed, err := shelledit.TagEdit(img, iv.ScanTag, len(it.Text), line)
			if err != nil {
				return err
			}
			if needed {
				sp.edits = append(sp.edits, ed)
				e.tags = append(e.tags, shelledit.ShellValue(iv.ScanTag))
			}
		case "railpack":
			if len(img.Tags) == 0 {
				return shelledit.Errorf(line, "this railpack build has no --name to tag")
			}
			if !e.railpackTagged(sp.i, c, img) {
				added = append(added, "docker tag "+img.Tags[0]+" "+iv.ScanTag)
				e.tags = append(e.tags, shelledit.ShellValue(iv.ScanTag))
			}
		default:
			return shelledit.Errorf(line, "setup cannot add a scan tag to a %s build", img.Tool)
		}
	}
	if img.Push && (img.Tool == "docker" || img.Tool == "buildx") {
		edits, pushes, err := shelledit.PushEdits(cmd, img, line)
		if err != nil {
			return err
		}
		if err := shelledit.AttestationFlags(cmd.ArgWords(), line); err != nil {
			return err
		}
		sp.edits = append(sp.edits, edits...)
		sp.pushes = append(sp.pushes, pushes...)
		sp.hasPush = true
		e.attest = true
	} else if ed, ok := shelledit.LoadEdit(img); ok {
		if err := shelledit.AttestationFlags(cmd.ArgWords(), line); err != nil {
			return err
		}
		sp.edits = append(sp.edits, ed)
	}
	if len(added) > 0 {
		at, text := shelledit.InsertPoint(it.Text, cmd, added)
		if strings.TrimSpace(it.Text[at:]) == "" {
			sp.added = append(sp.added, added...) // the build ends the item: new items after it
		} else {
			sp.edits = append(sp.edits, shelledit.Edit{At: at, End: at, Text: text})
		}
	}
	return nil
}

// railpackTagged reports whether setup's docker tag for the railpack build at command c of item
// i follows it: in the same item, or in the items of setup's own lines right after it.
func (e *jobEditor) railpackTagged(i, c int, img *plan.Image) bool {
	items := e.s.Items
	if shelledit.RailpackTagged(items[i].Cmds, c, img) {
		return true
	}
	for k := i + 1; k < len(items) && items[k].Pipe == "" && len(items[k].Cmds) > 0 && !slices.ContainsFunc(items[k].Cmds, func(c shell.Command) bool { return !shelledit.OwnLine(c) }); k++ {
		if shelledit.RailpackTagged(items[k].Cmds, -1, img) {
			return true
		}
	}
	return false
}

// decideSplit splits an item after its last gated build when another command follows that
// build in the gate's item (P1), or when one of its gated builds uses --push (R4 rule 6). An item
// that cannot be split fails the job.
func (e *jobEditor) decideSplit(sp *itemPlan) error {
	if len(sp.builds) == 0 || sp.it.Pipe != "" {
		return nil
	}
	it := sp.it
	g := e.gates[sp.gate]
	last := sp.builds[len(sp.builds)-1].Pos.Command
	gated := map[int]bool{}
	for _, b := range sp.builds {
		gated[b.Pos.Command] = true
	}
	need := sp.hasPush
	for _, og := range e.gates {
		if og.Existing == nil && !og.Before && og.At.Step == sp.i && og.At.Command >= 0 && shelledit.Follows(it.Cmds, og.At.Command, gated) {
			need = true
		}
	}
	if !need {
		return nil
	}
	for last+1 < len(it.Cmds) && shelledit.OwnLine(it.Cmds[last+1]) {
		last++
	}
	cmd := it.Cmds[last]
	line := it.line(cmd)
	switch {
	case sp.multi:
		return shelledit.Errorf(line, "builds of two projects share this item, which would need two splits")
	case g.Existing != nil && g.Existing.Pos.Step == sp.i:
		return shelledit.Errorf(line, "the existing gate runs in the same item as the --push build, so the push cannot move after it")
	}
	if err := shelledit.Insertable(cmd, line, "split the item after it"); err != nil {
		return err
	}
	sp.split = true
	sp.cut, sp.rest = shelledit.SplitPoints(it.Text, cmd)
	return nil
}

// editable reports whether an item's own text can be changed: a command that is not an alias
// and defines no anchor (editing either would change other items too).
func editable(it *Item) bool {
	return it.Pipe == "" && it.Node.Kind != yaml.AliasNode && it.Node.Anchor == "" && it.Value.Anchor == ""
}

// renderItem queues an item's new text (its first part when it is split) and the items setup
// adds right after it.
func (e *jobEditor) renderItem(sp *itemPlan) error {
	it := sp.it
	if it.Pipe != "" {
		return nil
	}
	line := it.itemLine()
	text := it.Text
	newText := text
	if sp.split {
		var first, second []shelledit.Edit
		for _, ed := range sp.edits {
			switch {
			case ed.At <= sp.cut && ed.End <= sp.cut:
				first = append(first, ed)
			case ed.At >= sp.rest:
				second = append(second, ed)
			default:
				return shelledit.Errorf(line, "an edit falls on the split point")
			}
		}
		part1, err := shelledit.Apply(text[:sp.cut], 0, first)
		if err != nil {
			return shelledit.Errorf(line, "%v", err)
		}
		rest, err := shelledit.Apply(text[sp.rest:], sp.rest, second)
		if err != nil {
			return shelledit.Errorf(line, "%v", err)
		}
		sp.remainder = itemValue(rest)
		newText = itemValue(part1)
	} else {
		var err error
		if newText, err = shelledit.Apply(text, 0, sp.edits); err != nil {
			return shelledit.Errorf(line, "%v", err)
		}
	}
	if newText != text {
		if !editable(it) {
			return shelledit.Errorf(line, "this item is a YAML alias or defines an anchor, so setup cannot change it in place")
		}
		if err := e.replaceItem(it, newText); err != nil {
			return shelledit.Errorf(line, "the item cannot be rewritten (%v)", err)
		}
	}
	if len(sp.added) > 0 {
		if err := e.insertItems(sp.i, sp.added); err != nil {
			return shelledit.Errorf(line, "setup cannot add items after this one (%v)", err)
		}
	}
	return nil
}

// itemValue normalizes the text of an item part: one line without a line break, or several
// lines ending in one.
func itemValue(s string) string {
	body := strings.TrimRight(s, "\n")
	if !strings.Contains(body, "\n") {
		return strings.TrimLeft(body, " \t")
	}
	return body + "\n"
}

// replaceItem queues item it's new text. A block that becomes one line becomes a plain (or
// quoted) scalar; anything else keeps its style.
func (e *jobEditor) replaceItem(it *Item, v string) error {
	n := it.Value
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 && !strings.Contains(v, "\n") {
		return e.d.ReplaceScalar(n, yamledit.Scalar(v))
	}
	first, last, lines, err := e.d.ScriptLines(n, v)
	if err != nil {
		return err
	}
	return e.d.ReplaceLines(first, last, lines)
}

// insertItems queues new command items after item after (-1: before the first item).
func (e *jobEditor) insertItems(after int, values []string) error {
	dc, err := e.d.DashColumn(e.s.Seq)
	if err != nil {
		return err
	}
	cc, err := e.d.ContentColumn(e.s.Seq)
	if err != nil {
		return err
	}
	for _, v := range values {
		lines := yamledit.RenderScript(v, 0, dc, cc)
		pad := strings.Repeat(" ", cc)
		for k := 1; k < len(lines); k++ {
			lines[k] = strings.TrimPrefix(lines[k], pad)
		}
		if err := e.d.InsertItem(e.s.Seq, after, lines); err != nil {
			return err
		}
	}
	return nil
}

// install queues the first Install of the job's preps as an item before the first build that
// needs it, unless an earlier item, or the item before that build, already mentions its Marker.
func (e *jobEditor) install() error {
	var first *plan.Prep
	for i := range e.preps {
		pr := &e.preps[i]
		if pr.Install == nil {
			continue
		}
		if first == nil || (at{pr.Pos.Step, pr.Pos.Command}).less(at{first.Pos.Step, first.Pos.Command}) {
			first = pr
		}
	}
	if first == nil {
		return nil
	}
	items := e.s.Items
	s := first.Pos.Step
	if s < 0 || s >= len(items) {
		return shelledit.Errorf(0, "the build that needs %s is not in the job", first.Install.Name)
	}
	in := first.Install
	if in.Marker != "" {
		for i := 0; i < s; i++ {
			if mentions(items[i].Cmds, in.Marker) || strings.Contains(items[i].Pipe, in.Marker) {
				return nil
			}
		}
		if c := first.Pos.Command; c >= 0 && c <= len(items[s].Cmds) && mentions(items[s].Cmds[:c], in.Marker) {
			return nil
		}
	}
	if in.Run == "" {
		return shelledit.Errorf(items[s].itemLine(), "setup has no command that installs %s", in.Name)
	}
	if err := e.insertItems(s-1, []string{in.Run}); err != nil {
		return shelledit.Errorf(items[s].itemLine(), "setup cannot add the item that installs %s (%v)", in.Name, err)
	}
	return nil
}

// mentions reports whether a word of cmds contains marker. Comments do not count.
func mentions(cmds []shell.Command, marker string) bool {
	return slices.ContainsFunc(cmds, func(c shell.Command) bool {
		return slices.ContainsFunc(c.Words, func(w shell.Word) bool { return strings.Contains(w.Value, marker) })
	})
}

// ---- gates ----

// slot returns where the items of gate g are inserted: after this item (-1: before the first).
func (e *jobEditor) slot(g plan.Gate) int {
	switch {
	case g.Existing != nil:
		return g.Existing.Pos.Step - 1
	case g.Before:
		return g.At.Step - 1
	}
	return g.At.Step
}

// runsBefore reports whether command c of item s runs before the items of gate g.
func (e *jobEditor) runsBefore(s, c int, g plan.Gate) bool {
	switch {
	case g.Existing != nil:
		return (at{s, c}).less(at{g.Existing.Pos.Step, g.Existing.Pos.Command})
	case g.Before:
		return s < g.At.Step
	case s < g.At.Step:
		return true
	case s > g.At.Step:
		return false
	}
	if sp := e.items[s]; sp != nil && sp.split {
		return e.s.Items[s].Cmds[c].End <= sp.cut
	}
	return true
}

// before returns the commands that run in the job's shell before gate g, in order.
func (e *jobEditor) before(g plan.Gate) []shell.Command {
	out := slices.Clone(e.s.Prelude)
	for s, it := range e.s.Items {
		for c, cmd := range it.Cmds {
			if e.runsBefore(s, c, g) {
				out = append(out, cmd)
			}
		}
	}
	return out
}

// errexitOff returns the set command after which errexit may still be off when gate g runs.
// The gate shares the job's shell, so a failing scan would not stop the job there.
func (e *jobEditor) errexitOff(g plan.Gate) *shell.Command { return shelledit.ErrexitOff(e.before(g)) }

// earlyStop is a command that can stop the job's shell early: its line, its name ("exit", "exec"
// or "source"), and where it runs (at.step -1: in the prelude, before every item).
type earlyStop struct {
	line int
	cmd  string
	pos  at
}

// stopBefore returns the last command that can stop the job's shell early before gate g runs
// (P12, design §3.5), or nil. Without one in the items, it is the prelude's stop (GitLab's
// before_script), which runs before every item. It runs once the splits are decided, since
// runsBefore needs them. A stop index outside its item fails the job.
func (e *jobEditor) stopBefore(g plan.Gate) (*earlyStop, error) {
	for s, st := range e.pj.Steps {
		for _, c := range st.Stops {
			if c < 0 || c >= len(e.s.Items[s].Cmds) {
				return nil, shelledit.Errorf(0, "a command that can stop the job early is not in the job")
			}
		}
	}
	for s := len(e.pj.Steps) - 1; s >= 0; s-- {
		stops := e.pj.Steps[s].Stops
		for k := len(stops) - 1; k >= 0; k-- {
			if c := stops[k]; e.runsBefore(s, c, g) {
				it := &e.s.Items[s]
				return &earlyStop{line: it.line(it.Cmds[c]), cmd: shelledit.StopName(it.Cmds[c]), pos: at{s, c}}, nil
			}
		}
	}
	if e.pj.PreludeStop != 0 || e.pj.PreludeStopCmd != "" {
		return &earlyStop{line: e.pj.PreludeStop, cmd: e.pj.PreludeStopCmd, pos: at{-1, -1}}, nil
	}
	return nil, nil
}

// skipSafe reports whether gate g may be skipped when the job's shell stops early at pos
// (at.step -1: in the prelude, before every item): g scans only its own images, and every build
// it gates comes after the stop (P12, condition 1 of design §3.3).
func skipSafe(g plan.Gate, pos at) bool {
	return g.OnlyOwnImages && !slices.ContainsFunc(g.Builds, func(b plan.Build) bool { return !pos.less(at{b.Pos.Step, b.Pos.Command}) })
}

// unchecked names what gate g would leave unchecked when the job's shell stops early at pos: the
// targets it scans other than the images built after the stop (an image built before the stop is
// listed), or every target when what it scans is unknown.
func unchecked(g plan.Gate, pos at) string {
	if g.Scans == nil {
		return "every target in shearbolt.yaml"
	}
	built := map[string]bool{}
	for _, iv := range g.Images {
		if pos.less(at{iv.Build.Pos.Step, iv.Build.Pos.Command}) {
			built[iv.Target] = true
		}
	}
	var rest []string
	for _, n := range g.Scans {
		if !built[n] {
			rest = append(rest, n)
		}
	}
	if len(rest) == 0 {
		rest = g.Scans // only a build without a target of its own runs before the stop
	}
	return strings.Join(rest, ", ")
}

// installed reports whether shearbolt is installed before gate g: an install command of the job
// runs before it, or this edit adds an install item at or before its slot.
func (e *jobEditor) installed(g plan.Gate) bool {
	for s, st := range e.pj.Steps {
		for _, gc := range st.Gates {
			if gc.Install && gc.Pos.Command >= 0 && e.runsBefore(s, gc.Pos.Command, g) {
				return true
			}
		}
	}
	return slices.ContainsFunc(e.installs, func(sl int) bool { return sl <= e.slot(g) })
}

// placeGate inserts gate g's install and scan items (the install only when none runs before
// it), or adds the missing install before an existing gate, and puts the push items and
// remainders of the items it gates after it.
func (e *jobEditor) placeGate(gi int, g plan.Gate) error {
	slot := e.slot(g)
	line := 0
	if slot+1 < len(e.s.Items) {
		line = e.s.Items[slot+1].itemLine()
	}
	if off := e.errexitOff(g); off != nil {
		return shelledit.Errorf(line, "%s turns off errexit (set +e) before the gate, and the gate runs in the same shell, so a failing scan would not stop the job; turn it back on with set -e before the gate", shelledit.CommandText(*off))
	}
	// An early stop before the gate ends the job's script successfully: the gate is skipped and
	// later jobs still run (P12).
	stop, err := e.stopBefore(g)
	if err != nil {
		return err
	}
	if stop != nil && !skipSafe(g, stop.pos) {
		return shelledit.Errorf(stop.line, "the job can stop before the shearbolt gate (%s at line %d), which would skip its check of %s while later jobs still run; move the early stop into the job's rules or condition, or add the gate by hand where it runs on every path", stop.cmd, stop.line, unchecked(g, stop.pos))
	}
	var gateItems []string
	if !e.installed(g) {
		gateItems = append(gateItems, InstallLine(g.Version))
		e.installs = append(e.installs, slot)
		e.unpinned = e.unpinned || !pinned(g.Version)
	}
	if g.Existing == nil {
		gateItems = append(gateItems, ScanLine(g, e.clone))
	}
	if err := e.insertItems(slot, gateItems); err != nil {
		return shelledit.Errorf(line, "setup cannot add the gate items (%v)", err)
	}
	anchor := slot
	if g.Existing != nil {
		anchor = g.Existing.Pos.Step
	}
	for _, sp := range e.sortedPlans() {
		if sp.gate != gi {
			continue
		}
		after := slices.Clone(sp.pushes)
		if sp.split && sp.remainder != "" {
			after = append(after, sp.remainder)
		}
		if err := e.insertItems(anchor, after); err != nil {
			return shelledit.Errorf(sp.it.itemLine(), "setup cannot add the items that follow the gate (%v)", err)
		}
	}
	return nil
}

// imageVarName matches the variables setup manages in a scan's prefix.
var imageVarName = regexp.MustCompile(`^SHEARBOLT_IMAGE(_[A-Z0-9_]+)?=`)

// assignment matches a VAR=value word.
var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// updateExisting re-renders an existing scan command (R8): its image variables, --ci, --config
// and --target are setup's; its program, other assignments and other arguments are kept.
func (e *jobEditor) updateExisting(g plan.Gate) error {
	ex := g.Existing
	sp := e.planFor(ex.Pos.Step)
	it := sp.it
	cmd := it.Cmds[ex.Pos.Command]
	line := it.line(cmd)
	if err := shelledit.UsableScan(it.Cmds, ex.Pos.Command, line); err != nil {
		return err
	}
	args := cmd.ArgWords()
	if len(args) < 2 || len(cmd.Words) == 0 {
		return shelledit.Errorf(line, "the existing scan command could not be read")
	}
	k := slices.IndexFunc(cmd.Words, func(w shell.Word) bool { return w.Start == args[0].Start })
	var words []string
	for _, w := range cmd.Words[:max(k, 0)] {
		if !assignment.MatchString(w.Raw) {
			return shelledit.Errorf(line, "the existing scan command %s runs through a wrapper, so setup cannot update it", shelledit.CommandText(cmd))
		}
		if !imageVarName.MatchString(w.Raw) {
			words = append(words, w.Raw)
		}
	}
	for _, iv := range g.Images {
		words = append(words, iv.Var+"="+iv.ScanTag)
	}
	words = append(words, args[0].Raw, "scan")
	words = append(words, managedArgs(g, e.clone)...)
	rest := args[2:]
	for i := 0; i < len(rest); i++ {
		v := rest[i].Value
		if f, ok := strings.CutPrefix(v, "-"); ok {
			v = strings.TrimPrefix(f, "-") // flag accepts -name and --name
		} else {
			v = ""
		}
		name, val, eq := strings.Cut(v, "=")
		switch name {
		case "ci":
			continue
		case "config", "target":
			if !eq && i+1 < len(rest) {
				i++ // and its value
				val = rest[i].Value
			}
			if name == "config" && !e.ownConfig(g, val, cmd.Before.Dir) {
				return shelledit.Errorf(line, "the existing scan uses --config %s, which is not %s, and setup does not change a config it did not write; point it at the project's shearbolt.yaml, or add the gate by hand", val, g.Config)
			}
			continue
		}
		words = append(words, rest[i].Raw)
	}
	start, end := cmd.Words[0].Start, 0
	for _, w := range cmd.Words {
		end = max(end, w.Start+len(w.Raw))
	}
	for _, r := range cmd.Redirs {
		if r.Start < end {
			return shelledit.Errorf(line, "the existing scan command %s has a redirection among its arguments, so setup cannot update it", shelledit.CommandText(cmd))
		}
	}
	if text := strings.Join(words, " "); text != it.Text[start:end] {
		sp.edits = append(sp.edits, shelledit.Edit{At: start, End: end, Text: text})
	}
	return nil
}

// ownConfig reports whether an existing scan's --config value names gate g's own config:
// <clone dir>/<dir>/shearbolt.yaml (also ${...}), or <dir>/shearbolt.yaml relative to the root
// or to the directory the scan runs in.
func (e *jobEditor) ownConfig(g plan.Gate, val, dir string) bool {
	want := g.Config
	if want == "" {
		want = path.Join(dirOf(g), "shearbolt.yaml")
	}
	name := strings.TrimPrefix(e.clone, "$")
	for _, pre := range []string{e.clone + "/", "${" + name + "}/"} {
		if rest, ok := strings.CutPrefix(val, pre); ok {
			return path.Clean(rest) == want
		}
	}
	if strings.ContainsAny(val, "$`") || path.IsAbs(val) {
		return false
	}
	return path.Clean(val) == want || dir != "" && path.Join(dir, val) == want
}

// updateInstalls re-renders the job's install items for a release build, so the pin follows
// the version (R8). Only items that hold nothing but the install are changed; a development
// build keeps them as they are.
func (e *jobEditor) updateInstalls() error {
	version := e.gates[0].Version
	if !pinned(version) {
		return nil
	}
	want := InstallLine(version)
	for s, st := range e.pj.Steps {
		if !slices.ContainsFunc(st.Gates, func(g plan.GateCmd) bool { return g.Install }) || !installOnly(e.s.Items[s], st) || !editable(&e.s.Items[s]) {
			continue // an install of the user's own, or one shared through an anchor, is kept
		}
		if it := &e.s.Items[s]; it.Text != want {
			sp := e.planFor(s)
			sp.edits = append(sp.edits, shelledit.Edit{At: 0, End: len(it.Text), Text: want})
		}
	}
	return nil
}

// installOnly reports whether an item holds only an install: install commands, the closing ) of
// their subshell, and the sh (or bash) they pipe into.
func installOnly(it Item, st plan.Step) bool {
	if it.Pipe != "" || len(it.Cmds) == 0 || !slices.ContainsFunc(st.Gates, func(g plan.GateCmd) bool { return g.Install }) {
		return false
	}
	for c, cmd := range it.Cmds {
		a := cmd.Argv()
		switch {
		case slices.ContainsFunc(st.Gates, func(g plan.GateCmd) bool { return g.Install && g.Pos.Command == c }):
		case len(cmd.Words) == 1 && cmd.Words[0].Raw == ")":
		case len(a) == 1 && (a[0] == "sh" || a[0] == "bash") && c == len(it.Cmds)-1:
		default:
			return false
		}
	}
	return true
}
