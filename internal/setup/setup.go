// Package setup implements `shearbolt setup`: it finds the npm and Rust projects in a repository
// and edits its CI so a Shearbolt gate runs after each build and before any push or deploy.
//
// Compute is a pure function of the repository and the answers: it returns a plan.Plan with the
// questions, every file change (full content plus diff) and notes, and writes nothing. Write
// applies a plan. Every mode of the command shares this one code path.
package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/scan"
	"github.com/klar-web-services/shearbolt/internal/setup/ci/bitbucket"
	"github.com/klar-web-services/shearbolt/internal/setup/ci/github"
	"github.com/klar-web-services/shearbolt/internal/setup/ci/gitlab"
	"github.com/klar-web-services/shearbolt/internal/setup/ecosystem/npm"
	"github.com/klar-web-services/shearbolt/internal/setup/ecosystem/rust"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

// ecosystems are the project kinds setup supports, in recognition order.
var ecosystems = []plan.Ecosystem{npm.Plugin{}, rust.Plugin{}}

// cis are the CI systems setup edits.
var cis = []plan.CI{github.Plugin{}, bitbucket.Plugin{}, gitlab.Plugin{}}

// Options are the inputs of Compute.
type Options struct {
	Root    string            // the repository root (the working directory)
	Answers map[string]string // --answer values by question ID
	Yes     bool              // accept the defaults of unanswered questions (the plan is then complete)
	Version string            // shearbolt's version, for install pins; "dev" leaves them out
}

// noCIMessage is NO_CI's message (R9).
const noCIMessage = "Shearbolt is a CI gate: it runs in GitHub Actions, Bitbucket Pipelines or GitLab CI after your build and before you push or deploy. No CI configuration was found here (.github/workflows/*.yml or *.yaml, bitbucket-pipelines.yml, .gitlab-ci.yml), so there is nothing to add it to. Run setup from the repository root, or add a pipeline that builds your project, then run setup again. Meanwhile `shearbolt scan --source .` checks your lockfiles locally."

// noProjectMessage is NO_PROJECT's message when setup finds nothing to protect. The find notes
// follow it.
const noProjectMessage = "No npm or Rust project with a lockfile, no Docker Compose file that pulls images and no Docker image build in the CI files was found here, so there is nothing for Shearbolt to protect. Run setup from the repository root. (Setup does not look for Docker builds inside scripts the CI calls.)"

// noDeploySeenMessage is NO_DEPLOY_SEEN's message, for a project with compose targets when no CI
// job setup read pushes or deploys.
const noDeploySeenMessage = "Setup saw no push or deploy step in the CI jobs it could read. If a platform deploys this project on its own (for example on every push), a failed shearbolt check does not stop it: require the check before merging to the deployed branch, or trigger the deploy from CI after the gate."

// alreadyExcluded are the directories the scan itself never descends into, so a source target
// does not need to exclude them (R1).
var alreadyExcluded = []string{"node_modules", ".git", "target", ".shearbolt"}

// Compute plans setup for the repository at o.Root with the registered plugins. It never writes:
// errors go into Plan.Error, and changes are computed with the given answers and the defaults
// of the other questions.
func Compute(o Options) *plan.Plan { return compute(o, ecosystems, cis) }

// ComputeWith is Compute with the given CI plugins in place of the registered ones, so tests of
// the parts CI plugins share can run them end to end.
func ComputeWith(o Options, cis ...plan.CI) *plan.Plan { return compute(o, ecosystems, cis) }

// ciSource is one CI file with its plugin, content and jobs.
type ciSource struct {
	ci   plan.CI
	path string
	src  []byte
	jobs []plan.Job
}

func compute(o Options, ecos []plan.Ecosystem, cis []plan.CI) *plan.Plan {
	p := &plan.Plan{SchemaVersion: plan.SchemaVersion, ShearboltVersion: o.Version}
	fail := func(code, msg, file string) *plan.Plan {
		p.Error = &plan.Note{Code: code, Message: msg, File: file}
		return p
	}
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return fail("READ", err.Error(), "")
	}
	r := plan.Repo{Root: root}

	// 1. Projects (R1) and their source excludes.
	projects, findNotes, err := findProjects(r, ecos)
	if err != nil {
		return fail("READ", err.Error(), "")
	}
	p.Projects = projects

	// 2. CI files and their jobs.
	classify := newClassifier(r, ecos)
	var files []*ciSource
	var notes []plan.Note
	var parseErr *plan.Note
	for _, ci := range cis {
		for _, f := range ci.Files(r) {
			p.CI = append(p.CI, plan.CIFile{System: ci.Name(), File: f})
			src, err := r.Read(f)
			if err != nil {
				if parseErr == nil {
					parseErr = &plan.Note{Code: "READ", Message: fmt.Sprintf("%s cannot be read: %v", f, err), File: f}
				}
				continue
			}
			jobs, jn, err := ci.Jobs(r, f, src, classify)
			if err != nil {
				if parseErr == nil {
					parseErr = &plan.Note{Code: "PARSE", Message: fmt.Sprintf("%s is not valid YAML: %v", f, err), File: f}
				}
				continue
			}
			files = append(files, &ciSource{ci: ci, path: f, src: src, jobs: jobs})
			notes = append(notes, pinSnippets(r, ci, f, o.Version, jn)...)
		}
	}
	slices.SortFunc(p.CI, func(a, b plan.CIFile) int { return strings.Compare(a.File, b.File) })
	slices.SortFunc(files, func(a, b *ciSource) int { return strings.Compare(a.path, b.path) })

	// Without an npm or Rust project, the image builds of the jobs make the root project.
	var jobs []plan.Job
	var paths []string
	for _, f := range files {
		jobs = append(jobs, f.jobs...)
		paths = append(paths, f.path)
	}
	projects = attachImageBuilds(projects, jobs, root)
	p.Projects = projects

	// 3. Errors. With nothing to protect, a CI file that was not read might hold an image build.
	if len(projects) == 0 {
		if parseErr != nil {
			p.Error = parseErr
			return p
		}
		msg := noProjectMessage
		for _, n := range findNotes {
			msg += "\n" + n.Message
		}
		return fail("NO_PROJECT", msg, "")
	}
	if len(p.CI) == 0 {
		return fail("NO_CI", noCIMessage, "")
	}
	if parseErr != nil {
		p.Error = parseErr
		return p
	}
	notes = append(slices.Clone(findNotes), notes...)

	// 4. Questions (R3).
	m := newGateModel(projects, jobs, paths)
	p.Questions = m.questions()
	for i := range p.Questions {
		p.Questions[i].Answer = o.Answers[p.Questions[i].ID]
	}

	// 5. Answers: the given ones, defaults for the rest.
	values, missing, err := plan.Resolve(p.Questions, o.Answers, o.Yes)
	if err != nil {
		return fail("BAD_ANSWER", err.Error(), "")
	}
	p.Complete = len(missing) == 0

	// 6. Placement (R3, R4).
	byPath := map[string]*ciSource{}
	for _, f := range files {
		byPath[f.path] = f
	}
	snippet := func(file string, g plan.Gate) string { return byPath[file].ci.Snippet(r, file, g) }
	pl := m.place(values, o.Version, snippet)
	notes = append(notes, pl.notes...)
	// NO_GATE_PLACED snippets carry the version already; a development build's are unpinned.
	if slices.ContainsFunc(pl.notes, func(n plan.Note) bool { return n.Code == "NO_GATE_PLACED" }) {
		for _, f := range files {
			if unpinnedSnippet(r, f.ci, f.path, o.Version) {
				notes = append(notes, plan.UnpinnedNote(f.path))
			}
		}
	}

	// 7-9. Planned targets, configs, then each gate's targets and image variables (R4 rule 8, R5).
	// A project whose config has a compose target gets NO_DEPLOY_SEEN when no job pushes or
	// deploys: a platform that deploys on its own is not stopped by the gate.
	commit := func(file string) string {
		if f := byPath[file]; f != nil {
			return commitExprs[f.ci.Name()]
		}
		return ""
	}
	pushed := false
	for j := range jobs {
		pushed = pushed || m.firstPush(j) != never
	}
	var changes []plan.FileChange
	var gates []plan.Gate
	var selected []plan.Project
	usedTags := map[string]map[string]bool{} // scan tags per job, unique across projects
	for _, proj := range projects {
		pp := pl.projects[proj.Dir]
		if pp == nil {
			continue
		}
		selected = append(selected, proj)
		ch, merged, names, vars, cn := configChange(r, proj, pp.targets)
		if ch != nil {
			changes = append(changes, *ch)
		}
		notes = append(notes, cn...)
		gates = append(gates, pp.finish(merged, names, vars, commit, usedTags)...)
		ts := merged
		if merged == nil {
			ts = pp.targets
		}
		if !pushed && slices.ContainsFunc(ts, func(t config.Target) bool { return t.Kind() == "compose" }) {
			notes = append(notes, plan.Note{Code: "NO_DEPLOY_SEEN", Message: noDeploySeenMessage, File: configFile(proj)})
		}
	}

	// 10. Prepare every build attributed to a selected project. The CI-file parts of a
	// non-RewriteOnly build's Prep go to its CI plugin, per job; Via-file inserts are the core's.
	preps := map[string][]plan.Prep{}
	var via []viaInsert
	for _, proj := range selected {
		eco := ecosystemOf(ecos, proj.Ecosystem)
		if eco == nil {
			continue
		}
		for _, b := range pl.attributed[proj.Dir] {
			prep, pn := eco.Prepare(r, proj, b)
			for _, n := range pn {
				if n.File == "" {
					n.File, n.Line = b.Pos.File, b.Pos.Line
				}
				notes = append(notes, n)
			}
			for _, in := range prep.Inserts {
				if in.Via != "" {
					via = append(via, viaInsert{Insert: in, job: b.Pos.Job, reached: !b.RewriteOnly})
				}
			}
			if b.RewriteOnly {
				continue
			}
			prep.Pos = b.Pos
			prep.Inserts = slices.DeleteFunc(slices.Clone(prep.Inserts), func(in plan.Insert) bool { return in.Via != "" })
			if len(prep.Inserts) > 0 || prep.Install != nil {
				preps[b.Pos.Job] = append(preps[b.Pos.Job], prep)
			}
		}
	}

	// 11. CI edits, called-file inserts and ignore files.
	failed := map[string]bool{}
	for _, f := range files {
		var fg []plan.Gate
		for _, g := range gates {
			if strings.HasPrefix(g.Job, f.path+"#") && slices.ContainsFunc(f.jobs, func(j plan.Job) bool { return j.ID == g.Job }) {
				fg = append(fg, g)
			}
		}
		fp := map[string][]plan.Prep{}
		for _, j := range f.jobs {
			if ps := preps[j.ID]; len(ps) > 0 {
				fp[j.ID] = ps
			}
		}
		if len(fg) == 0 && len(fp) == 0 {
			continue
		}
		ed, err := f.ci.Edit(r, f.path, f.src, f.jobs, fg, fp)
		if err != nil {
			msg := fmt.Sprintf("%s could not be edited (%v); add the gate by hand", f.path, err)
			if len(fg) > 0 {
				msg += " to each job that builds, after the build and before any push or deploy:"
				for _, g := range fg {
					msg += "\n" + g.Job + ":\n" + f.ci.Snippet(r, f.path, g)
				}
			}
			notes = append(notes, plan.Note{Code: "UNSUPPORTED_EDIT", Message: msg, File: f.path})
			for _, j := range f.jobs {
				failed[j.ID] = true
			}
			continue
		}
		notes = append(notes, ed.Notes...)
		for _, id := range ed.Failed {
			failed[id] = true
		}
		if !bytes.Equal(ed.Src, f.src) {
			changes = append(changes, plan.FileChange{Path: f.path, Action: "modify", Content: string(ed.Src), Base: f.src})
		}
	}
	vc, vn := viaChanges(r, via, failed)
	changes = append(changes, vc...)
	notes = append(notes, vn...)
	var gated []plan.Build
	for _, g := range gates {
		gated = append(gated, g.Builds...)
	}
	changes = append(changes, ignoreChanges(r, selected, gated)...)

	// 12. Diffs, bases and modes.
	slices.SortStableFunc(changes, func(a, b plan.FileChange) int { return strings.Compare(a.Path, b.Path) })
	for i := range changes {
		c := &changes[i]
		if i > 0 && changes[i-1].Path == c.Path {
			return fail("READ", fmt.Sprintf("setup planned two changes to %s; this is a bug, please report it", c.Path), c.Path)
		}
		if c.Action == "create" {
			c.Base, c.Mode = nil, 0o644
			c.Diff = plan.UnifiedDiff(c.Path, nil, []byte(c.Content))
			continue
		}
		c.Diff = plan.UnifiedDiff(c.Path, c.Base, []byte(c.Content))
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(c.Path)))
		if err != nil {
			return fail("READ", err.Error(), c.Path)
		}
		c.Mode = info.Mode().Perm()
	}
	p.Changes = changes
	p.Notes = dedupeNotes(notes)
	return p
}

// pinSnippets re-renders the snippets in a CI file's Jobs notes, which plugins render for
// plan.Gate{} because Jobs gets no version, for a gate that carries version, so their install
// lines are pinned like setup's own edits. When a development build leaves such a snippet
// unpinned, the file gets UNPINNED_VERSION.
func pinSnippets(r plan.Repo, ci plan.CI, file, version string, notes []plan.Note) []plan.Note {
	bare := ci.Snippet(r, file, plan.Gate{})
	versioned := ci.Snippet(r, file, plan.Gate{Version: version})
	out := slices.Clone(notes)
	used := false
	for i := range out {
		if strings.Contains(out[i].Message, bare) {
			out[i].Message = strings.ReplaceAll(out[i].Message, bare, versioned)
			used = true
		}
	}
	if used && unpinnedSnippet(r, ci, file, version) {
		out = append(out, plan.UnpinnedNote(file))
	}
	return out
}

// unpinnedSnippet reports whether ci's snippet for file holds an install line that version
// leaves unpinned: the snippet depends on the version, but not on this one (a development build).
func unpinnedSnippet(r plan.Repo, ci plan.CI, file, version string) bool {
	bare := ci.Snippet(r, file, plan.Gate{})
	return ci.Snippet(r, file, plan.Gate{Version: version}) == bare && ci.Snippet(r, file, plan.Gate{Version: "1.0.0"}) != bare
}

// dedupeNotes drops exact repeats, such as the same note about a build in a called file that
// two jobs run.
func dedupeNotes(notes []plan.Note) []plan.Note {
	seen := map[plan.Note]bool{}
	var out []plan.Note
	for _, n := range notes {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// findProjects runs the ecosystems (R1), sorts their projects by directory and sets each
// project's source excludes. When two ecosystems find a project in the same directory, the first
// one's is kept: they would share one shearbolt.yaml.
func findProjects(r plan.Repo, ecos []plan.Ecosystem) ([]plan.Project, []plan.Note, error) {
	var projects []plan.Project
	var notes []plan.Note
	for _, e := range ecos {
		ps, ns, err := e.Find(r)
		if err != nil {
			return nil, nil, err
		}
		notes = append(notes, ns...)
		for _, p := range ps {
			if i := slices.IndexFunc(projects, func(q plan.Project) bool { return q.Dir == p.Dir }); i >= 0 {
				notes = append(notes, plan.Note{
					Code: "DUPLICATE_PROJECT",
					Message: fmt.Sprintf("%s holds both a %s and a %s project; setup protects it as the %s project, whose source target scans every lockfile in the directory",
						p.Dir, projects[i].Ecosystem, p.Ecosystem, projects[i].Ecosystem),
				})
				continue
			}
			projects = append(projects, p)
		}
	}
	slices.SortStableFunc(projects, func(a, b plan.Project) int { return strings.Compare(a.Dir, b.Dir) })
	files, cn, err := findCompose(r)
	if err != nil {
		return nil, nil, err
	}
	notes = append(notes, cn...)
	projects = attachCompose(projects, files, r.Root)
	if len(projects) == 0 {
		return nil, notes, nil
	}

	var skipped []string
	err = r.Walk(func(string, []string) {}, func(dir string) {
		if slices.Contains(alreadyExcluded, path.Base(dir)) {
			return
		}
		if found, err := scan.FindLockfiles(filepath.Join(r.Root, filepath.FromSlash(dir)), nil); err == nil && len(found) > 0 {
			skipped = append(skipped, dir)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	for i := range projects {
		p := &projects[i]
		var ex []string
		for _, q := range projects {
			if q.Dir != p.Dir {
				if rel, ok := insideDir(p.Dir, q.Dir); ok {
					ex = append(ex, rel)
				}
			}
		}
		for _, d := range skipped {
			if rel, ok := insideDir(p.Dir, d); ok {
				ex = append(ex, rel)
			}
		}
		p.Excludes = topmost(ex)
	}
	return projects, notes, nil
}

// insideDir returns dir relative to base, when dir is strictly inside base.
func insideDir(base, dir string) (string, bool) {
	if base == "." {
		return dir, dir != "."
	}
	rel, ok := strings.CutPrefix(dir, base+"/")
	return rel, ok && rel != ""
}

// topmost returns the directories that are not inside another one of them, sorted, each with a
// trailing "/".
func topmost(dirs []string) []string {
	slices.Sort(dirs)
	var out []string
	for _, d := range slices.Compact(dirs) {
		if !slices.ContainsFunc(dirs, func(o string) bool { return strings.HasPrefix(d, o+"/") }) {
			out = append(out, d+"/")
		}
	}
	return out
}

func ecosystemOf(ecos []plan.Ecosystem, name string) plan.Ecosystem {
	for _, e := range ecos {
		if e.Name() == name {
			return e
		}
	}
	return nil
}

// viaInsert is an Insert into a called file, with the job of the build that needs it. reached
// is false for a RewriteOnly build (not run by that job).
type viaInsert struct {
	plan.Insert
	job     string
	reached bool
}

// viaChanges applies the called-file inserts, once per file and offset. An insert that a
// non-RewriteOnly build of a failed job reaches is dropped: that job got no install, so the
// rewritten command would fail there.
func viaChanges(r plan.Repo, ins []viaInsert, failed map[string]bool) ([]plan.FileChange, []plan.Note) {
	type key struct {
		file   string
		offset int
	}
	blocked := map[key]bool{}
	for _, in := range ins {
		if in.reached && failed[in.job] {
			blocked[key{in.Via, in.Offset}] = true
		}
	}
	byFile := map[string][]plan.Insert{}
	seen := map[key]bool{}
	for _, in := range ins {
		k := key{in.Via, in.Offset}
		if blocked[k] || seen[k] {
			continue
		}
		seen[k] = true
		byFile[in.Via] = append(byFile[in.Via], in.Insert)
	}
	var out []plan.FileChange
	var notes []plan.Note
	for _, file := range slices.Sorted(maps.Keys(byFile)) {
		src, err := r.Read(file)
		if err != nil {
			notes = append(notes, plan.Note{Code: "UNSUPPORTED_EDIT", Message: fmt.Sprintf("%s could not be read again (%v), so its cargo builds were not rewritten to cargo auditable build", file, err), File: file})
			continue
		}
		list := byFile[file]
		slices.SortFunc(list, func(a, b plan.Insert) int { return b.Offset - a.Offset })
		text := string(src)
		ok := true
		for _, in := range list {
			if in.Offset < 0 || in.Offset > len(text) {
				ok = false
				break
			}
			text = text[:in.Offset] + in.Text + text[in.Offset:]
		}
		if !ok {
			notes = append(notes, plan.Note{Code: "UNSUPPORTED_EDIT", Message: fmt.Sprintf("%s changed while setup read it, so its cargo builds were not rewritten", file), File: file})
			continue
		}
		out = append(out, plan.FileChange{Path: file, Action: "modify", Content: text, Base: src})
	}
	return out, notes
}

// Write applies plan p to the repository at root. It first checks every file: a file to modify
// must still equal the content the plan was computed from, and a file to create must still be
// missing; otherwise it writes nothing. Each file is written to a temporary file in its own
// directory and renamed into place, keeping its mode (new files get 0644); missing parent
// directories are created.
func Write(root string, p *plan.Plan) error {
	if p == nil || p.Error != nil {
		return errors.New("setup: the plan has an error, so nothing is written")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	r := plan.Repo{Root: abs}
	for _, c := range p.Changes {
		if c.Path == "" || plan.CleanDir(c.Path) != c.Path {
			return fmt.Errorf("setup: %q is not a path inside the repository", c.Path)
		}
		cur, err := r.Read(c.Path)
		switch {
		case c.Action == "create" && !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("setup: %s was created after the plan was computed; run setup again", c.Path)
		case c.Action == "modify" && err != nil:
			return fmt.Errorf("setup: %s: %w; run setup again", c.Path, err)
		case c.Action == "modify" && !bytes.Equal(cur, c.Base):
			return fmt.Errorf("setup: %s changed after the plan was computed; run setup again", c.Path)
		case c.Action != "create" && c.Action != "modify":
			return fmt.Errorf("setup: unknown action %q for %s", c.Action, c.Path)
		}
		if err := insideRoot(abs, path.Dir(c.Path)); err != nil {
			return err
		}
	}
	for _, c := range p.Changes {
		if err := writeFile(filepath.Join(abs, filepath.FromSlash(c.Path)), []byte(c.Content), c.Mode); err != nil {
			return fmt.Errorf("setup: writing %s: %w", c.Path, err)
		}
	}
	return nil
}

// insideRoot checks that the deepest existing ancestor of dir (repo-relative) resolves inside
// root, so a symlinked directory cannot redirect a write outside the repository.
func insideRoot(root, dir string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	for d := dir; ; d = path.Dir(d) {
		full := filepath.Join(root, filepath.FromSlash(d))
		if real, err := filepath.EvalSymlinks(full); err == nil {
			rel, err := filepath.Rel(realRoot, real)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return fmt.Errorf("setup: %s resolves outside the repository", dir)
			}
			return nil
		}
		if d == "." {
			return nil
		}
	}
}

func writeFile(full string, content []byte, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".shearbolt-setup-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op after the rename
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode.Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, full)
}
