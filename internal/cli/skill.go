package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/skill"
)

const skillUsage = `Usage: shearbolt skill install [--agent claude|codex|all] [--project]

Installs the coding-agent skill, which adds setup and fix: /shearbolt setup in Claude
Code, $shearbolt setup in Codex. On a terminal it asks which agent to install for;
--agent answers that. Without --agent, and with its input or output not a terminal, it
installs for Claude Code.

  Claude Code  ~/.claude/skills/shearbolt, with --project .claude/skills/shearbolt
  Codex        ~/.agents/skills/shearbolt, with --project .agents/skills/shearbolt

The folder in your home directory serves all your projects; --project writes into the
current directory, to commit and share with your team. A skill installed earlier by
shearbolt is updated; a different skill with the same name is left alone.

Exit codes: 0 success, 2 error.

Flags:
`

func runSkill(args []string, stdout, stderr io.Writer, d deps) int {
	flags := flag.NewFlagSet("skill install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, skillUsage); flags.PrintDefaults() }
	project := flags.Bool("project", false, "install into the current directory instead of your home directory")
	// --global was needed for a user install in 1.0.0; it stays accepted so scripts keep working.
	global := flags.Bool("global", false, "install into your home directory (the default)")
	agentFlag := flags.String("agent", "", "claude, codex or all")
	usageError := func(msg string) int {
		if msg != "" {
			fmt.Fprintf(stderr, "shearbolt skill: %s\n\n", msg)
		}
		flags.Usage()
		return 2
	}
	switch {
	case len(args) == 0:
		return usageError("")
	case args[0] != "install":
		return usageError(fmt.Sprintf("unknown subcommand %q", args[0]))
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		return usageError(fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
	}
	if *project && *global {
		return usageError("--project and --global cannot be combined")
	}
	// name is what --agent would say; without the flag and without a terminal it is Claude Code,
	// because skills installed before 1.10.0 update themselves with this command and are all
	// Claude Code's. suffix repeats the choice in the commands a message tells the user to run.
	name, suffix := string(skill.Claude), ""
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "agent" {
			name, suffix = *agentFlag, " --agent "+*agentFlag
		}
	})
	if _, known := skillAgents(name); !known {
		return usageError(fmt.Sprintf("--agent must be claude, codex or all, found %q", name))
	}

	fail := func(msg string) int {
		fmt.Fprintf(stderr, "shearbolt skill: %s\n", termSafe(msg, false))
		return 2
	}
	var base, again string
	if !*project {
		base = d.getenv("HOME")
		switch {
		case base == "":
			return fail("cannot find your home directory: HOME is not set. Run shearbolt skill install --project" + suffix + " to install into this project instead.")
		case !filepath.IsAbs(base):
			return fail(fmt.Sprintf("HOME (%q) is not an absolute path, so the user skill folder is unknown", base))
		}
		again = "shearbolt skill install"
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return fail(err.Error())
		}
		base, again = cwd, "shearbolt skill install --project"
	}
	if suffix == "" && skillCanAsk(stdout, d) {
		var err error
		if name, err = askSkillAgent(d.stdin, stdout); err != nil {
			return fail(err.Error() + ". Nothing was written.")
		}
		suffix = " --agent " + name
	}
	again += suffix
	agents, _ := skillAgents(name)

	for _, a := range agents {
		dir := skill.Dir(base, a)
		shown := dir
		var outcome skill.Outcome
		var err error
		if !*project {
			outcome, err = skill.Install(dir)
		} else {
			shown = skill.Dir("", a)
			outcome, err = skill.InstallInside(dir, base)
		}
		switch {
		case errors.Is(err, skill.ErrForeign):
			return fail(fmt.Sprintf("%s already holds a SKILL.md that shearbolt did not write; remove or rename that folder, then run %s again.", shown, again))
		case errors.Is(err, skill.ErrOutside):
			return fail(err.Error() + ". Nothing was written there. Remove that symlink, or install for your user without --project.")
		case err != nil:
			return fail(err.Error())
		}
		shown = termSafe(shown, false)
		switch outcome {
		case skill.Installed:
			fmt.Fprintf(stdout, "Installed the Shearbolt skill in %s. In %s, type %s setup.\n", shown, a.Label(), a.Command())
		case skill.Updated:
			fmt.Fprintf(stdout, "Updated the Shearbolt skill in %s. In %s, type %s setup.\n", shown, a.Label(), a.Command())
		default:
			fmt.Fprintf(stdout, "The Shearbolt skill in %s is up to date.\n", shown)
		}
		if *project {
			fmt.Fprintf(stdout, "Commit %s to share it with your team.\n", shown)
		}
	}
	return 0
}

func skillAgents(name string) ([]skill.Agent, bool) {
	if name == "all" {
		return skill.Agents, true
	}
	if a := skill.Agent(name); slices.Contains(skill.Agents, a) {
		return []skill.Agent{a}, true
	}
	return nil, false
}

// With stdout in a file or a pipe nobody would see the question.
func skillCanAsk(stdout io.Writer, d deps) bool {
	if f, isFile := stdout.(*os.File); isFile && !isTerminal(f) {
		return false
	}
	return d.stdinTTY()
}

func askSkillAgent(stdin io.Reader, stdout io.Writer) (string, error) {
	fmt.Fprint(stdout, "Install the Shearbolt skill for:\n")
	var options []string
	for _, a := range skill.Agents {
		options = append(options, string(a))
		fmt.Fprintf(stdout, "  %d) %s\n", len(options), a.Label())
	}
	options = append(options, "all")
	fmt.Fprintf(stdout, "  %d) all\n", len(options))
	in := bufio.NewReaderSize(stdin, maxAnswer+2)
	for try := 1; ; try++ {
		fmt.Fprintf(stdout, "Choose one [%d]: ", len(options))
		line, err := readLine(in)
		if err != nil && !errors.Is(err, errLongAnswer) {
			fmt.Fprintln(stdout)
			return "", fmt.Errorf("no answer to which agent to install for: %s", stdinProblem(err))
		}
		if err == nil {
			answer := strings.ToLower(line)
			if answer == "" {
				return "all", nil
			}
			for i, o := range options {
				if answer == o || answer == strconv.Itoa(i+1) {
					return o, nil
				}
			}
		}
		fmt.Fprintf(stdout, "  Answer with a number from 1 to %d.\n", len(options))
		if try >= setupMaxTries {
			return "", fmt.Errorf("no valid answer to which agent to install for after %d tries", setupMaxTries)
		}
	}
}
