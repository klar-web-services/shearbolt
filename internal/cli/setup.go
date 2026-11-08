package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const setupUsage = `Usage: shearbolt setup [--yes] [--plan] [--apply] [--json] [--answer id=value ...]

Adds the Shearbolt gate to this repository's CI. Setup finds the npm and Rust projects in the
current directory, which it treats as the repository root, writes a shearbolt.yaml for each,
and edits the CI so the gate runs after each build and before any push or deploy. It shows
every change as a diff and never commits.

Without flags it asks its questions on the terminal, then asks before writing.

Exit codes: 0 success (also when you decline), 2 error.

Flags:
`

const (
	setupNeedsTerminal = "setup needs a terminal; use --yes, or --plan --json and --apply --answer"
	setupReview        = "Review the changes with git status and git diff (new files show only in git status), then commit them. Setup never commits."
	setupMaxTries      = 3
	maxAnswer          = 4 << 10 // longest answer line read from stdin, in bytes
)

// errLongAnswer is an answer line longer than maxAnswer: an invalid answer.
var errLongAnswer = errors.New("the answer is longer than 4 KiB")

// ciSystemNames are the CI systems' names in human output.
var ciSystemNames = map[string]string{
	"github":    "GitHub Actions",
	"bitbucket": "Bitbucket Pipelines",
	"gitlab":    "GitLab CI",
}

// setupRun holds the output side of one setup run.
type setupRun struct {
	stdout, stderr io.Writer
	d              deps
	json           bool
}

func runSetup(args []string, stdout, stderr io.Writer, d deps) int {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, setupUsage); flags.PrintDefaults() }
	yes := flags.Bool("yes", false, "accept the defaults of unanswered questions and write without asking")
	planOnly := flags.Bool("plan", false, "print the plan and write nothing")
	apply := flags.Bool("apply", false, "write the plan; every applicable question needs an --answer, unless --yes is given")
	jsonOut := flags.Bool("json", false, "with --plan or --apply: print the plan, or the error, as JSON")
	var answerArgs stringList
	flags.Var(&answerArgs, "answer", "answer a question as `id=value` (repeatable); --plan --json lists the questions")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageError := func(msg string) int {
		fmt.Fprintf(stderr, "shearbolt setup: %s\n", msg)
		return 2
	}
	switch {
	case flags.NArg() > 0:
		return usageError(fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
	case *planOnly && *apply:
		return usageError("use either --plan or --apply, not both")
	case *jsonOut && !*planOnly && !*apply:
		return usageError("--json needs --plan or --apply")
	}

	s := setupRun{stdout: stdout, stderr: stderr, d: d, json: *jsonOut}
	answers, err := plan.ParseAnswers(answerArgs)
	if err != nil {
		return s.fail(nil, "BAD_ANSWER", err.Error())
	}
	interactive := !*planOnly && !*apply && !*yes
	if interactive && !d.stdinTTY() {
		return usageError(setupNeedsTerminal)
	}
	root, err := os.Getwd()
	if err != nil {
		return s.fail(nil, "READ", err.Error())
	}
	o := setup.Options{Root: root, Answers: answers, Yes: *yes, Version: d.version}
	if interactive {
		return s.interactive(root, o)
	}

	p := setup.Compute(o)
	if p.Error != nil {
		return s.failPlan(p)
	}
	if *planOnly {
		if s.json {
			if err := s.printJSON(p); err != nil {
				fmt.Fprintf(stderr, "shearbolt setup: cannot print the plan as JSON: %v\n", err)
				return 2
			}
			return 0
		}
		s.printFound(p)
		s.printQuestions(p, answers, *yes)
		s.printChanges(p)
		s.printNotes(p)
		if len(p.Changes) == 0 {
			fmt.Fprintf(stdout, "\n%s\n", nothingToChange(p))
		} else {
			fmt.Fprintln(stdout, "\nNo changes written (--plan).")
		}
		return 0
	}
	if *apply && !*yes {
		_, missing, err := plan.Resolve(p.Questions, answers, false)
		if err != nil {
			return s.fail(p, "BAD_ANSWER", err.Error())
		}
		if len(missing) > 0 {
			return s.fail(p, "UNANSWERED", fmt.Sprintf(
				"these questions have no answer: %s. Answer each with --answer id=value (setup --plan --json lists them and their options), or add --yes to accept their defaults.",
				strings.Join(missing, ", ")))
		}
	}
	return s.write(root, p, true)
}

// interactive asks the applicable open questions on the terminal, one at a time, re-evaluating
// which questions apply after each answer; then it shows the plan and asks before writing.
func (s setupRun) interactive(root string, o setup.Options) int {
	p := setup.Compute(o)
	if p.Error != nil {
		return s.failPlan(p)
	}
	s.printFound(p)
	in := bufio.NewReaderSize(s.d.stdin, maxAnswer+2) // room for the answer and its CR LF
	answers := maps.Clone(o.Answers)
	if answers == nil {
		answers = map[string]string{}
	}
	asked := false
	for {
		_, missing, err := plan.Resolve(p.Questions, answers, false)
		if err != nil {
			return s.fail(p, "BAD_ANSWER", err.Error())
		}
		if len(missing) == 0 {
			break
		}
		i := slices.IndexFunc(p.Questions, func(q plan.Question) bool { return q.ID == missing[0] })
		v, err := s.ask(in, p.Questions, answers, p.Questions[i])
		if err != nil {
			return s.fail(p, "BAD_ANSWER", err.Error())
		}
		answers[p.Questions[i].ID] = v
		asked = true
	}
	if asked {
		o.Answers = answers
		if p = setup.Compute(o); p.Error != nil {
			return s.failPlan(p)
		}
	}
	s.printChanges(p)
	s.printNotes(p)
	if len(p.Changes) == 0 {
		s.finish(p)
		return 0
	}
	fmt.Fprint(s.stdout, "\nApply these changes? [y/N] ")
	line, err := readLine(in)
	switch {
	case errors.Is(err, errLongAnswer):
		line = "" // not a yes
	case err != nil:
		fmt.Fprintln(s.stdout, "\nNo changes written.")
		fmt.Fprintf(s.stderr, "shearbolt setup: no answer to \"Apply these changes?\" (%s), so nothing was written\n", stdinProblem(err))
		return 2
	}
	if l := strings.ToLower(line); l != "y" && l != "yes" {
		fmt.Fprintln(s.stdout, "No changes written.")
		return 0
	}
	return s.write(root, p, false)
}

// ask asks question q until it gets a valid answer, at most setupMaxTries times. Options are
// numbered; an answer is option numbers or values (several, comma-separated, for a multi
// question), and an empty line takes the default.
func (s setupRun) ask(in *bufio.Reader, qs []plan.Question, answers map[string]string, q plan.Question) (string, error) {
	fmt.Fprintf(s.stdout, "\n%s\n", termSafe(q.Prompt, false))
	for i, o := range q.Options {
		fmt.Fprintf(s.stdout, "  %d) %s\n", i+1, termSafe(o.Label, false))
	}
	prompt := "Choose one"
	if q.Kind == "multi" {
		prompt = "Choose one or more (comma-separated)"
	}
	for try := 1; ; try++ {
		fmt.Fprintf(s.stdout, "%s [%s]: ", prompt, termSafe(optionNumbers(q, q.Default), false))
		line, err := readLine(in)
		if err != nil && !errors.Is(err, errLongAnswer) {
			fmt.Fprintln(s.stdout)
			return "", fmt.Errorf("no answer to question %s: %s", q.ID, stdinProblem(err))
		}
		var v string
		if err == nil {
			v, err = checkAnswer(qs, answers, q, line)
		}
		if err == nil {
			return v, nil
		}
		fmt.Fprintf(s.stdout, "  %s\n", termSafe(err.Error(), false))
		if try >= setupMaxTries {
			return "", fmt.Errorf("no valid answer to question %s after %d tries", q.ID, setupMaxTries)
		}
	}
}

// checkAnswer turns a typed line into q's value (an empty line is the default) and validates it
// with Resolve, next to the answers given so far.
func checkAnswer(qs []plan.Question, answers map[string]string, q plan.Question, line string) (string, error) {
	v := q.Default
	if line != "" {
		v = answerValue(q, line)
	}
	trial := maps.Clone(answers)
	trial[q.ID] = v
	if _, _, err := plan.Resolve(qs, trial, false); err != nil {
		return "", err
	}
	return v, nil
}

// readLine reads one line without its line ending and surrounding spaces. A last line without a
// newline counts; io.EOF means there was nothing left. A line longer than maxAnswer gives
// errLongAnswer after at most the reader's buffer size (maxAnswer+2) is read, so an endless
// stdin (`setup </dev/zero`) uses bounded memory; the rest of that line is read as the next one.
func readLine(in *bufio.Reader) (string, error) {
	b, err := in.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(bytes.TrimRight(b, "\r\n")) > maxAnswer {
		return "", errLongAnswer
	}
	if err != nil && (!errors.Is(err, io.EOF) || len(b) == 0) {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// stdinProblem says why no answer could be read.
func stdinProblem(err error) string {
	if errors.Is(err, io.EOF) {
		return "stdin was closed"
	}
	return "cannot read stdin: " + err.Error()
}

// answerValue turns typed option numbers into option values. The options are shown numbered, so
// a word that is a valid option number selects that option, even when another option's value is
// the same text; any other word is taken as a value.
func answerValue(q plan.Question, line string) string {
	value := func(word string) string {
		word = strings.TrimSpace(word)
		if n, err := strconv.Atoi(word); err == nil && n >= 1 && n <= len(q.Options) {
			return q.Options[n-1].Value
		}
		return word
	}
	if q.Kind != "multi" {
		return value(line)
	}
	words := strings.Split(line, ",")
	for i := range words {
		words[i] = value(words[i])
	}
	return strings.Join(words, ",")
}

// optionNumbers shows a value in --answer syntax as option numbers, or as written when a part
// of it is not an option.
func optionNumbers(q plan.Question, v string) string {
	parts := []string{v}
	if q.Kind == "multi" {
		parts = strings.Split(v, ",")
	}
	nums := make([]string, len(parts))
	for i, part := range parts {
		n := slices.IndexFunc(q.Options, func(o plan.Option) bool { return o.Value == part })
		if n < 0 {
			return v
		}
		nums[i] = strconv.Itoa(n + 1)
	}
	return strings.Join(nums, ",")
}

// write applies p and ends the run. It writes first and prints afterwards, so a failed write
// leaves stdout empty. show prints what was found, the diffs and the notes (interactive mode has
// shown them already). In JSON mode it prints the plan instead, and the closing lines go to
// stderr so stdout stays JSON.
func (s setupRun) write(root string, p *plan.Plan, show bool) int {
	if len(p.Changes) > 0 {
		if err := setup.Write(root, p); err != nil {
			// Write checks every file before it writes any, but then writes them one at a time.
			return s.fail(p, "WRITE", strings.TrimPrefix(err.Error(), "setup: ")+
				" (files are written one at a time, so files before this one may already be written; check git status)")
		}
	}
	switch {
	case s.json:
		if err := s.printJSON(p); err != nil {
			fmt.Fprintf(s.stderr, "shearbolt setup: the changes were written, but the plan cannot be printed as JSON: %v\n", err)
			return 2
		}
	case show:
		s.printFound(p)
		s.printChanges(p)
		s.printNotes(p)
	}
	s.finish(p)
	return 0
}

// finish prints the closing lines of a run that wrote, or had nothing to write.
func (s setupRun) finish(p *plan.Plan) {
	w := s.stdout
	if s.json {
		w = s.stderr
	}
	fmt.Fprintln(w)
	if len(p.Changes) == 0 {
		fmt.Fprintln(w, nothingToChange(p))
	}
	fmt.Fprintln(w, setupReview)
}

// nothingToChange is the line for a plan without changes. It claims that Shearbolt is set up
// only when no note says a gate had to be left for the user to add by hand, or that something
// runs unchecked by a gate (a push before it, a build after it, or an existing gate after a push).
func nothingToChange(p *plan.Plan) string {
	for _, n := range p.Notes {
		switch n.Code {
		case "UNSUPPORTED_EDIT", "NO_GATE_PLACED", "BUILD_AFTER_GATE", "PUSH_BEFORE_GATE", "GATE_AFTER_PUSH":
			return "Setup has nothing more to change automatically; add what the notes above describe by hand."
		}
	}
	return "Shearbolt is already set up here; nothing to change."
}

// fail reports an error with code and message, and returns exit code 2. p is the plan computed
// so far (nil before Compute).
func (s setupRun) fail(p *plan.Plan, code, msg string) int {
	q := plan.Plan{SchemaVersion: 1, ShearboltVersion: s.d.version}
	if p != nil {
		q = *p
	}
	q.Error = &plan.Note{Code: code, Message: msg}
	return s.failPlan(&q)
}

// failPlan reports p.Error, on stderr or, in JSON mode, as the plan JSON. The JSON has no
// changes, because the plan's changes were not applied (after a failed write, some files may
// have been; the WRITE message says so). It returns exit code 2.
func (s setupRun) failPlan(p *plan.Plan) int {
	msg := termSafe(p.Error.Message, true)
	if s.json {
		q := *p
		q.Changes = nil
		if err := s.printJSON(&q); err != nil {
			fmt.Fprintf(s.stderr, "shearbolt setup: %s\nshearbolt setup: cannot print this error as JSON: %v\n", msg, err)
		}
	} else {
		fmt.Fprintf(s.stderr, "shearbolt setup: %s\n", msg)
	}
	return 2
}

// printJSON prints p indented. The encoder does not escape HTML characters, so shell text such
// as `>> "$GITHUB_OUTPUT"` stays readable (json.Marshal would escape it again). An error means
// stdout did not get the plan; the caller then exits 2.
func (s setupRun) printJSON(p *plan.Plan) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return err
	}
	_, err := s.stdout.Write(buf.Bytes())
	return err
}

// printFound prints the projects and CI files setup found.
func (s setupRun) printFound(p *plan.Plan) {
	for _, pr := range p.Projects {
		kind := pr.Ecosystem
		if pr.Framework == "nextjs" {
			kind += ", Next.js"
		}
		fmt.Fprintf(s.stdout, "Found: %s (%s) in %s\n", termSafe(pr.Name, false), termSafe(kind, false), termSafe(pr.Dir, false))
	}
	for _, c := range p.CI {
		name := ciSystemNames[c.System]
		if name == "" {
			name = c.System
		}
		fmt.Fprintf(s.stdout, "CI: %s (%s)\n", termSafe(name, false), termSafe(c.File, false))
	}
}

// printQuestions lists the applicable questions of a --plan with the value setup uses.
func (s setupRun) printQuestions(p *plan.Plan, answers map[string]string, yes bool) {
	values, _, err := plan.Resolve(p.Questions, answers, yes)
	if err != nil || len(values) == 0 {
		return
	}
	fmt.Fprintln(s.stdout, "\nQuestions:")
	for _, q := range p.Questions {
		v, ok := values[q.ID]
		if !ok {
			continue
		}
		how := "default"
		if _, answered := answers[q.ID]; answered {
			how = "answered"
		}
		opts := make([]string, len(q.Options))
		for i, o := range q.Options {
			opts[i] = o.Value
		}
		fmt.Fprintf(s.stdout, "  %s (%s): %s\n    %s\n    values: %s\n",
			termSafe(q.ID, false), how, termSafe(v, false), termSafe(q.Prompt, false), termSafe(strings.Join(opts, ", "), false))
	}
	if !p.Complete {
		fmt.Fprintln(s.stdout, "  Answer with --answer id=value, or add --yes to accept the defaults.")
	}
}

// printChanges prints each change's unified diff.
func (s setupRun) printChanges(p *plan.Plan) {
	for _, c := range p.Changes {
		diff := c.Diff
		if !strings.HasSuffix(diff, "\n") {
			diff += "\n"
		}
		fmt.Fprintf(s.stdout, "\n%s", termSafe(diff, true))
	}
}

// printNotes prints one bullet per note, each message in full: UNSUPPORTED_EDIT and
// NO_GATE_PLACED carry the snippet to add by hand on their following lines.
func (s setupRun) printNotes(p *plan.Plan) {
	if len(p.Notes) == 0 {
		return
	}
	fmt.Fprintln(s.stdout, "\nNotes:")
	for _, n := range p.Notes {
		fmt.Fprintln(s.stdout, noteLine(n))
	}
}

// noteLine is a note's bullet, safe for the terminal: its location (unless the message starts
// with its file), then the message with its line breaks.
func noteLine(n plan.Note) string {
	loc := ""
	switch {
	case n.File == "" || strings.HasPrefix(n.Message, n.File):
	case n.Line > 0:
		loc = fmt.Sprintf("%s:%d: ", n.File, n.Line)
	default:
		loc = n.File + ": "
	}
	return "- " + termSafe(loc, false) + termSafe(n.Message, true)
}

// termSafe makes repository text safe to print on a terminal, so a package name, file name, job
// key or snippet cannot send escape sequences. Control characters (unicode.IsControl, the set
// report.clean replaces with a space) and bytes that are not UTF-8 are shown visibly as Go
// escapes (\x1b, \u0085, \n), so the output shows that the text holds them instead of hiding it.
// With multiline, newlines, tabs and CR LF line endings are kept; a lone CR is escaped, because
// it would let a line overwrite itself.
func termSafe(s string, multiline bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case multiline && (r == '\n' || r == '\t' || r == '\r' && strings.HasPrefix(s[i+1:], "\n")):
			b.WriteRune(r)
		case unicode.IsControl(r):
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}
