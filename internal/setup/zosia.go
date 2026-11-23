package setup

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const (
	zosiaInputLine = "zosia-secret: ${{ secrets.ZOSIA_SECRET }}"
	zosiaEnvLine   = "ZOSIA_SECRET: ${{ secrets.ZOSIA_SECRET }}"
)

// Zosia computes what `shearbolt zosia enable` (on) and `disable` change: whether the Shearbolt
// gates the CI files already have are handed the secret (the zosia-secret input of an action
// step, ZOSIA_SECRET in the env of a run step), and nothing else. Like Compute it never writes;
// Write applies the result.
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
			gates := allScanGates(jobs)
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
				if g.Zosia != on {
					edit = append(edit, g)
				}
			}
			out, failed, err := ed.SetZosia(f, src, edit, on)
			if err != nil {
				return fail("PARSE", fmt.Sprintf("%s cannot be edited: %v", f, err), f)
			}
			// The notes name the gates where they are once the change is written.
			written := gates
			if !bytes.Equal(out, src) {
				c := plan.FileChange{Path: f, Action: "modify", Content: string(out), Base: src}
				if err := describeChange(abs, &c); err != nil {
					return fail("READ", err.Error(), f)
				}
				p.Changes = append(p.Changes, c)
				if jobs, _, err := ci.Jobs(r, f, out, classify); err == nil {
					if now := allScanGates(jobs); len(now) == len(gates) {
						written = now
					}
				}
			}
			p.Notes = append(p.Notes, zosiaGateNotes(gates, written, edit, failed, on)...)
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

func allScanGates(jobs []plan.Job) []plan.GateCmd {
	var out []plan.GateCmd
	for _, j := range jobs {
		out = append(out, scanGates(j)...)
	}
	return out
}

// zosiaGateNotes says, per gate of a file whose gates are handed the secret in the file, what
// enable or disable did. written holds the same gates at their positions after the edit.
func zosiaGateNotes(gates, written, edit []plan.GateCmd, failed map[int]error, on bool) []plan.Note {
	var notes []plan.Note
	note := func(g plan.GateCmd, code, msg string) {
		notes = append(notes, plan.Note{Code: code, Message: msg, File: g.Pos.File, Line: g.Pos.Line})
	}
	for k, g := range gates {
		i := slices.IndexFunc(edit, func(e plan.GateCmd) bool { return e.Pos == g.Pos })
		run := g.Pos.Command >= 0
		g = written[k]
		switch {
		case i >= 0 && failed[i] != nil && on && run:
			note(g, "UNSUPPORTED_EDIT", fmt.Sprintf("this shearbolt step cannot be edited: %s. Add `%s` under its env: by hand.", cause(failed[i]), zosiaEnvLine))
		case i >= 0 && failed[i] != nil && on:
			note(g, "UNSUPPORTED_EDIT", fmt.Sprintf("this shearbolt step cannot be edited: %s. Add `%s` under its with: by hand.", cause(failed[i]), zosiaInputLine))
		case i >= 0 && failed[i] != nil && run:
			notes = append(notes, zosiaKeptNote(g))
		case i >= 0 && failed[i] != nil:
			note(g, "UNSUPPORTED_EDIT", fmt.Sprintf("this shearbolt step cannot be edited: %s. Remove its zosia-secret line by hand.", cause(failed[i])))
		case i >= 0 && on && run:
			note(g, "ZOSIA_ADDED", "added `"+zosiaEnvLine+"` to the env of this step, which runs shearbolt.")
		case i >= 0 && on:
			note(g, "ZOSIA_ADDED", "added `"+zosiaInputLine+"` to this shearbolt step.")
		case i >= 0 && run:
			note(g, "ZOSIA_REMOVED", "removed ZOSIA_SECRET from the env of this step.")
		case i >= 0:
			note(g, "ZOSIA_REMOVED", "removed the zosia-secret input from this shearbolt step.")
		case on:
			note(g, "ZOSIA_ALREADY", "this shearbolt step already sends to Zosia.")
		}
		if on && !sendsToZosia(g.Version) {
			note(g, "ZOSIA_GATE_TOO_OLD", fmt.Sprintf("this step runs Shearbolt %s, which cannot send reports to Zosia: that came with %s. Nothing is sent until the step runs a newer release; move its version (and its checksum, if it pins one).", g.Version, zosiaSince))
		}
	}
	return notes
}

const zosiaSince = "1.4.0"

// sendsToZosia reports whether Shearbolt release v (X.Y.Z) sends reports; "" is a release that
// is not known, which is taken to send.
func sendsToZosia(v string) bool {
	var major, minor int
	if _, err := fmt.Sscanf(v, "%d.%d.", &major, &minor); err != nil {
		return true
	}
	return major > 1 || major == 1 && minor >= 4
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
