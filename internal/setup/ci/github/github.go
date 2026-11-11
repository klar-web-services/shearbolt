// Package github is the GitHub Actions plugin of shearbolt setup: it parses workflows into the
// CI-neutral job model and splices gates, image tags, step splits and installs into them.
package github

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/shelledit"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

const (
	workflowsDir = ".github/workflows"
	gateAction   = "klar-web-services/shearbolt"
	gateUses     = gateAction + "@v1"
)

// pushActions are the actions that push or deploy (R2).
var pushActions = []string{
	"softprops/action-gh-release", "ncipollo/release-action", "actions/upload-release-asset",
	"aws-actions/amazon-ecs-deploy-task-definition", "google-github-actions/deploy-cloudrun",
	"azure/webapps-deploy", "superfly/flyctl-actions", "amondnet/vercel-action",
	"cloudflare/wrangler-action", "appleboy/ssh-action", "appleboy/scp-action",
}

// Plugin is the GitHub Actions plugin.
type Plugin struct{}

// Name implements plan.CI.
func (Plugin) Name() string { return "github" }

// Files returns the workflow files: .github/workflows/*.yml and *.yaml, sorted.
func (Plugin) Files(r plan.Repo) []string {
	names, err := r.ReadDir(workflowsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if !strings.HasSuffix(n, ".yml") && !strings.HasSuffix(n, ".yaml") {
			continue
		}
		p := workflowsDir + "/" + n
		if _, err := r.ReadDir(p); err == nil {
			continue // a directory
		}
		if r.Exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// ---- the workflow model ----

// workflow is a parsed workflow file.
type workflow struct {
	file    string
	doc     *yamledit.Doc
	env     []string // workflow env names
	jobs    []*job
	skipped []string // jobs that cannot be edited: "key (reason)"
}

// job is one editable job.
type job struct {
	key     string
	id      string
	node    *yaml.Node // the job mapping
	seq     *yaml.Node // its steps sequence
	line    int
	env     []string
	windows bool // the runner may be Windows, where run steps default to PowerShell
	// runPosix: a new run step without shell: runs in bash or sh (the job's and workflow's
	// defaults.run.shell, else the runner's default).
	runPosix bool
	steps    []*step
}

// step is one workflow step.
type step struct {
	item   *yaml.Node // the sequence item as written (an alias when the step is one)
	m      *yaml.Node // the step's mapping (resolved); nil when the item is not a mapping
	line   int
	name   string
	id     string
	uses   string
	ifNode *yaml.Node
	run    *yaml.Node // the run scalar, or nil
	script string
	dir    string // working directory before the script ("" = unknown)
	posix  bool   // the script runs in bash or sh
	// shell is the effective shell: setting (the step's shell:, else defaults.run.shell), and
	// shellSet whether one is set at all.
	shell    string
	shellSet bool
	env      []string
	cmds     []shell.Command
}

// runDefaults are a defaults.run mapping's values.
type runDefaults struct {
	dir, shell       string
	hasDir, hasShell bool
}

func readDefaults(d *yamledit.Doc, n *yaml.Node, parent runDefaults) runDefaults {
	out := parent
	if v := scalar(d.Get(n, "defaults", "run", "working-directory")); v != nil {
		out.dir, out.hasDir = v.Value, true
	}
	if v := scalar(d.Get(n, "defaults", "run", "shell")); v != nil {
		out.shell, out.hasShell = v.Value, true
	}
	return out
}

func scalar(n *yaml.Node) *yaml.Node {
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil
	}
	return n
}

func value(n *yaml.Node) string {
	if n = scalar(n); n == nil {
		return ""
	}
	return n.Value
}

// keys returns the scalar keys of mapping n (nil for anything else).
func keys(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, n.Content[i].Value)
	}
	return out
}

// rawValue returns the value node of key in mapping m as written (aliases not resolved).
func rawValue(m *yaml.Node, key string) *yaml.Node {
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

func resolveAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i < 100; i++ {
		n = n.Alias
	}
	return n
}

// hasAnchor reports whether n or anything inside it defines an anchor.
func hasAnchor(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Anchor != "" {
		return true
	}
	return slices.ContainsFunc(n.Content, hasAnchor)
}

// shared reports whether editing n would also change another part of the file: n is an alias,
// or defines an anchor that an alias may reuse.
func shared(n *yaml.Node) bool { return n != nil && (n.Kind == yaml.AliasNode || hasAnchor(n)) }

func flow(n *yaml.Node) bool { return n != nil && n.Style&yaml.FlowStyle != 0 }

// workDir returns a working-directory value as a repo-relative directory, or "" when it is not
// certain (an expression or variable, absolute, outside the repository).
func workDir(v string) string {
	if strings.ContainsAny(v, "$`") {
		return ""
	}
	return plan.CleanDir(v)
}

// parseWorkflow parses a workflow into its editable jobs. Jobs that call a reusable workflow, or
// have no steps, are not jobs. Jobs that use YAML anchors, aliases or merge keys, or flow-style
// steps, are left out and listed in skipped: editing them would change other jobs too, or the
// file cannot be edited in place.
func parseWorkflow(file string, src []byte) (*workflow, error) {
	d, err := yamledit.Parse(src)
	if err != nil {
		return nil, err
	}
	wf := &workflow{file: file, doc: d}
	root := d.Root()
	if root == nil || root.Kind != yaml.MappingNode {
		return wf, nil
	}
	wf.env = keys(d.Get(root, "env"))
	wdef := readDefaults(d, root, runDefaults{})
	jobs := d.Get(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return wf, nil
	}
	if flow(jobs) {
		for _, k := range keys(jobs) {
			wf.skipped = append(wf.skipped, k+" (flow-style jobs)")
		}
		return wf, nil
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		key, raw := jobs.Content[i].Value, jobs.Content[i+1]
		skip := func(reason string) { wf.skipped = append(wf.skipped, key+" ("+reason+")") }
		if shared(raw) {
			skip("it uses a YAML anchor or alias")
			continue
		}
		jm := raw
		if jm.Kind != yaml.MappingNode {
			continue
		}
		if slices.Contains(keys(jm), "<<") {
			skip("it uses a YAML merge key")
			continue
		}
		if rawValue(jm, "uses") != nil {
			continue // a reusable workflow call: no steps to edit
		}
		seq := rawValue(jm, "steps")
		switch {
		case seq == nil:
			continue
		case shared(seq):
			skip("its steps use a YAML anchor or alias")
			continue
		case seq.Kind != yaml.SequenceNode || len(seq.Content) == 0:
			continue
		case flow(seq):
			skip("flow-style steps")
			continue
		}
		j := &job{key: key, id: file + "#" + key, node: jm, seq: seq, line: jobs.Content[i].Line, env: keys(d.Get(jm, "env"))}
		j.windows = mayBeWindows(d, jm)
		jdef := readDefaults(d, jm, wdef)
		j.runPosix = posixShell(jdef.shell, jdef.hasShell, j.windows)
		merge := false
		for _, item := range seq.Content {
			st := &step{item: item, m: resolveAlias(item), line: item.Line}
			if st.m != nil && st.m.Kind != yaml.MappingNode {
				st.m = nil
			}
			if st.m != nil {
				if slices.Contains(keys(st.m), "<<") {
					merge = true
				}
				if len(st.m.Content) > 0 {
					st.line = st.m.Content[0].Line
				}
				st.name = value(d.Get(st.m, "name"))
				st.id = value(d.Get(st.m, "id"))
				st.uses = value(d.Get(st.m, "uses"))
				st.ifNode = scalar(d.Get(st.m, "if"))
				st.env = keys(d.Get(st.m, "env"))
				if run := scalar(d.Get(st.m, "run")); run != nil {
					st.run, st.script = run, run.Value
					dir, shellName, hasShell := jdef.dir, jdef.shell, jdef.hasShell
					if !jdef.hasDir {
						dir = "."
					}
					if v := scalar(d.Get(st.m, "working-directory")); v != nil {
						dir = v.Value
					}
					if v := scalar(d.Get(st.m, "shell")); v != nil {
						shellName, hasShell = v.Value, true
					}
					st.dir = workDir(dir)
					st.posix = posixShell(shellName, hasShell, j.windows)
					st.shell, st.shellSet = shellName, hasShell
					st.cmds, _ = shell.Parse(st.script, shell.State{Dir: st.dir})
				}
			}
			j.steps = append(j.steps, st)
		}
		if merge {
			skip("a step uses a YAML merge key")
			continue
		}
		wf.jobs = append(wf.jobs, j)
	}
	return wf, nil
}

// posixShell reports whether a run step's shell is bash or sh. Without a shell setting, that is
// the runner's default, except on Windows (PowerShell).
func posixShell(name string, set, windows bool) bool {
	if !set {
		return !windows
	}
	name = strings.TrimSpace(name)
	return name == "bash" || name == "sh" || strings.HasPrefix(name, "bash ") || strings.HasPrefix(name, "sh ")
}

// mayBeWindows reports whether a job's runs-on may name a Windows runner: a label containing
// "windows", or an expression that cannot be resolved to labels without it. ${{ matrix.<key> }}
// is resolved through strategy.matrix.<key> and its include entries.
func mayBeWindows(d *yamledit.Doc, jm *yaml.Node) bool {
	ro := d.Get(jm, "runs-on")
	if ro == nil {
		return false
	}
	var labels []*yaml.Node
	switch ro.Kind {
	case yaml.ScalarNode:
		labels = []*yaml.Node{ro}
	case yaml.SequenceNode:
		labels = ro.Content
	case yaml.MappingNode:
		if l := d.Get(ro, "labels"); l != nil && l.Kind == yaml.SequenceNode {
			labels = l.Content
		} else if l != nil {
			labels = []*yaml.Node{l}
		}
		if g := value(d.Get(ro, "group")); windowsLabel(g) {
			return true
		}
	}
	for _, l := range labels {
		l = resolveAlias(l)
		if l == nil || l.Kind != yaml.ScalarNode {
			return true
		}
		v := strings.TrimSpace(l.Value)
		if windowsLabel(v) {
			return true
		}
		if !strings.Contains(v, "${{") {
			continue
		}
		key, ok := matrixKey(v)
		if !ok {
			return true
		}
		vals, ok := matrixValues(d, jm, key)
		if !ok {
			return true
		}
		for _, x := range vals {
			if windowsLabel(x) || strings.Contains(x, "${{") {
				return true
			}
		}
	}
	return false
}

// windowsLabel reports a runner label that may name a Windows runner: it contains "windows" or
// starts with "win", in any case.
func windowsLabel(v string) bool {
	l := strings.ToLower(strings.TrimSpace(v))
	return strings.Contains(l, "windows") || strings.HasPrefix(l, "win")
}

// matrixKey returns KEY for a value that is exactly ${{ matrix.KEY }}.
func matrixKey(v string) (string, bool) {
	inner, ok := strings.CutPrefix(v, "${{")
	if !ok {
		return "", false
	}
	inner, ok = strings.CutSuffix(inner, "}}")
	if !ok {
		return "", false
	}
	key, ok := strings.CutPrefix(strings.TrimSpace(inner), "matrix.")
	if !ok || key == "" || strings.ContainsAny(key, " .()[]|&!=<>'\"") {
		return "", false
	}
	return key, true
}

// matrixValues returns every literal value strategy.matrix.<key> can take, including those its
// include entries add; false when they cannot all be read.
func matrixValues(d *yamledit.Doc, jm *yaml.Node, key string) ([]string, bool) {
	m := d.Get(jm, "strategy", "matrix")
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, false
	}
	var out []string
	found := false
	if v := d.Get(m, key); v != nil {
		found = true
		if v.Kind != yaml.SequenceNode {
			return nil, false
		}
		for _, x := range v.Content {
			if x = resolveAlias(x); x == nil || x.Kind != yaml.ScalarNode {
				return nil, false
			}
			out = append(out, x.Value)
		}
	}
	if inc := d.Get(m, "include"); inc != nil {
		if inc.Kind != yaml.SequenceNode {
			return nil, false
		}
		for _, e := range inc.Content {
			if v := d.Get(e, key); v != nil {
				found = true
				if v.Kind != yaml.ScalarNode {
					return nil, false
				}
				out = append(out, v.Value)
			}
		}
	}
	return out, found
}

// cmdLine returns the file line where command c of the step's script starts.
func (st *step) cmdLine(c shell.Command) int {
	n := st.run
	nl := strings.Count(st.script[:min(c.Start, len(st.script))], "\n")
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return n.Line + 1 + nl
	}
	return n.Line + nl
}

// ---- Jobs ----

// Jobs parses a workflow into editable jobs (R2): run scripts are parsed from the step's
// working directory and every command classified; uses steps are existing gates or pushes.
func (p Plugin) Jobs(r plan.Repo, file string, src []byte, classify plan.Classifier) ([]plan.Job, []plan.Note, error) {
	wf, err := parseWorkflow(file, src)
	if err != nil {
		return nil, nil, err
	}
	var jobs []plan.Job
	for _, j := range wf.jobs {
		pj := plan.Job{ID: j.id, Label: path.Base(file) + ": " + j.key}
		for s, st := range j.steps {
			ps := plan.Step{Name: st.name, Dir: st.dir, Uses: st.uses}
			switch {
			case st.run != nil:
				ps.Commands = st.cmds
				for c, cmd := range st.cmds {
					pos := plan.Pos{File: file, Job: j.id, Step: s, Command: c, Line: st.cmdLine(cmd)}
					env := map[string]bool{}
					for _, list := range [][]string{wf.env, j.env, st.env, cmd.Before.Env, cmd.Prefix()} {
						for _, n := range list {
							env[n] = true
						}
					}
					cl := classify(cmd, plan.Context{Dir: cmd.Before.Dir, Env: env})
					for _, b := range cl.Builds {
						b.Pos = pos
						ps.Builds = append(ps.Builds, b)
					}
					if cl.Push {
						ps.Push = append(ps.Push, c)
					}
					if cl.Stops {
						ps.Stops = append(ps.Stops, c)
					}
					if cl.Gate != nil {
						g := *cl.Gate
						g.Pos = pos
						ps.Gates = append(ps.Gates, g)
					}
				}
				// The sh an install pipes into is part of the install (Batch 5 re-review).
				for _, g := range slices.Clone(ps.Gates) {
					sh := shelledit.InstallShell(st.cmds, g.Pos.Command)
					if !g.Install || sh < 0 || slices.ContainsFunc(ps.Gates, func(o plan.GateCmd) bool { return o.Pos.Command == sh }) {
						continue
					}
					pos := plan.Pos{File: file, Job: j.id, Step: s, Command: sh, Line: st.cmdLine(st.cmds[sh])}
					ps.Gates = append(ps.Gates, plan.GateCmd{Install: true, Pos: pos})
				}
			case st.uses != "":
				pos := plan.Pos{File: file, Job: j.id, Step: s, Command: -1, Line: st.line}
				switch {
				case actionIs(st.uses, gateAction):
					ps.Gates = append(ps.Gates, plan.GateCmd{Project: gateProject(wf.doc, st), Pos: pos})
				case actionIs(st.uses, buildPushAction):
					// Its own push is derived from Image.Push and OutputPush, never Step.Push.
					b := buildPushBuild(wf.doc, st, j.steps)
					b.Pos = pos
					ps.Builds = append(ps.Builds, b)
				case pushesAction(wf.doc, st):
					ps.Push = []int{-1}
				}
			}
			pj.Steps = append(pj.Steps, ps)
		}
		jobs = append(jobs, pj)
	}
	var notes []plan.Note
	if len(wf.skipped) > 0 {
		notes = append(notes, plan.Note{
			Code: "UNSUPPORTED_EDIT",
			Message: fmt.Sprintf("%s: these jobs cannot be edited automatically: %s. If one of them builds and pushes or deploys, add this step by hand after the build and before the push:\n%s",
				file, strings.Join(wf.skipped, ", "), p.Snippet(r, file, plan.Gate{})),
			File: file,
		})
	}
	return jobs, notes, nil
}

// actionIs reports whether a uses reference names action (owner/repo, case-insensitive).
func actionIs(uses, action string) bool {
	name, _, _ := strings.Cut(uses, "@")
	return strings.EqualFold(strings.TrimSuffix(name, "/"), action)
}

// gateProject returns the project an existing gate step checks: with.working-directory (default
// ".") joined with the directory of with.config; "" when that is not certain.
func gateProject(d *yamledit.Doc, st *step) string {
	wd, cfg := ".", "shearbolt.yaml"
	if v := scalar(d.Get(st.m, "with", "working-directory")); v != nil {
		wd = v.Value
	}
	if v := scalar(d.Get(st.m, "with", "config")); v != nil {
		cfg = v.Value
	}
	if strings.ContainsAny(wd+cfg, "$`") || path.IsAbs(cfg) {
		return ""
	}
	return plan.CleanDir(path.Join(wd, path.Dir(cfg)))
}

// pushesAction reports whether a uses step is a known deploy or release action (R2), or a
// docker/bake-action whose push input pushes (true in any case, or an expression).
func pushesAction(d *yamledit.Doc, st *step) bool {
	for _, a := range pushActions {
		if actionIs(st.uses, a) {
			return true
		}
	}
	return actionIs(st.uses, "docker/bake-action") && pushInput(value(d.Get(st.m, "with", "push")))
}

// ---- Snippet ----

// Snippet renders gate g as the step a user adds by hand, at the usual step indentation. Image
// variables carry the scan tag the image must also be tagged with, as a value GitHub expands in
// env: (P11): the tag with its variables mapped to expressions (snippetValue), or else the build
// step's output, after two comment lines that show the line the build step needs to write it.
// A tag whose build cannot be found in the file, or an action's tag, is rendered as written.
func (Plugin) Snippet(r plan.Repo, file string, g plan.Gate) string {
	var wf *workflow
	if len(g.Images) > 0 {
		if src, err := r.Read(file); err == nil {
			wf, _ = parseWorkflow(file, src)
		}
	}
	var env [][2]string
	notes := map[int][]string{} // env index: comment lines before its entry
	outputs := map[[2]int]string{}
	perStep := map[int]int{}
	for i, iv := range g.Images {
		value := shelledit.ShellValue(iv.ScanTag)
		if j, st, before, ok := snippetBuild(wf, iv.Build); ok {
			sv := st
			if iv.Build.Via != "" {
				sv = nil // the tag is in a file the step runs
			}
			if v, ok := snippetValue(iv.ScanTag, wf, j, sv, before); ok {
				value = v
			} else {
				at := [2]int{iv.Build.Pos.Step, iv.Build.Pos.Command}
				name, seen := outputs[at]
				if !seen {
					name = "shearbolt-image"
					if perStep[at[0]]++; perStep[at[0]] > 1 {
						name = fmt.Sprintf("shearbolt-image-%d", perStep[at[0]])
					}
					outputs[at] = name
				}
				id, give := st.id, ""
				if id == "" {
					id, give = "<id>", " (give it an id)"
				}
				value = fmt.Sprintf("${{ steps.%s.outputs.%s }}", id, name)
				notes[i] = []string{
					"# " + iv.Var + ": this tag is set in the script; in the build step" + give + ", add:",
					"#   " + outputEcho(name, iv.ScanTag),
				}
			}
		}
		env = append(env, [2]string{iv.Var, value})
	}
	item := gateLines(g, "", env, 2)
	if len(notes) > 0 {
		at := slices.Index(item, "env:")
		var out []string
		for k, l := range item {
			if c, ok := notes[k-at-1]; ok && k > at {
				for _, n := range c {
					out = append(out, "  "+n)
				}
			}
			out = append(out, l)
		}
		item = out
	}
	var b strings.Builder
	for i, l := range item {
		switch {
		case i == 0:
			b.WriteString("      - " + l)
		case l == "":
		default:
			b.WriteString("        " + l)
		}
		if i < len(item)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// snippetBuild finds run-step build b in workflow wf: its job, its step, and the step's commands
// ahead of it. It fails for an action build, and when wf is nil (the file could not be read) or
// does not hold the build.
func snippetBuild(wf *workflow, b plan.Build) (*job, *step, []shell.Command, bool) {
	if wf == nil || b.Pos.Command < 0 {
		return nil, nil, nil, false
	}
	j := wf.job(b.Pos.Job)
	if j == nil || b.Pos.Step < 0 || b.Pos.Step >= len(j.steps) {
		return nil, nil, nil, false
	}
	st := j.steps[b.Pos.Step]
	if st.run == nil || b.Pos.Command >= len(st.cmds) {
		return nil, nil, nil, false
	}
	return j, st, st.cmds[:b.Pos.Command], true
}

// githubDefaults are GitHub's default variables that snippetValue maps, with the github context
// values they hold; the env context does not have them.
var githubDefaults = map[string]string{
	"GITHUB_SHA":              "github.sha",
	"GITHUB_REPOSITORY":       "github.repository",
	"GITHUB_REPOSITORY_OWNER": "github.repository_owner",
	"GITHUB_REF_NAME":         "github.ref_name",
	"GITHUB_ACTOR":            "github.actor",
	"GITHUB_RUN_ID":           "github.run_id",
	"GITHUB_RUN_NUMBER":       "github.run_number",
}

// snippetValue returns scan tag tag, a shell word of step st in job j of workflow wf, as a value
// GitHub expands in the gate step's env: (P11), and false when it cannot. A ${{ … }} expression
// is text, as GitHub fills it in before the shell runs; quotes and escapes are removed. A plain
// $NAME or ${NAME} becomes ${{ github.* }} for one of githubDefaults, or ${{ env.NAME }} for a
// key of the workflow's or the job's env:, when the step's own env: does not set it (or cannot
// be read) and none of before, the step's commands ahead of the build, may assign it (assigns).
// Any other variable, any other expansion (${NAME,,}, ${NAME:-x}, $(…), backticks, $1), and a
// step that does not run in bash or sh fail. st is nil when the tag is in a file the step runs,
// whose assignments setup does not see: then no variable maps.
func snippetValue(tag string, wf *workflow, j *job, st *step, before []shell.Command) (string, bool) {
	cmds, _ := shell.Parse(tag, shell.State{})
	if len(cmds) != 1 || len(cmds[0].Words) != 1 || len(cmds[0].Redirs) > 0 || cmds[0].Depth > 0 {
		return "", false
	}
	raw := cmds[0].Words[0].Raw
	var b strings.Builder
	quoted := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case strings.HasPrefix(raw[i:], "${{"):
			end := strings.Index(raw[i:], "}}")
			if end < 0 {
				return "", false
			}
			b.WriteString(raw[i : i+end+2])
			i += end + 1
		case c == '"':
			quoted = !quoted
		case c == '\'' && !quoted:
			end := strings.IndexByte(raw[i+1:], '\'')
			if end < 0 {
				return "", false
			}
			b.WriteString(raw[i+1 : i+1+end])
			i += end + 1
		case c == '\\':
			if i+1 == len(raw) || raw[i+1] == '\n' {
				return "", false
			}
			if quoted && !strings.ContainsRune("$`\"\\", rune(raw[i+1])) {
				b.WriteByte('\\') // kept in double quotes
			}
			b.WriteByte(raw[i+1])
			i++
		case c == '`':
			return "", false
		case c == '$':
			name, n := plainVar(raw[i:])
			if n == 0 {
				return "", false
			}
			expr, ok := mapVar(name, wf, j, st, before)
			if !ok {
				return "", false
			}
			b.WriteString("${{ " + expr + " }}")
			i += n - 1
		default:
			b.WriteByte(c)
		}
	}
	if quoted {
		return "", false
	}
	return b.String(), true
}

// plainVar returns NAME and the length of the $NAME or ${NAME} that s starts with; 0 for
// anything else.
func plainVar(s string) (string, int) {
	braced := strings.HasPrefix(s, "${")
	from := 1
	if braced {
		from = 2
	}
	k := from
	for k < len(s) && (s[k] == '_' || s[k] >= 'a' && s[k] <= 'z' || s[k] >= 'A' && s[k] <= 'Z' || k > from && s[k] >= '0' && s[k] <= '9') {
		k++
	}
	switch {
	case k == from:
		return "", 0
	case !braced:
		return s[from:k], k
	case k < len(s) && s[k] == '}':
		return s[from:k], k + 1
	}
	return "", 0
}

// mapVar returns the GitHub expression that holds variable name where the build reads it (see
// snippetValue).
func mapVar(name string, wf *workflow, j *job, st *step, before []shell.Command) (string, bool) {
	if st == nil || !st.posix {
		return "", false
	}
	if own, ok := envNames(st.m); !ok || slices.Contains(own, name) {
		return "", false
	}
	if slices.ContainsFunc(before, func(c shell.Command) bool { return assigns(c, name) }) {
		return "", false
	}
	if e, ok := githubDefaults[name]; ok {
		return e, true
	}
	if slices.Contains(wf.env, name) || slices.Contains(j.env, name) {
		return "env." + name, true
	}
	return "", false
}

// assigns reports whether command c may assign variable name (P11): a NAME=… word, alone or
// before a command; export, declare, typeset, local, readonly, read, mapfile, readarray, unset,
// let, getopts or printf, or a for or select loop, that names it; a ${NAME=…} or ${NAME:=…}
// expansion; or eval, source or ., which may assign anything. A false match only gives the
// step-output advice, which is always right.
func assigns(c shell.Command, name string) bool {
	if slices.Contains(c.Prefix(), name) {
		return true
	}
	for _, w := range c.Words {
		if strings.Contains(w.Raw, "${"+name+"=") || strings.Contains(w.Raw, "${"+name+":=") {
			return true
		}
	}
	argv := builtinArgv(c)
	if len(argv) == 0 {
		return false
	}
	switch argv[0] {
	case "eval", "source", ".":
		return true
	case "export", "declare", "typeset", "local", "readonly", "read", "mapfile", "readarray", "unset",
		"let", "getopts", "printf", "for", "select":
		return slices.ContainsFunc(argv[1:], func(a string) bool { return a == name || varName(a) == name })
	}
	return false
}

// outputEcho returns the line that writes scan tag tag, a shell word, to the step output name:
// the tag inside the double quotes when it means the same there, else as written after them.
func outputEcho(name, tag string) string {
	if v, ok := inDoubleQuotes(tag); ok {
		return `echo "` + name + "=" + v + `" >> "$GITHUB_OUTPUT"`
	}
	return `echo "` + name + `="` + tag + ` >> "$GITHUB_OUTPUT"`
}

// inDoubleQuotes returns shell word w as text with the same value inside double quotes: w
// without its quotes when it is one double-quoted string, w itself when it holds no quote or
// backslash; false otherwise.
func inDoubleQuotes(w string) (string, bool) {
	if !strings.ContainsAny(w, `"'\`) {
		return w, true
	}
	if len(w) < 2 || w[0] != '"' || w[len(w)-1] != '"' {
		return "", false
	}
	inner := w[1 : len(w)-1]
	for i := 0; i < len(inner); i++ {
		switch {
		case strings.HasPrefix(inner[i:], "${{"):
			end := strings.Index(inner[i:], "}}")
			if end < 0 {
				return "", false
			}
			i += end + 1
		case inner[i] == '\\' && i+1 < len(inner):
			i++
		case inner[i] == '\\', inner[i] == '"':
			return "", false
		}
	}
	return inner, true
}

// gateLines renders a gate step relative to its item's content column: name, if, uses, env,
// with (working-directory when the project is not the root, targets when the gate lists them).
func gateLines(g plan.Gate, ifCond string, env [][2]string, unit int) []string {
	pad := strings.Repeat(" ", unit)
	out := []string{"name: shearbolt"}
	if ifCond != "" {
		out = append(out, "if: "+yamledit.Scalar(ifCond))
	}
	out = append(out, "uses: "+gateUses)
	if len(env) > 0 {
		out = append(out, "env:")
		for _, e := range env {
			out = append(out, pad+e[0]+": "+yamledit.Scalar(e[1]))
		}
	}
	var with []string
	if g.Project.Dir != "" && g.Project.Dir != "." {
		with = append(with, pad+"working-directory: "+yamledit.Scalar(g.Project.Dir))
	}
	if g.Targets != nil {
		with = append(with, pad+"targets: "+yamledit.Scalar(strings.Join(g.Targets, " ")))
	}
	if len(with) > 0 {
		out = append(append(out, "with:"), with...)
	}
	return out
}
