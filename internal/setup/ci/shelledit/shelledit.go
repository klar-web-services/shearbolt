// Package shelledit holds the CI-neutral pieces setup's CI plugins use to edit shell scripts:
// where added lines may go and where a script can be split (R4 rules 5 and 6), the scan tag,
// --load and --push edits of image builds (R2, R5), and the checks that keep those edits fail
// closed. The GitHub plugin edits run steps with them, the script-list plugins (Bitbucket,
// GitLab) script items.
package shelledit

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// Error is why a job cannot be edited, located at a file line (0 when unknown).
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Errorf returns an *Error at line.
func Errorf(line int, format string, args ...any) error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, args...)}
}

// Edit replaces script[At:End] with Text (an insert when At == End).
type Edit struct {
	At, End int
	Text    string
}

// Apply applies the edits that fall in s, which starts at offset base of the script. Edits
// at one offset keep their queue order.
func Apply(s string, base int, edits []Edit) (string, error) {
	idx := make([]int, len(edits))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		if edits[a].At != edits[b].At {
			return edits[b].At - edits[a].At
		}
		return b - a
	})
	for _, i := range idx {
		a, b := edits[i].At-base, edits[i].End-base
		if a < 0 || b < a || b > len(s) {
			return "", fmt.Errorf("an edit falls outside its script")
		}
		s = s[:a] + edits[i].Text + s[b:]
	}
	return s, nil
}

// Insertable checks that lines can be added, or the step split, right after cmd (R4 rule 5
// (b), (c)): at the top level, outside a heredoc, without a line continuation after it, and
// ending in &&, ;, a newline or the end of the script.
func Insertable(cmd shell.Command, line int, what string) error {
	text := CommandText(cmd)
	switch {
	case cmd.Depth > 0:
		return Errorf(line, "%s runs inside a shell conditional, loop, group or heredoc, so setup cannot %s", text, what)
	case cmd.ContAfter:
		return Errorf(line, "a \\ line continuation follows %s, so setup cannot %s", text, what)
	case slices.ContainsFunc(cmd.Redirs, func(r shell.Redir) bool {
		op := strings.TrimLeft(r.Op, "0123456789")
		return strings.HasPrefix(op, "<<") && !strings.HasPrefix(op, "<<<")
	}):
		return Errorf(line, "%s reads a heredoc, so setup cannot %s", text, what)
	}
	switch cmd.Op {
	case "&&", ";", "\n", "":
		return nil
	case "|":
		return Errorf(line, "%s is piped (|) into the next command, so setup cannot %s", text, what)
	case "||":
		return Errorf(line, "%s is followed by ||, so setup cannot %s", text, what)
	}
	return Errorf(line, "%s ends in %q, so setup cannot %s", text, cmd.Op, what)
}

// CommandText is a short form of a command for messages.
func CommandText(cmd shell.Command) string {
	var words []string
	for _, w := range cmd.Words {
		words = append(words, w.Raw)
	}
	s := strings.Join(words, " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:57]) + "..."
	}
	return "`" + s + "`"
}

// InsertPoint returns where added lines go after cmd in script, and their text: joined with the
// command's own && or ;, or on new lines after its logical line (with its indentation). Lines
// added at the end of a script are terminated, so a one-line script becomes a block.
func InsertPoint(script string, cmd shell.Command, added []string) (int, string) {
	var b strings.Builder
	switch cmd.Op {
	case "&&", ";":
		sep := " && "
		if cmd.Op == ";" {
			sep = "; "
		}
		for _, a := range added {
			b.WriteString(sep + a)
		}
		return cmd.End, b.String()
	}
	at := lineEnd(script, cmd.End)
	ind := lineIndent(script, cmd.Start)
	for _, a := range added {
		b.WriteString("\n" + ind + a)
	}
	if at == len(script) {
		b.WriteString("\n")
	}
	return at, b.String()
}

func lineEnd(s string, from int) int {
	if i := strings.IndexByte(s[from:], '\n'); i >= 0 {
		return from + i
	}
	return len(s)
}

func lineIndent(s string, at int) string {
	start := strings.LastIndexByte(s[:at], '\n') + 1
	end := start
	for end < len(s) && (s[end] == ' ' || s[end] == '\t') {
		end++
	}
	return s[start:end]
}

// UsableScan checks that an existing scan, command c of cmds, stops the job when it fails: it
// runs at the top level (not in a conditional, loop, group, subshell or after !), not in the
// background or a pipeline, and is joined to the commands around it only by ;, a newline or
// nothing. With || true, inside if, or as an element of an && list it would not: under set -e a
// failing element of && other than the last does not exit (Batch 5 review).
func UsableScan(cmds []shell.Command, c, line int) error {
	cmd := cmds[c]
	prev := ""
	if c > 0 {
		prev = cmds[c-1].Op
	}
	joined := func(op string) bool { return op == ";" || op == "\n" || op == "" }
	why := ""
	switch {
	case cmd.Depth > 0:
		why = "runs inside a shell conditional, loop, group, subshell or negation"
	case cmd.Op == "|" || prev == "|":
		why = "runs in a pipeline"
	case cmd.Op == "&":
		why = "runs in the background"
	case !joined(cmd.Op):
		why = fmt.Sprintf("is followed by %s", cmd.Op)
	case !joined(prev):
		why = fmt.Sprintf("follows %s", prev)
	default:
		return nil
	}
	return Errorf(line, "the existing scan %s %s, so a failing scan would not stop the job; make it a command of its own (joined only by ; or a newline), or remove it and run setup again", CommandText(cmd), why)
}

// AllTagsPushes fails when a command of cmds is `docker push -a|--all-tags <repo>` (also
// `docker image push`) for a repository whose scan tag is in scanTags, or for one that may be:
// it would push the local scan tag, which must never be pushed (R5, Batch 5 review). Only two
// literal repositories that differ are told apart.
func AllTagsPushes(cmds []shell.Command, scanTags []string, line func(shell.Command) int) error {
	var repos []string
	for _, t := range scanTags {
		v := ShellValue(t)
		if i := strings.LastIndex(v, ":shearbolt-"); i > 0 {
			repos = append(repos, v[:i])
		}
	}
	if len(repos) == 0 {
		return nil
	}
	for _, cmd := range cmds {
		repo, ok := allTagsPush(cmd)
		if !ok {
			continue
		}
		for _, r := range repos {
			if repo == r || strings.ContainsAny(repo+r, "$`") {
				return Errorf(line(cmd), "%s pushes every tag of %s (--all-tags), the local scan tag %s:shearbolt-... included, which must never be pushed; push the tags by name instead", CommandText(cmd), repo, r)
			}
		}
	}
	return nil
}

// allTagsPush returns the repository of `docker [image] push -a|--all-tags <repo>`.
func allTagsPush(cmd shell.Command) (string, bool) {
	args := cmd.ArgWords()
	if len(args) < 2 || path.Base(args[0].Value) != "docker" {
		return "", false
	}
	rest := args[1:]
	switch {
	case rest[0].Value == "push":
		rest = rest[1:]
	case len(rest) > 1 && rest[0].Value == "image" && rest[1].Value == "push":
		rest = rest[2:]
	default:
		return "", false
	}
	all, repo := false, ""
	for _, w := range rest {
		name, val, eq := strings.Cut(w.Value, "=")
		switch {
		case name == "-a" || name == "--all-tags":
			all = !eq || !strings.EqualFold(val, "false")
		case strings.HasPrefix(w.Value, "-"):
		default:
			repo = w.Value
		}
	}
	return repo, all && repo != ""
}

// ErrexitOff returns the set command after which errexit may still be off at the end of cmds:
// a set +e (or +o errexit) that no later top-level set -e turns back on. A set at Depth > 0 may
// not run, so it can turn errexit off but never back on.
func ErrexitOff(cmds []shell.Command) *shell.Command {
	var off *shell.Command
	for _, cmd := range cmds {
		a := cmd.Argv()
		if len(a) == 0 || a[0] != "set" {
			continue
		}
		for i := 1; i < len(a); i++ {
			v, turn := a[i], 0 // -1 off, 1 on
			switch {
			case v == "--":
				i = len(a)
			case v == "-o" || v == "+o":
				if i+1 < len(a) && a[i+1] == "errexit" {
					turn = map[byte]int{'-': 1, '+': -1}[v[0]]
				}
				i++
			case strings.HasPrefix(v, "+") && strings.Contains(v, "e"):
				turn = -1
			case strings.HasPrefix(v, "-") && strings.Contains(v, "e"):
				turn = 1
			}
			switch {
			case turn < 0:
				off = &cmd
			case turn > 0 && cmd.Depth == 0:
				off = nil
			}
		}
	}
	return off
}

// InstallShell returns the index of the sh (or bash) that the install command c of cmds pipes
// into, such as `curl … | sh` or `(curl … || wget …) | SHEARBOLT_VERSION=1.2.3 sh`, or -1. That
// sh is part of the install, not a command of its own.
func InstallShell(cmds []shell.Command, c int) int {
	for k := c; k+1 < len(cmds); k++ {
		switch cmds[k].Op {
		case "|":
			if a := cmds[k+1].Argv(); len(a) == 1 && (a[0] == "sh" || a[0] == "bash") {
				return k + 1
			}
			return -1
		case "||", "&&", "":
			continue // inside the (curl || wget) group, or before its closing )
		}
		return -1
	}
	return -1
}

// closers end a compound command; they are not commands that "follow" a build.
var closers = []string{"fi", "done", "esac", "}", ")"}

// Follows reports whether a command comes after command at (P1) that is not a closer, one of
// setup's own lines, or one of the gated builds in skip.
func Follows(cmds []shell.Command, at int, skip map[int]bool) bool {
	for k := at + 1; k < len(cmds); k++ {
		c := cmds[k]
		if len(c.Words) == 1 && slices.Contains(closers, c.Words[0].Raw) || OwnLine(c) || skip[k] {
			continue
		}
		return true
	}
	return false
}

// OwnLine reports a line setup adds: the output line or the railpack docker tag.
func OwnLine(c shell.Command) bool {
	a := c.Argv()
	return len(a) >= 2 && a[0] == "echo" && strings.HasPrefix(a[1], "shearbolt-image") ||
		len(a) == 4 && a[0] == "docker" && a[1] == "tag" && strings.Contains(a[3], ":shearbolt-")
}

// SplitPoints returns where part 1 ends and the remainder starts for a split after cmd.
func SplitPoints(script string, cmd shell.Command) (cut, rest int) {
	if cmd.Op == "&&" || cmd.Op == ";" {
		i := cmd.End
		for i < len(script) && (script[i] == ' ' || script[i] == '\t') {
			i++
		}
		if strings.HasPrefix(script[i:], cmd.Op) {
			i += len(cmd.Op)
		}
		for i < len(script) && strings.IndexByte(" \t\r\n", script[i]) >= 0 {
			i++
		}
		return cmd.End, i
	}
	le := lineEnd(script, cmd.End)
	return le, min(le+1, len(script))
}

// TagEdit returns the edit that adds scanTag to docker or buildx build img in a script of
// scriptLen bytes (R5): ` -t <tag>` after the word holding the first tag's value, or after the
// build subcommand when there is no tag. needed is false when a tag holding :shearbolt- is
// already there.
func TagEdit(img *plan.Image, scanTag string, scriptLen, line int) (ed Edit, needed bool, err error) {
	if HasScanTag(img) {
		return Edit{}, false, nil
	}
	at := img.TagEnd
	if at == 0 {
		at = img.SubEnd
	}
	if at <= 0 || at > scriptLen {
		return Edit{}, false, Errorf(line, "setup could not find where to add the tag %s to this build", scanTag)
	}
	return Edit{At: at, End: at, Text: " -t " + scanTag}, true, nil
}

// LoadEdit returns the edit that adds --load after the subcommand of a buildx build that neither
// loads nor pushes (R2), so a docker-container builder leaves the image in the daemon.
func LoadEdit(img *plan.Image) (Edit, bool) {
	if img.Tool == "buildx" && !img.Load && !img.Push && img.SubEnd > 0 {
		return Edit{At: img.SubEnd, End: img.SubEnd, Text: " --load"}, true
	}
	return Edit{}, false
}

// PushEdits converts gated --push build img, run by cmd (R2): every --push word becomes --load,
// and pushes are the `docker push <tag>` lines that push its original tags after the gate (a tag
// holding :shearbolt- is never pushed). A build without a -t tag, or with an argument that may
// expand to several words, fails: converting it would drop the push silently.
func PushEdits(cmd shell.Command, img *plan.Image, line int) (edits []Edit, pushes []string, err error) {
	if len(img.PushArgs) == 0 {
		return nil, nil, Errorf(line, "setup could not find the --push option of this build")
	}
	// The push moves to docker push lines for the tags as written; tags setup cannot read
	// exactly would be pushed no more.
	if len(img.Tags) == 0 {
		return nil, nil, Errorf(line, "this --push build has no -t tag, so setup cannot push its image after the gate")
	}
	args := cmd.ArgWords()
	for i, w := range args {
		if multiWord.MatchString(w.Raw) || MayExpand(w.Raw) && !flagValue(args, i) {
			return nil, nil, Errorf(line, "this --push build has an argument that may expand to several words (%s), so setup cannot tell which tags to push after the gate", w.Raw)
		}
	}
	for _, a := range img.PushArgs {
		edits = append(edits, Edit{At: a[0], End: a[1], Text: "--load"})
	}
	for _, t := range img.Tags {
		if !strings.Contains(ShellValue(t), ":shearbolt-") {
			pushes = append(pushes, "docker push "+t)
		}
	}
	return edits, pushes, nil
}

// attestFlags are the build options that ask BuildKit for attestations.
var attestFlags = []string{"--sbom", "--provenance", "--attest"}

// AttestationFlags fails a build that asks for attestations (--sbom, --provenance or --attest,
// alone or as =value) where setup would load its image for the gate: attestations break with
// --load on the classic image store, and dropping them silently would weaken the supply chain
// (Batch 4 review decision). --sbom=false and --provenance=false are accepted.
func AttestationFlags(args []shell.Word, line int) error {
	for _, w := range args {
		name, val, eq := strings.Cut(w.Value, "=")
		if !slices.Contains(attestFlags, name) || eq && name != "--attest" && strings.EqualFold(val, "false") {
			continue
		}
		return Errorf(line, "this build asks for attestations (%s), which setup cannot keep when it loads the image for the gate; remove the option, or build and push after the gate yourself", w.Raw)
	}
	return nil
}

// MayExpand reports whether a shell word may expand to several words, or to words setup cannot
// read: an unquoted $ expansion or command substitution, or "$@" / "${name[@]}" in double quotes.
// A GitHub ${{ ... }} expression is filled in before the shell runs, so it counts as text.
func MayExpand(raw string) bool {
	inDouble := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case strings.HasPrefix(raw[i:], "${{"):
			end := strings.Index(raw[i:], "}}")
			if end < 0 {
				return true
			}
			i += end + 1
		case c == '\\':
			i++
		case c == '\'' && !inDouble:
			end := strings.IndexByte(raw[i+1:], '\'')
			if end < 0 {
				return true
			}
			i += end + 1
		case c == '"':
			inDouble = !inDouble
		case c == '`':
			return true
		case c == '$' && i+1 < len(raw):
			next := raw[i+1:]
			if inDouble {
				if next[0] == '@' || strings.HasPrefix(next, "{@}") || arrayAll.MatchString(next) {
					return true
				}
				continue
			}
			if strings.IndexByte("{(@*#?!$-_0123456789", next[0]) >= 0 || next[0] >= 'a' && next[0] <= 'z' || next[0] >= 'A' && next[0] <= 'Z' {
				return true
			}
		}
	}
	return false
}

// valueFlags are the docker build options whose value may hold an unquoted expansion in a
// --push build: an expansion there cannot add tags without breaking the build, and the push
// line repeats a tag word as written, so it expands the same way.
var valueFlags = []string{"-t", "--tag", "-f", "--file", "--build-arg", "--platform", "--label", "--target",
	"--cache-from", "--cache-to", "--secret", "--ssh", "--build-context"}

// flagValue reports whether args[i] is the value of one of valueFlags: the word after the flag,
// or the value part of --flag=value.
func flagValue(args []shell.Word, i int) bool {
	if i > 0 && args[i-1].Raw == args[i-1].Value && slices.Contains(valueFlags, args[i-1].Value) {
		return true
	}
	name, _, eq := strings.Cut(args[i].Raw, "=")
	return eq && strings.HasPrefix(name, "--") && slices.Contains(valueFlags, name)
}

// multiWord matches the expansions that always yield one word per element: $@, $*, ${@}, ${*}
// and ${name[@]} / ${name[*]}.
var multiWord = regexp.MustCompile(`\$[@*]|\$\{[@*]|\$\{[A-Za-z_][A-Za-z0-9_]*\[[@*]\]`)

// arrayAll matches the start of ${name[@]}, which expands to one word per element.
var arrayAll = regexp.MustCompile(`^\{[A-Za-z_][A-Za-z0-9_]*\[@\]`)

// HasScanTag reports whether a tag of shell build img holds :shearbolt-.
func HasScanTag(img *plan.Image) bool {
	return slices.ContainsFunc(img.Tags, func(t string) bool { return strings.Contains(ShellValue(t), ":shearbolt-") })
}

// RailpackTagged reports whether a `docker tag <name> ...:shearbolt-...` command follows the
// railpack build at command c of cmds.
func RailpackTagged(cmds []shell.Command, c int, img *plan.Image) bool {
	name := ShellValue(img.Tags[0])
	for _, cmd := range cmds[c+1:] {
		a := cmd.Argv()
		if len(a) == 4 && a[0] == "docker" && a[1] == "tag" && a[2] == name && strings.Contains(a[3], ":shearbolt-") {
			return true
		}
	}
	return false
}

// ShellValue returns a shell word's value without its quotes ($ expansions kept).
func ShellValue(word string) string {
	cmds, _ := shell.Parse(word, shell.State{})
	if len(cmds) == 1 && len(cmds[0].Words) == 1 {
		return cmds[0].Words[0].Value
	}
	return word
}
