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

// safeIf returns an if: condition that cannot run after a failed gate: a condition with
// always(), failure(), cancelled() or !cancelled() becomes ${{ success() && (<condition>) }}.
func safeIf(cond string) (string, error) {
	if !needsWrap(cond) {
		return cond, nil
	}
	if t := strings.TrimSpace(cond); strings.Contains(t, "${{") && !wholeExpr(t) {
		return "", fmt.Errorf("the condition %s mixes ${{ }} with other text, so it cannot be wrapped in success() && (...)", cond)
	}
	return "${{ success() && (" + bareExpr(cond) + ") }}", nil
}

// wholeExpr reports whether t is one ${{ ... }} expression and nothing else.
func wholeExpr(t string) bool {
	return strings.HasPrefix(t, "${{") && strings.HasSuffix(t, "}}") && strings.Count(t, "${{") == 1 && strings.Count(t, "}}") == 1
}

func needsWrap(cond string) bool {
	if !statusCheck.MatchString(cond) {
		return false
	}
	inner, ok := strings.CutPrefix(bareExpr(cond), "success() && (")
	if !ok || !strings.HasSuffix(inner, ")") {
		return true
	}
	return !balanced(strings.TrimSuffix(inner, ")"))
}

// bareExpr returns a condition without its ${{ }} wrapper.
func bareExpr(cond string) string {
	t := strings.TrimSpace(cond)
	if wholeExpr(t) {
		return strings.TrimSpace(t[3 : len(t)-2])
	}
	return t
}

// balanced reports whether s's parentheses (outside '...' strings) never close more than they
// opened and all close.
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

// The marker (design §3.2, §3.4): a split step that can stop before its build sets the output
// shearbolt-reached right after the build, and what runs after the gate checks it. These helpers
// write and read setup's forms of that check.

// markerExpr is the condition that step id got past its build: its marker output is set.
func markerExpr(id string) string {
	return "steps." + id + ".outputs." + shelledit.ReachedOutput + " != ''"
}

// withMarker adds step id's marker to condition cond: (<cond>) && <marker>, with cond's ${{ }}
// wrapper removed, or the marker alone when cond is empty. The parentheses keep an a || b
// condition from swallowing the marker.
func withMarker(cond, id string) string {
	if b := bareExpr(cond); b != "" {
		return "(" + b + ") && " + markerExpr(id)
	}
	return markerExpr(id)
}

// continuedIf returns the if: of the continued step of split step id when that step can stop
// early: success() && (<copied>) && <marker>, where copied (the step's own if:) is made safe by
// safeIf and then unwrapped, or the marker alone when there is no copied condition. Left
// wrapped, (${{ … }}) would be a non-empty string, which is always true; for the same reason a
// condition that mixes ${{ }} with other text cannot be combined.
func continuedIf(copied, id string) (string, error) {
	if bareExpr(copied) == "" {
		return markerExpr(id), nil
	}
	safe, err := safeIf(copied)
	if err != nil {
		return "", err
	}
	b := bareExpr(safe)
	if strings.Contains(b, "${{") {
		return "", fmt.Errorf("the condition %s mixes ${{ }} with other text, so it cannot be combined with the check that the build ran", copied)
	}
	return "success() && (" + b + ") && " + markerExpr(id), nil
}

// stripMarker removes step id's marker from condition cond as withMarker adds it: (<X>) &&
// <marker> becomes X, and a condition that is exactly <marker> becomes "". A ${{ }} wrapper is
// dropped, which means the same. The marker is matched with any run of whitespace where setup
// writes a space; X keeps its text. ok is false, and cond comes back unchanged, when cond holds
// no such term.
func stripMarker(cond, id string) (string, bool) {
	b := bareExpr(cond)
	if collapse(b) == markerExpr(id) {
		return "", true
	}
	tail := regexp.MustCompile(`\)\s+&&\s+steps\.` + regexp.QuoteMeta(id) + `\.outputs\.` + regexp.QuoteMeta(shelledit.ReachedOutput) + `\s+!=\s+''$`)
	loc := tail.FindStringIndex(b)
	if loc == nil || !strings.HasPrefix(b, "(") {
		return cond, false
	}
	inner := strings.TrimSpace(b[1:loc[0]])
	if inner == "" || !balanced(inner) {
		return cond, false
	}
	return inner, true
}

// markerAlone matches a marker condition, whitespace collapsed, and captures its step id.
var markerAlone = regexp.MustCompile(`^steps\.([A-Za-z_][A-Za-z0-9_-]*)\.outputs\.` + regexp.QuoteMeta(shelledit.ReachedOutput) + ` != ''$`)

// markerGuarded reports whether condition cond is one of setup's two forms that run a step only
// when split step id got past its build: exactly <marker>, or success() && (<X>) && <marker>
// where X's parentheses are balanced, as needsWrap requires, so that success() && (a) || (b) &&
// <marker> does not count. The text is compared without its ${{ }} wrapper and with each run of
// whitespace collapsed to one space.
func markerGuarded(cond string) (id string, ok bool) {
	n := collapse(cond)
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

// collapse returns cond without its ${{ }} wrapper and with each run of whitespace as one space.
func collapse(cond string) string {
	return strings.Join(strings.Fields(bareExpr(cond)), " ")
}

// pushes reports whether step s pushes or deploys: a command or the step itself, or an image
// build that pushes (Classified.Push leaves those out).
func (e *jobEditor) pushes(s int) bool {
	ps := e.pj.Steps[s]
	return len(ps.Push) > 0 || slices.ContainsFunc(ps.Builds, func(b plan.Build) bool {
		return b.Image != nil && (b.Image.Push || b.Image.OutputPush)
	})
}

// wrapPushIfs rewrites the if: of existing push steps after a gate whose condition would run
// them after a failed gate (always(), failure(), cancelled(), !cancelled()).
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
