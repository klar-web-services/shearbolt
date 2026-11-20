package setup

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const zosiaInputLine = "zosia-secret: ${{ secrets.ZOSIA_SECRET }}"

// Zosia computes what `shearbolt zosia enable` (on) and `disable` change: the zosia-secret input
// of the Shearbolt gates the CI files already have, and nothing else. Like Compute it never
// writes; Write applies the result.
func Zosia(root string, on bool) *plan.Plan {
	p := &plan.Plan{SchemaVersion: plan.SchemaVersion}
	fail := func(code, msg, file string) *plan.Plan {
		p.Error = &plan.Note{Code: code, Message: msg, File: file}
		return p
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fail("READ", err.Error(), "")
	}
	r := plan.Repo{Root: abs}
	classify := newClassifier(r, ecosystems)
	gated := false
	for _, ci := range cis {
		for _, f := range ci.Files(r) {
			p.CI = append(p.CI, plan.CIFile{System: ci.Name(), File: f})
			src, err := r.Read(f)
			if err != nil {
				return fail("READ", fmt.Sprintf("%s cannot be read: %v", f, err), f)
			}
			jobs, _, err := ci.Jobs(r, f, src, classify)
			if err != nil {
				return fail("PARSE", fmt.Sprintf("%s is not valid YAML: %v", f, err), f)
			}
			var gates []plan.GateCmd
			for _, j := range jobs {
				for _, st := range j.Steps {
					for _, g := range st.Gates {
						if !g.Install {
							gates = append(gates, g)
						}
					}
				}
			}
			if len(gates) == 0 {
				continue
			}
			gated = true
			ed, ok := ci.(plan.ZosiaEditor)
			if !ok {
				if on {
					p.Notes = append(p.Notes, plan.ZosiaSecretNote(ci.Name(), f))
				} else {
					p.Notes = append(p.Notes, plan.Note{Code: "ZOSIA_VARIABLE", File: f,
						Message: "nothing to change in this file: its gate sends to Zosia whenever the CI variable ZOSIA_SECRET is set."})
				}
				continue
			}
			var edit []plan.GateCmd
			for _, g := range gates {
				if g.Pos.Command < 0 && g.Zosia != on {
					edit = append(edit, g)
				}
			}
			out, failed, err := ed.SetZosia(f, src, edit, on)
			if err != nil {
				return fail("PARSE", fmt.Sprintf("%s cannot be edited: %v", f, err), f)
			}
			if !bytes.Equal(out, src) {
				c := plan.FileChange{Path: f, Action: "modify", Content: string(out), Base: src}
				if err := describeChange(abs, &c); err != nil {
					return fail("READ", err.Error(), f)
				}
				p.Changes = append(p.Changes, c)
			}
			p.Notes = append(p.Notes, zosiaGateNotes(gates, edit, failed, on)...)
			if on {
				n := plan.ZosiaSecretNote(ci.Name(), f)
				n.Message += " With the GitHub CLI, run `gh secret set ZOSIA_SECRET` in the repository and paste the secret when it asks."
				p.Notes = append(p.Notes, n)
			} else if !slices.ContainsFunc(gates, func(g plan.GateCmd) bool { return g.Zosia }) {
				p.Notes = append(p.Notes, plan.Note{Code: "ZOSIA_OFF", File: f, Message: "no shearbolt gate in this file sends to Zosia through its step, so there is nothing to remove."})
			}
		}
	}
	switch {
	case len(p.CI) == 0:
		return fail("NO_CI", "No CI configuration was found here. Run this from the repository root, after `shearbolt setup` has added the gate.", "")
	case !gated:
		return fail("NO_GATE", "No Shearbolt gate was found in the CI files here, so there is nothing to send reports from. Run `shearbolt setup` first; it asks about Zosia too.", "")
	}
	if !on {
		p.Notes = append(p.Notes, plan.Note{Code: "ZOSIA_SECRET_STAYS",
			Message: "The secret itself stays where it is. Delete ZOSIA_SECRET from the repository's secrets or CI variables (GitHub: `gh secret delete ZOSIA_SECRET`), or revoke it at https://zosiasec.com/app/secrets, to be sure nothing is sent."})
	}
	p.Complete = true
	return p
}

// zosiaGateNotes says, per gate of a file whose gates take the input, what enable or disable did.
func zosiaGateNotes(gates, edit []plan.GateCmd, failed map[int]error, on bool) []plan.Note {
	var notes []plan.Note
	note := func(g plan.GateCmd, code, msg string) {
		notes = append(notes, plan.Note{Code: code, Message: msg, File: g.Pos.File, Line: g.Pos.Line})
	}
	for _, g := range gates {
		i := slices.IndexFunc(edit, func(e plan.GateCmd) bool { return e.Pos == g.Pos })
		switch {
		case i >= 0 && failed[i] != nil && on:
			note(g, "UNSUPPORTED_EDIT", fmt.Sprintf("this shearbolt step cannot be edited: %s. Add `%s` under its with: by hand.", cause(failed[i]), zosiaInputLine))
		case i >= 0 && failed[i] != nil:
			note(g, "UNSUPPORTED_EDIT", fmt.Sprintf("this shearbolt step cannot be edited: %s. Remove its zosia-secret line by hand.", cause(failed[i])))
		case i >= 0 && on:
			note(g, "ZOSIA_ADDED", "added `"+zosiaInputLine+"` to this shearbolt step.")
		case i >= 0:
			note(g, "ZOSIA_REMOVED", "removed the zosia-secret input from this shearbolt step.")
		case on && g.Zosia:
			note(g, "ZOSIA_ALREADY", "this shearbolt step already sends to Zosia.")
		case on:
			notes = append(notes, zosiaRunGateNote(g))
		case g.Zosia:
			notes = append(notes, zosiaKeptNote(g))
		}
	}
	return notes
}

// cause drops the "line N: " an edit error starts with; the note carries the line.
func cause(err error) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "line "); ok {
		if _, after, ok := strings.Cut(rest, ": "); ok {
			return after
		}
	}
	return msg
}
