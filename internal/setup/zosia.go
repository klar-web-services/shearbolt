package setup

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
	"go.yaml.in/yaml/v3"
)

const (
	zosiaInputLine = "zosia-secret: ${{ secrets.ZOSIA_SECRET }}"
	zosiaEnvLine   = "ZOSIA_SECRET: ${{ secrets.ZOSIA_SECRET }}"
)

// Zosia computes what `shearbolt zosia enable` (on) and `disable` change: whether the Shearbolt
// gates the CI files already have are handed the secret (the zosia-secret input of an action
// step, ZOSIA_SECRET in the env of a run step), and whether the shearbolt.yaml files those gates
// read take their policy from Zosia (the zosia block). Nothing else. Like Compute it never
// writes; Write applies the result.
//
// policy widens enable and narrows disable: enable adds the block only with it, and disable
// with it removes only the block. Plain disable removes both.
func Zosia(root string, on, policy bool) *plan.Plan {
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
	var all []plan.GateCmd
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
			if on && policy && actionAtCommit.Match(src) {
				p.Notes = append(p.Notes, plan.Note{Code: "ZOSIA_POLICY_GATE_UNKNOWN", File: f,
					Message: "a step here uses the Shearbolt action at a commit, so its release cannot be told. A release before " + zosiaPolicySince + " fails on a shearbolt.yaml with a zosia block: check that the commit is of that release or a newer one."})
			}
			// A script that installs Shearbolt pins the release on the install line, not on the scan.
			pin := installPin(src)
			for _, g := range gates {
				if g.Version == "" {
					g.Version = pin
				}
				all = append(all, g)
			}
			if !on && policy {
				continue
			}
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
	if policy || !on {
		changes, notes, err := zosiaPolicy(r, abs, all, on)
		if err != nil {
			return fail("READ", err.Error(), "")
		}
		p.Changes = append(p.Changes, changes...)
		for _, n := range notes {
			// Plain disable says what it removed, and what it found and could not remove. What it could
			// not look at is said only to who asked about the policy, and a line that keeps a project
			// from sending only to who turned sending off.
			if policy && n.Code != "ZOSIA_SEND_KEPT" || !policy && n.Code != "ZOSIA_POLICY_BY_HAND" {
				p.Notes = append(p.Notes, n)
			}
		}
	}
	if !on && !policy {
		p.Notes = append(p.Notes, plan.Note{Code: "ZOSIA_SECRET_STAYS",
			Message: "The secret itself stays where it is. Delete ZOSIA_SECRET from the repository's secrets or CI variables (GitHub: `gh secret delete ZOSIA_SECRET`), or revoke it at https://zosiasec.com/app/secrets, to be sure nothing is sent."})
	}
	p.Complete = true
	return p
}

const (
	zosiaPolicySince = "1.11.0"
	zosiaSendSince   = "1.12.0"
)

// zosiaPolicy adds `policy: true` to the zosia block of (on), or removes it from, each
// shearbolt.yaml that one of the gates reads. A `send: false` in the block stays. A file is left alone, with a note, when it is not certain that a gate reads
// it, when it cannot be edited safely, or when a gate that reads it pins a Shearbolt release
// that would fail on the block.
func zosiaPolicy(r plan.Repo, abs string, gates []plan.GateCmd, on bool) ([]plan.FileChange, []plan.Note, error) {
	var changes []plan.FileChange
	var notes []plan.Note
	byHand := "Add these two lines to the shearbolt.yaml it reads by hand: `zosia:` and, indented below it, `policy: true`."
	value := "true"
	if !on {
		value = ""
		byHand = "Remove the policy line of the zosia block from the shearbolt.yaml it reads by hand, if it has one."
	}
	var configs []string
	old := map[string]bool{}
	for _, g := range gates {
		if g.Config == "" {
			notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_BY_HAND", File: g.Pos.File, Line: g.Pos.Line,
				Message: "it is not certain which shearbolt.yaml this step reads, so no file was changed for it. " + byHand})
			continue
		}
		if !slices.Contains(configs, g.Config) {
			configs = append(configs, g.Config)
		}
		if on && !old[g.Config] && !knowsZosiaPolicy(g.Version) {
			old[g.Config] = true
			notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_GATE_TOO_OLD", File: g.Pos.File, Line: g.Pos.Line,
				Message: fmt.Sprintf("this step runs Shearbolt %s, which fails on a shearbolt.yaml with a zosia block: that came with %s. %s was left as it is. Move the step's version (and its checksum, if it pins one), then run this again.", g.Version, zosiaPolicySince, g.Config)})
		}
	}
	slices.Sort(configs)
	added := false
	for _, f := range configs {
		if old[f] {
			continue
		}
		if !r.Exists(f) {
			if on {
				notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_BY_HAND", File: f, Message: "a gate reads this file, but it is not there, so nothing was changed."})
			}
			continue
		}
		src, err := r.Read(f)
		if err != nil {
			return nil, nil, fmt.Errorf("%s cannot be read: %v", f, err)
		}
		out, state, err := setZosiaKey(src, "policy", value)
		switch {
		case err != nil && !on && !bytes.Contains(src, []byte("zosia")):
			// Nothing in it names Zosia, so there is no block to tell about.
			continue
		case err != nil && !on:
			notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_KEPT", File: f, Message: fmt.Sprintf("this file has a zosia block that cannot be removed for you: %s. Until you remove it by hand, CI scans of this project go on asking Zosia for the policy.", cause(err))})
			continue
		case err != nil:
			notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_BY_HAND", File: f, Message: fmt.Sprintf("this file cannot be edited: %s. %s", cause(err), byHand)})
			continue
		case state == "already":
			notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_ALREADY", File: f, Message: "this project already takes its policy from Zosia."})
		case state == "added":
			notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_ADDED", File: f, Message: "added the zosia block: CI scans of this project now ask Zosia for the policy."})
		case state == "removed" && readZosiaKeys(out).Send != nil:
			notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_REMOVED", File: f, Message: "removed `policy: true` from the zosia block: this project uses the policy in this file again."})
		case state == "removed":
			notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_REMOVED", File: f, Message: "removed the zosia block: this project uses the policy in this file again."})
		}
		if send := readZosiaKeys(out).Send; !on && send != nil && !*send {
			notes = append(notes, plan.Note{Code: "ZOSIA_SEND_KEPT", File: f, Message: "this file still says `zosia: {send: false}`, and that line stays: the project's reports are not sent, whatever secret a scan has. `shearbolt zosia enable --all-projects` removes it."})
		}
		added = added || (on && err == nil)
		if !bytes.Equal(out, src) {
			c := plan.FileChange{Path: f, Action: "modify", Content: string(out), Base: src}
			if err := describeChange(abs, &c); err != nil {
				return nil, nil, err
			}
			changes = append(changes, c)
		}
	}
	if added {
		notes = append(notes, plan.Note{Code: "ZOSIA_POLICY_FILE_COUNTS",
			Message: "Set the policy in the repository's settings at https://zosiasec.com. The policy block of shearbolt.yaml is not changed and still counts: a scan that gets no policy from Zosia (Zosia cannot be reached, a pull request from a fork, a local scan) uses it, so keep one there that you can live with."})
	}
	return changes, notes, nil
}

// zosiaKeys is what the zosia block of a shearbolt.yaml says; a key that is not there is nil.
type zosiaKeys struct {
	Policy *bool `yaml:"policy"`
	Send   *bool `yaml:"send"`
}

// readZosiaKeys reads the block; a file that cannot be read as one has no keys.
func readZosiaKeys(src []byte) zosiaKeys {
	var f struct {
		Zosia zosiaKeys `yaml:"zosia"`
	}
	if yaml.Unmarshal(src, &f) != nil {
		return zosiaKeys{}
	}
	return f.Zosia
}

// setZosiaKey returns src with key of the zosia block ("policy" or "send") set to value, or
// removed when value is "", and what it did: "added", "removed", "already", or "" when there was
// nothing to remove. The block is made for its first key and goes with its last; another key in
// it is not touched.
func setZosiaKey(src []byte, key, value string) ([]byte, string, error) {
	doc, err := yamledit.Parse(src)
	if err != nil {
		return nil, "", err
	}
	root := doc.Root()
	block := doc.Get(root, "zosia")
	var line *yaml.Node
	if block != nil {
		line = doc.Get(block, key)
	}
	state := ""
	switch {
	case value != "" && block == nil:
		state, err = "added", doc.SetKey(root, "zosia", "\n  "+key+": "+value, "")
	case value != "" && line == nil:
		state, err = "added", doc.SetKey(block, key, value, "")
	case value != "" && line.Value == value:
		return src, "already", nil
	case value != "":
		state, err = "added", doc.ReplaceScalar(line, value)
	case line == nil:
		return src, "", nil
	case len(block.Content) == 2:
		state, err = "removed", doc.DeleteEntry(root, "zosia")
	default:
		state, err = "removed", doc.DeleteEntry(block, key)
	}
	if err != nil {
		return nil, "", err
	}
	out, err := doc.Result()
	if err != nil {
		return nil, "", err
	}
	// The result must be a config that says what was asked and nothing else new, or the file is
	// not touched.
	before, after := readZosiaKeys(src), readZosiaKeys(out)
	edited, other, was := after.Policy, after.Send, before.Send
	if key == "send" {
		edited, other, was = after.Send, after.Policy, before.Policy
	}
	same := func(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	if !same(other, was) || (value == "") != (edited == nil) || edited != nil && strconv.FormatBool(*edited) != value {
		return nil, "", fmt.Errorf("the edit did not give the file that was meant")
	}
	return out, state, nil
}

// ZosiaProject is a shearbolt.yaml that a scan gate of the repository reads: a project, as an
// account's plan in Zosia counts them.
type ZosiaProject struct {
	Config string // its path in the repository
	Name   string // the project it names; "" when the file cannot be read
	// A gate that reads it hands its report to Zosia, and the file does not say send: false.
	Sends bool
	Off   bool // the file says zosia: {send: false}
	// Why `send: false` cannot be written here, so that the project sends whatever is chosen; ""
	// when it can. FixedCode is the note's code for it.
	Fixed, FixedCode string
}

// ZosiaProjects lists the repository's projects, sorted by path, for choosing which of them send
// to Zosia. The notes name the gates whose shearbolt.yaml is not certain: they are in no project.
func ZosiaProjects(root string) ([]ZosiaProject, []plan.Note, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	r := plan.Repo{Root: abs}
	classify := newClassifier(r, ecosystems)
	by := map[string]*ZosiaProject{}
	hands := map[string]bool{}
	var notes []plan.Note
	files := 0
	for _, ci := range cis {
		// Where a file can hand the secret to a step, a gate sends when its step is handed it; where
		// it cannot, every gate sends once the CI variable is set.
		_, perStep := ci.(plan.ZosiaEditor)
		for _, f := range ci.Files(r) {
			files++
			src, err := r.Read(f)
			if err != nil {
				return nil, nil, fmt.Errorf("%s cannot be read: %v", f, err)
			}
			jobs, _, err := ci.Jobs(r, f, src, classify)
			if err != nil {
				return nil, nil, fmt.Errorf("%s is not valid YAML: %v", f, err)
			}
			pin, atCommit := installPin(src), actionAtCommit.Match(src)
			for _, g := range allScanGates(jobs) {
				if g.Version == "" {
					g.Version = pin
				}
				if g.Config == "" {
					notes = append(notes, plan.Note{Code: "ZOSIA_SEND_BY_HAND", File: g.Pos.File, Line: g.Pos.Line,
						Message: "it is not certain which shearbolt.yaml this step reads, so its project is not among those to choose from. To keep it from sending, add `send: false` to the zosia block of that file by hand."})
					continue
				}
				p := by[g.Config]
				if p == nil {
					p = &ZosiaProject{Config: g.Config}
					by[g.Config] = p
				}
				if (g.Zosia || !perStep) && sendsToZosia(g.Version) {
					hands[g.Config] = true
				}
				switch {
				case !knowsZosiaSend(g.Version):
					p.FixedCode = "ZOSIA_SEND_GATE_TOO_OLD"
					p.Fixed = fmt.Sprintf("a gate that reads it runs Shearbolt %s, which fails on a shearbolt.yaml that says `send: false`: that came with %s", g.Version, zosiaSendSince)
				case g.Version == "" && atCommit && p.Fixed == "":
					p.FixedCode = "ZOSIA_SEND_GATE_UNKNOWN"
					p.Fixed = "a gate that reads it uses the Shearbolt action at a commit, so its release cannot be told, and one before " + zosiaSendSince + " fails on a shearbolt.yaml that says `send: false`"
				}
			}
		}
	}
	if files == 0 {
		return nil, nil, fmt.Errorf("no CI configuration was found here")
	}
	var out []ZosiaProject
	for _, p := range by {
		src, err := r.Read(p.Config)
		if err != nil {
			if p.Fixed == "" {
				p.Fixed, p.FixedCode = "the file cannot be read", "ZOSIA_SEND_BY_HAND"
			}
		} else {
			var named struct {
				Project string `yaml:"project"`
			}
			_ = yaml.Unmarshal(src, &named)
			p.Name = named.Project
			send := readZosiaKeys(src).Send
			p.Off = send != nil && !*send
			if _, _, err := setZosiaKey(src, "send", "false"); err != nil && p.Fixed == "" {
				p.Fixed, p.FixedCode = "the file cannot be edited: "+cause(err), "ZOSIA_SEND_BY_HAND"
			}
		}
		p.Sends = hands[p.Config] && !p.Off
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b ZosiaProject) int { return strings.Compare(a.Config, b.Config) })
	return out, notes, nil
}

// ZosiaSend computes the second write of `shearbolt zosia enable`: the projects in sending (paths
// of shearbolt.yaml files) send to Zosia, and every other project of the repository says
// `zosia: {send: false}`. A project that cannot be switched off keeps sending, with a note.
func ZosiaSend(root string, sending []string) *plan.Plan {
	p := &plan.Plan{SchemaVersion: plan.SchemaVersion}
	fail := func(code, msg string) *plan.Plan {
		p.Error = &plan.Note{Code: code, Message: msg}
		return p
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fail("READ", err.Error())
	}
	projects, notes, err := ZosiaProjects(root)
	if err != nil {
		return fail("READ", err.Error())
	}
	var paths []string
	for _, pr := range projects {
		paths = append(paths, pr.Config)
	}
	for _, s := range sending {
		if !slices.Contains(paths, s) {
			return fail("NO_PROJECT", fmt.Sprintf("%s is not a shearbolt.yaml that a gate of this repository reads. Its projects are: %s.", s, strings.Join(paths, ", ")))
		}
	}
	r := plan.Repo{Root: abs}
	for _, pr := range projects {
		off := !slices.Contains(sending, pr.Config)
		note := func(code, msg string) {
			p.Notes = append(p.Notes, plan.Note{Code: code, File: pr.Config, Message: msg})
		}
		if off == pr.Off {
			continue
		}
		if off && pr.Fixed != "" {
			// One that sends nothing anyway is neither switched off nor kept on: nothing to say.
			if !pr.Sends {
				continue
			}
			msg := "this project keeps sending its reports to Zosia: " + pr.Fixed + "."
			if pr.FixedCode == "ZOSIA_SEND_GATE_TOO_OLD" {
				msg += " Move the step's version (and its checksum, if it pins one), then run this again."
			}
			note(pr.FixedCode, msg)
			continue
		}
		src, err := r.Read(pr.Config)
		if err != nil {
			return fail("READ", fmt.Sprintf("%s cannot be read: %v", pr.Config, err))
		}
		value := ""
		if off {
			value = "false"
		}
		out, _, err := setZosiaKey(src, "send", value)
		if err != nil {
			note("ZOSIA_SEND_BY_HAND", fmt.Sprintf("this file says `zosia: {send: false}` and cannot be edited: %s. Remove the line by hand for the project to send again.", cause(err)))
			continue
		}
		c := plan.FileChange{Path: pr.Config, Action: "modify", Content: string(out), Base: src}
		if err := describeChange(abs, &c); err != nil {
			return fail("READ", err.Error())
		}
		p.Changes = append(p.Changes, c)
		if off {
			note("ZOSIA_SEND_OFF", "added `send: false` to the zosia block: this project's reports are not sent to Zosia.")
		} else {
			note("ZOSIA_SEND_ON", "removed `send: false`: this project sends its reports to Zosia again.")
		}
	}
	p.Notes = append(p.Notes, notes...)
	p.Complete = true
	return p
}

// SHEARBOLT_VERSION=1.2.3 on the install line, or SHEARBOLT_VERSION: "1.2.3" among the variables.
var installPinned = regexp.MustCompile(`SHEARBOLT_VERSION\s*[=:]\s*["']?(\d+)\.(\d+)\.(\d+)`)

// A step that uses the action at a commit: which release that is cannot be told from the file.
var actionAtCommit = regexp.MustCompile(`klar-web-services/shearbolt@[0-9a-f]{40}\b`)

// installPin is the oldest Shearbolt release that a CI file pins for its install
// (SHEARBOLT_VERSION=X.Y.Z, or the same as a variable), or "" when it pins none.
func installPin(src []byte) string {
	oldest, pin := [3]int{}, ""
	for _, m := range installPinned.FindAllSubmatch(src, -1) {
		var v [3]int
		for i := range v {
			v[i], _ = strconv.Atoi(string(m[i+1]))
		}
		if pin == "" || slices.Compare(v[:], oldest[:]) < 0 {
			oldest, pin = v, fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
		}
	}
	return pin
}

// knowsZosiaPolicy reports whether Shearbolt release v (X.Y.Z) reads the zosia block; "" is a
// release that is not known, which is taken to.
func knowsZosiaPolicy(v string) bool {
	var major, minor int
	if _, err := fmt.Sscanf(v, "%d.%d.", &major, &minor); err != nil {
		return true
	}
	return major > 1 || major == 1 && minor >= 11
}

// knowsZosiaSend reports whether Shearbolt release v (X.Y.Z) reads `send` in the zosia block; ""
// is a release that is not known, which is taken to.
func knowsZosiaSend(v string) bool {
	var major, minor int
	if _, err := fmt.Sscanf(v, "%d.%d.", &major, &minor); err != nil {
		return true
	}
	return major > 1 || major == 1 && minor >= 12
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
