package github

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/shelledit"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

// statusCheck matches the status functions that make a condition run after a failed step.
var statusCheck = regexp.MustCompile(`(?i)\b(always|failure|cancelled)\s*\(\s*\)`)

// safeIf returns an if: condition that cannot run after a failed gate.
func safeIf(cond string) (string, error) {
	if !needsWrap(cond) {
		return cond, nil
	}
	if t := strings.TrimSpace(cond); strings.Contains(t, "${{") && !wholeExpr(t) {
		return "", fmt.Errorf("the condition %s mixes ${{ }} with other text, so it cannot be wrapped in success() && (...)", cond)
	}
	return "${{ success() && (" + bareExpr(cond) + ") }}", nil
}

func wholeExpr(t string) bool {
	return strings.HasPrefix(t, "${{") && strings.HasSuffix(t, "}}") && strings.Count(t, "${{") == 1 && strings.Count(t, "}}") == 1
}

func needsWrap(cond string) bool {
	if !statusCheck.MatchString(cond) {
		return false
	}
	// setup's marker forms begin with success() or hold no status function: already safe.
	if _, ok := markerGuarded(cond); ok {
		return false
	}
	inner, ok := strings.CutPrefix(bareExpr(cond), "success() && (")
	if !ok || !strings.HasSuffix(inner, ")") {
		return true
	}
	return !balanced(strings.TrimSuffix(inner, ")"))
}

func bareExpr(cond string) string {
	t := strings.TrimSpace(cond)
	if wholeExpr(t) {
		return strings.TrimSpace(t[3 : len(t)-2])
	}
	return t
}

func balanced(s string) bool {
	depth, quoted := 0, false
	for _, c := range s {
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			if depth--; depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !quoted
}

func markerExpr(id string) string {
	return "steps." + id + ".outputs." + shelledit.ReachedOutput + " != ''"
}

// The parentheses keep an a || b condition from swallowing the marker.
func withMarker(cond, id string) string {
	if b := bareExpr(cond); b != "" {
		return "(" + b + ") && " + markerExpr(id)
	}
	return markerExpr(id)
}

// The leading success() keeps a status function in copied from running the step after a failed
// gate. Left wrapped, (${{ … }}) would be a non-empty string, which is always true; for the same
// reason a condition that mixes ${{ }} with other text cannot be combined.
func continuedIf(copied, id string) (string, error) {
	b := bareExpr(copied)
	if b == "" {
		return markerExpr(id), nil
	}
	if strings.Contains(b, "${{") {
		return "", fmt.Errorf("the condition %s mixes ${{ }} with other text, so it cannot be combined with the check that the build ran", copied)
	}
	return "success() && (" + b + ") && " + markerExpr(id), nil
}

// stripAnyMarker removes a marker term in setup's form, whatever its step id: (<X>) && <marker>
// becomes X, and a condition that is exactly <marker> becomes "". A ${{ }} wrapper is dropped,
// which means the same. The marker is matched with any run of whitespace where setup writes a
// space; X keeps its text.
func stripAnyMarker(cond string) (stripped, id string, ok bool) {
	b := bareExpr(cond)
	if m := markerAlone.FindStringSubmatch(normCond(b)); m != nil {
		return "", m[1], true
	}
	loc := markerAfter.FindStringSubmatchIndex(b)
	if loc == nil || !strings.HasPrefix(b, "(") {
		return cond, "", false
	}
	inner := strings.TrimSpace(b[1:loc[0]])
	if inner == "" || !balanced(inner) {
		return cond, "", false
	}
	return inner, b[loc[2]:loc[3]], true
}

// markerAlone matches a marker condition, whitespace collapsed, and captures its step id.
var markerAlone = regexp.MustCompile(`^steps\.([A-Za-z_][A-Za-z0-9_-]*)\.outputs\.` + regexp.QuoteMeta(shelledit.ReachedOutput) + ` != ''$`)

// markerAfter matches `) && <marker>` at the end of a condition, with any run of whitespace where
// setup writes a space, and captures the marker's step id.
var markerAfter = regexp.MustCompile(`\)\s+&&\s+steps\.([A-Za-z_][A-Za-z0-9_-]*)\.outputs\.` + regexp.QuoteMeta(shelledit.ReachedOutput) + `\s+!=\s+''$`)

// markerGuarded reports whether condition cond is one of setup's two forms that run a step only
// when split step id got past its build: exactly <marker>, or success() && (<X>) && <marker>
// where X's parentheses are balanced, as needsWrap requires, so that success() && (a) || (b) &&
// <marker> does not count. ${{ }} anywhere inside makes the condition a string, which is always
// true, so it does not count either.
func markerGuarded(cond string) (id string, ok bool) {
	n := normCond(cond)
	if strings.Contains(n, "${{") {
		return "", false
	}
	if m := markerAlone.FindStringSubmatch(n); m != nil {
		return m[1], true
	}
	rest, ok := strings.CutPrefix(n, "success() && (")
	i := strings.LastIndex(rest, ") && ")
	if !ok || i < 0 || !balanced(rest[:i]) {
		return "", false
	}
	if m := markerAlone.FindStringSubmatch(rest[i+len(") && "):]); m != nil {
		return m[1], true
	}
	return "", false
}

// templated reports whether condition cond still holds ${{ }} without its own wrapper: combined
// with the marker it would be a template string, which is always true.
func templated(cond string) bool { return strings.Contains(bareExpr(cond), "${{") }

func normCond(cond string) string {
	return strings.Join(strings.Fields(bareExpr(cond)), " ")
}

const (
	reasonAllTargets = "it checks every target in shearbolt.yaml"
	reasonOtherSteps = "it also follows builds in other steps"
	reasonLaterPush  = "a later step pushes or deploys"
	reasonTemplate   = "its condition cannot be combined with the marker"
	reasonNotNext    = "it does not directly follow this step"
	reasonRunStep    = "setup does not add an if: to a gate in a run step"
)

// gateMaySkip reports whether gate g may be skipped when split step s stopped before its build,
// and why not when it may not: (1) g scans only its own images, all built in step s, and (2) no
// step after s pushes or deploys unless its if: is one of setup's marker forms for s. On a first
// run the steps setup inserts after s (the gate, its continued and push steps) are not among the
// job's steps yet, so the later steps are the original ones; on a re-run the continued step is
// one of them and passes (2) by its if:.
func (e *jobEditor) gateMaySkip(g plan.Gate, s int) (bool, string) {
	if !g.OnlyOwnImages || slices.ContainsFunc(g.Builds, func(b plan.Build) bool { return b.Pos.Step != s }) {
		if g.Scans == nil {
			return false, reasonAllTargets
		}
		here := map[string]bool{}
		for _, iv := range g.Images {
			if iv.Build.Pos.Step == s {
				here[iv.Target] = true
			}
		}
		for _, f := range g.Files {
			if f.Build.Pos.Step == s {
				here[f.Target] = true
			}
		}
		var other []string
		for _, n := range g.Scans {
			if !here[n] {
				other = append(other, n)
			}
		}
		if len(other) == 0 {
			return false, reasonOtherSteps
		}
		return false, "it also checks " + strings.Join(other, ", ")
	}
	id := e.j.steps[s].id
	if sp := e.steps[s]; sp != nil && sp.id != "" {
		id = sp.id
	}
	for k := s + 1; k < len(e.j.steps); k++ {
		if !e.pushes(k) {
			continue
		}
		if n := e.j.steps[k].ifNode; n != nil && id != "" {
			if got, ok := markerGuarded(n.Value); ok && got == id {
				continue
			}
		}
		return false, reasonLaterPush
	}
	return true, ""
}

// pushes reports whether step s pushes or deploys: a command or the step itself, or an image
// build that pushes (Classified.Push leaves those out).
func (e *jobEditor) pushes(s int) bool {
	ps := e.pj.Steps[s]
	return len(ps.Push) > 0 || slices.ContainsFunc(ps.Builds, func(b plan.Build) bool {
		return b.Image != nil && (b.Image.Push || b.Image.OutputPush)
	})
}

func (e *jobEditor) wrapPushIfs() error {
	done := map[int]bool{}
	for _, g := range e.gates {
		after := g.At.Step
		switch {
		case g.Existing != nil:
			after = g.Existing.Pos.Step
		case g.Before:
			after = g.At.Step - 1
		}
		for s := after + 1; s < len(e.j.steps); s++ {
			st := e.j.steps[s]
			if done[s] || !e.pushes(s) || st.ifNode == nil || !needsWrap(st.ifNode.Value) {
				continue
			}
			if !editable(st) {
				return failAt(st.line, "this push step runs even when the gate fails (if: %s), and setup cannot change it in place", st.ifNode.Value)
			}
			wrapped, err := safeIf(st.ifNode.Value)
			if err != nil {
				return failAt(st.ifNode.Line, "this push step runs even when the gate fails, and %v", err)
			}
			if err := e.d.SetKey(st.m, "if", yamledit.Scalar(wrapped), ""); err != nil {
				return failAt(st.line, "this push step runs even when the gate fails, and its if: cannot be changed (%v)", err)
			}
			done[s] = true
			label := st.name
			if label == "" {
				label = fmt.Sprintf("step %d", s+1)
			}
			e.notes = append(e.notes, plan.Note{
				Code:    "PUSH_IF_WRAPPED",
				Message: fmt.Sprintf("the push step %s had if: %s, which would run it even when the gate fails; setup changed it to %s", label, st.ifNode.Value, wrapped),
				File:    e.wf.file,
				Line:    st.ifNode.Line,
			})
		}
	}
	return nil
}
