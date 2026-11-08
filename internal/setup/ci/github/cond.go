package github

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

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
