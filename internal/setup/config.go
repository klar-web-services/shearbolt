package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

// This file writes R6: a new shearbolt.yaml per selected project, or the missing targets
// appended to an existing one, which is never rewritten otherwise.

// configHeader is the first line of a generated shearbolt.yaml.
const configHeader = "# Shearbolt deployment gate: https://github.com/klar-web-services/shearbolt#readme"

// configFile returns the repo-relative path of project p's shearbolt.yaml.
func configFile(p plan.Project) string { return path.Join(p.Dir, "shearbolt.yaml") }

// imageVar matches an image reference that is one variable, with or without a default:
// ${SHEARBOLT_IMAGE:-web:local}. The CI sets that variable to the tag the job built.
var imageVar = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-[^}]*)?\}$`)

// imageVarOf returns the variable of an image reference written as ${VAR:-default}, or "".
func imageVarOf(image string) string {
	if m := imageVar.FindStringSubmatch(image); m != nil {
		return m[1]
	}
	return ""
}

// configChange plans project p's shearbolt.yaml for the planned targets (R6).
//
//   - Without a file, it renders a new one: image targets, then file targets, then compose
//     targets, then source, with COMPOSE_ADDED when it holds compose targets.
//   - With one, it appends the targets that are not present after the last item of the block
//     targets sequence, and touches nothing else. A planned target is present when an existing
//     target has the same kind and value (its name is then used everywhere), or the same name:
//     an image target whose reference is one ${VAR:-...} supplies VAR; any other value under
//     that name gets TARGET_EXISTS (plus IMAGE_REF_FIXED for an image) and is left alone. A
//     target of another kind under the planned name is no match: the planned target is appended
//     under a free name, with TARGET_EXISTS, so what the job builds is still scanned.
//   - Compose targets are written only into a new file. With an existing one, a planned compose
//     target is present when a compose target scans the same file (its name is then used);
//     otherwise COMPOSE_NOT_GATED carries the target to add, so a target the user removed is
//     not added back and the gates name no compose target the file lacks.
//   - A file that cannot be read, parsed or edited gets UNSUPPORTED_EDIT with the targets to add
//     by hand, and no change; merged is then nil, so the gates scan every target the file has.
//
// merged is the file's targets after the change (raw values, variables not expanded). names
// maps each planned name to the name in the file. vars maps each image target in merged whose
// reference is ${VAR:-...} to VAR; an image target without an entry has a fixed reference.
// change is nil when nothing changes. Its Mode is left for the caller.
func configChange(r plan.Repo, p plan.Project, planned []config.Target) (change *plan.FileChange, merged []config.Target, names map[string]string, vars map[string]string, notes []plan.Note) {
	file := configFile(p)
	planned = sortTargets(normalizeTargets(planned))
	listed := planned // the targets an UNSUPPORTED_EDIT snippet lists
	unsupported := func(reason string) (*plan.FileChange, []config.Target, map[string]string, map[string]string, []plan.Note) {
		names, vars := map[string]string{}, map[string]string{}
		for _, t := range planned {
			names[t.Name] = t.Name
		}
		addVars(vars, planned)
		notes := []plan.Note{{
			Code: "UNSUPPORTED_EDIT",
			Message: fmt.Sprintf("%s could not be written automatically (%s); add these targets to its targets list by hand:\n%s",
				file, reason, strings.Join(targetItems(listed, "  "), "\n")),
			File: file,
		}}
		// Compose targets left out of the snippet get their own note each.
		for _, t := range planned {
			if t.Kind() == "compose" && !slices.ContainsFunc(listed, func(l config.Target) bool { return l.Name == t.Name }) {
				notes = append(notes, composeNotGated(t, file))
			}
		}
		return nil, nil, names, vars, notes
	}
	src, err := r.Read(file)
	if errors.Is(err, fs.ErrNotExist) {
		content := renderConfig(p, planned)
		if !readsBack([]byte(content), planned) {
			return unsupported("the generated file does not read back as intended")
		}
		names, vars = map[string]string{}, map[string]string{}
		for _, t := range planned {
			names[t.Name] = t.Name
		}
		addVars(vars, planned)
		var files []string
		for _, t := range planned {
			if t.Kind() == "compose" {
				files = append(files, t.Compose)
			}
		}
		if len(files) > 0 {
			notes = append(notes, plan.Note{
				Code: "COMPOSE_ADDED",
				Message: fmt.Sprintf("setup gates the images of %s in %s. A compose file that only runs locally or in tests ships nothing: remove its target, which setup will not add back, or exclude single services with exclude: [service]. After editing the targets, run shearbolt setup again, so every gate scans them (or stops naming a removed one).",
					strings.Join(files, ", "), file),
				File: file,
			})
		}
		return &plan.FileChange{
			Path:    file,
			Action:  "create",
			Content: content,
			Diff:    plan.UnifiedDiff(file, nil, []byte(content)),
			Mode:    0o644,
		}, planned, names, vars, notes
	}
	// Compose targets are create-only: an existing file's by-hand snippet never lists them.
	listed = slices.DeleteFunc(slices.Clone(planned), func(t config.Target) bool { return t.Kind() == "compose" })
	if err != nil {
		return unsupported("it cannot be read: " + err.Error())
	}
	doc, err := yamledit.Parse(src)
	if err != nil {
		return unsupported("it is not valid YAML: " + err.Error())
	}
	root := doc.Root()
	if root == nil || root.Kind != yaml.MappingNode {
		return unsupported("it is not a YAML mapping")
	}
	seq := doc.Get(root, "targets")
	if seq == nil || seq.Kind != yaml.SequenceNode || seq.Style&yaml.FlowStyle != 0 || len(seq.Content) == 0 {
		return unsupported("it has no block-style targets list")
	}
	var existing []config.Target
	if err := seq.Decode(&existing); err != nil {
		return unsupported("its targets cannot be read: " + err.Error())
	}

	names, vars = map[string]string{}, map[string]string{}
	taken := map[string]bool{}
	for _, e := range existing {
		taken[e.Name] = true
	}
	var add []config.Target
	for _, t := range planned {
		if t.Kind() == "compose" {
			if i := slices.IndexFunc(existing, func(e config.Target) bool { return sameValue(e, t) }); i >= 0 {
				names[t.Name] = existing[i].Name
				continue
			}
			notes = append(notes, composeNotGated(t, file))
			continue
		}
		if i := slices.IndexFunc(existing, func(e config.Target) bool { return sameValue(e, t) && e.Name == t.Name }); i >= 0 {
			names[t.Name] = existing[i].Name
			continue
		}
		if i := slices.IndexFunc(existing, func(e config.Target) bool { return sameValue(e, t) }); i >= 0 {
			names[t.Name] = existing[i].Name
			continue
		}
		i := slices.IndexFunc(existing, func(e config.Target) bool { return e.Name == t.Name })
		switch {
		case i < 0:
			names[t.Name] = uniqueName(t.Name, taken) // taken only by an earlier rename
			t.Name = names[t.Name]
			add = append(add, t)
		case existing[i].Kind() == t.Kind():
			e := existing[i]
			names[t.Name] = e.Name
			if t.Kind() == "image" && imageVarOf(e.Image) != "" {
				continue
			}
			notes = append(notes, plan.Note{
				Code: "TARGET_EXISTS",
				Message: fmt.Sprintf("target %s in %s already exists with %s %s, so setup left it alone instead of setting %s %s",
					e.Name, file, e.Kind(), e.Ref(), t.Kind(), t.Ref()),
				File: file,
			})
			if t.Kind() == "image" {
				notes = append(notes, plan.Note{
					Code: "IMAGE_REF_FIXED",
					Message: fmt.Sprintf("target %s in %s uses a fixed image reference, so the gate scans that reference, not the image this job built; change it to `%s` and run setup again.",
						e.Name, file, t.Image),
					File: file,
				})
			}
		default:
			e := existing[i]
			n := uniqueName(t.Name, taken)
			notes = append(notes, plan.Note{
				Code: "TARGET_EXISTS",
				Message: fmt.Sprintf("target %s in %s already exists as a %s target, so setup adds the %s target %s as %s",
					e.Name, file, e.Kind(), t.Kind(), t.Ref(), n),
				File: file,
			})
			names[t.Name] = n
			t.Name = n
			add = append(add, t)
		}
	}
	merged = append(slices.Clone(existing), add...)
	addVars(vars, merged)
	if len(add) == 0 {
		return nil, merged, names, vars, notes
	}

	// Inserts at one point apply in queue order, so the items keep theirs.
	for _, t := range add {
		if err := doc.InsertItem(seq, len(seq.Content)-1, targetLines(t)); err != nil {
			return unsupported(err.Error())
		}
	}
	out, err := doc.Result()
	if err != nil {
		return unsupported(err.Error())
	}
	if !readsBack(out, merged) {
		return unsupported("the edited file does not read back as intended")
	}
	return &plan.FileChange{
		Path:    file,
		Action:  "modify",
		Content: string(out),
		Diff:    plan.UnifiedDiff(file, src, out),
		Base:    src,
	}, merged, names, vars, notes
}

// normalizeTargets returns a copy of ts whose source excludes are sorted, deduplicated and free
// of entries config.Load would reject as globs (leaving one out scans more, never less).
func normalizeTargets(ts []config.Target) []config.Target {
	out := slices.Clone(ts)
	for i := range out {
		if len(out[i].Exclude) == 0 {
			continue
		}
		var ex []string
		for _, e := range out[i].Exclude {
			if e != "" && !strings.ContainsAny(e, "*?[{\\$") {
				ex = append(ex, e)
			}
		}
		slices.Sort(ex)
		out[i].Exclude = slices.Compact(ex)
	}
	return out
}

// sortTargets returns ts in R6 order: image targets, then dir and file targets, then compose
// targets, then source, keeping the order within each kind.
func sortTargets(ts []config.Target) []config.Target {
	rank := map[string]int{"image": 0, "dir": 1, "file": 2, "compose": 3, "source": 4}
	out := slices.Clone(ts)
	slices.SortStableFunc(out, func(a, b config.Target) int { return rank[a.Kind()] - rank[b.Kind()] })
	return out
}

func addVars(vars map[string]string, ts []config.Target) {
	for _, t := range ts {
		if v := imageVarOf(t.Image); t.Kind() == "image" && v != "" {
			vars[t.Name] = v
		}
	}
}

// sameValue reports whether two targets scan the same thing: the same kind and value. Paths are
// compared cleaned, so ./target/release/demo matches target/release/demo.
func sameValue(a, b config.Target) bool {
	if a.Kind() != b.Kind() || a.Kind() == "" {
		return false
	}
	if a.Kind() == "image" {
		return a.Image == b.Image
	}
	return path.Clean(a.Ref()) == path.Clean(b.Ref())
}

// uniqueName returns name, or name-2, name-3, ... when it is taken, and marks the result taken.
func uniqueName(name string, taken map[string]bool) string {
	n := name
	for k := 2; taken[n]; k++ {
		n = fmt.Sprintf("%s-%d", name, k)
	}
	taken[n] = true
	return n
}

// renderConfig renders a new shearbolt.yaml (R6) with targets in the given order. No policy or
// exceptions: the defaults apply.
func renderConfig(p plan.Project, targets []config.Target) string {
	project := p.Name
	if strings.TrimSpace(project) == "" {
		project = shortName(p)
	}
	lines := []string{configHeader, "version: 1", "project: " + yamledit.Scalar(project), "targets:"}
	lines = append(lines, targetItems(targets, "  ")...)
	return strings.Join(lines, "\n") + "\n"
}

// targetItems renders targets as block sequence items with the dash at indent.
func targetItems(ts []config.Target, indent string) []string {
	var out []string
	for _, t := range ts {
		for i, l := range targetLines(t) {
			prefix := indent + "  "
			if i == 0 {
				prefix = indent + "- "
			}
			out = append(out, prefix+l)
		}
	}
	return out
}

// targetLines renders one target's keys, relative to its item's content column.
func targetLines(t config.Target) []string {
	out := []string{"name: " + yamledit.Scalar(t.Name)}
	switch t.Kind() {
	case "image":
		out = append(out, "image: "+yamledit.Scalar(t.Image))
	case "dir":
		out = append(out, "dir: "+yamledit.Scalar(t.Dir))
	case "file":
		out = append(out, "file: "+yamledit.Scalar(t.File))
	case "compose":
		out = append(out, "compose: "+yamledit.Scalar(t.Compose))
	case "source":
		out = append(out, "source: "+yamledit.Scalar(t.Source))
	}
	if len(t.Exclude) > 0 {
		ex := make([]string, len(t.Exclude))
		for i, e := range t.Exclude {
			ex[i] = flowScalar(e)
		}
		out = append(out, "exclude: ["+strings.Join(ex, ", ")+"]")
	}
	if t.LockfileFallback != "" {
		out = append(out, "lockfileFallback: "+yamledit.Scalar(t.LockfileFallback))
	}
	return out
}

// flowScalar renders s for a flow sequence: plain when Scalar keeps it plain and it holds no
// flow indicator, otherwise single-quoted.
func flowScalar(s string) string {
	if v := yamledit.Scalar(s); v != s || strings.ContainsAny(s, ",[]{}") {
		if strings.HasPrefix(v, "\"") {
			return v
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return s
}

// targetName is config's pattern for target names.
var targetName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// readsBack reports whether out's targets decode to want, as config.Load would read them, with
// valid and distinct names.
func readsBack(out []byte, want []config.Target) bool {
	var got struct {
		Targets []config.Target `yaml:"targets"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(out)).Decode(&got); err != nil || len(got.Targets) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for i := range want {
		a, b := got.Targets[i], want[i]
		if !targetName.MatchString(a.Name) || seen[a.Name] {
			return false
		}
		seen[a.Name] = true
		if a.Name != b.Name || a.Image != b.Image || a.Dir != b.Dir || a.File != b.File || a.Compose != b.Compose || a.Source != b.Source ||
			a.LockfileFallback != b.LockfileFallback || !slices.Equal(a.Exclude, b.Exclude) {
			return false
		}
	}
	return true
}

// shortName is project p's target name (R6): its name without an npm @scope/, lower-cased, with
// characters outside [a-z0-9._-] replaced by "-" and leading or trailing separators trimmed;
// "app" when nothing is left.
func shortName(p plan.Project) string {
	n := p.Name
	if strings.HasPrefix(n, "@") {
		if _, rest, ok := strings.Cut(n, "/"); ok {
			n = rest
		}
	}
	n = strings.Trim(sanitizeName(n), ".-_")
	if n == "" {
		return "app"
	}
	return n
}

// sanitizeName lower-cases s and replaces every character outside [a-z0-9._-] with "-".
func sanitizeName(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// composeNotGated is the COMPOSE_NOT_GATED note for compose target t, which file has no target for.
func composeNotGated(t config.Target, file string) plan.Note {
	return plan.Note{
		Code: "COMPOSE_NOT_GATED",
		Message: fmt.Sprintf("%s is not gated: %s has no compose target for it. If its images ship, add this to targets, then run shearbolt setup again, so every gate scans it:\n%s",
			t.Compose, file, strings.Join(targetItems([]config.Target{t}, "  "), "\n")),
		File: file,
	}
}
