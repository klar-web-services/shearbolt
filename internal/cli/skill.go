package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klar-web-services/shearbolt/internal/skill"
)

const skillUsage = `Usage: shearbolt skill install [--global]

Installs the coding-agent skill for Claude Code, which adds /shearbolt setup and
/shearbolt fix. It goes into .claude/skills/shearbolt in the current directory, or with
--global into ~/.claude/skills/shearbolt. A skill installed earlier by shearbolt is
updated; a different skill with the same name is left alone.

Exit codes: 0 success, 2 error.

Flags:
`

// projectSkillDir is the project install's folder as shown to the user.
var projectSkillDir = filepath.Join(".claude", "skills", skill.Name)

func runSkill(args []string, stdout, stderr io.Writer, d deps) int {
	flags := flag.NewFlagSet("skill install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, skillUsage); flags.PrintDefaults() }
	global := flags.Bool("global", false, "install into ~/.claude/skills instead of the current directory")
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

	fail := func(msg string) int {
		fmt.Fprintf(stderr, "shearbolt skill: %s\n", termSafe(msg, false))
		return 2
	}
	var base, shown, again string
	if *global {
		base = d.getenv("HOME")
		switch {
		case base == "":
			return fail("cannot find your home directory: HOME is not set")
		case !filepath.IsAbs(base):
			return fail(fmt.Sprintf("HOME (%q) is not an absolute path, so the global skill folder is unknown", base))
		}
		shown, again = skill.Dir(base), "shearbolt skill install --global"
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return fail(err.Error())
		}
		base, shown, again = cwd, projectSkillDir, "shearbolt skill install"
	}

	var outcome skill.Outcome
	var err error
	if *global {
		// Follow symlinks anywhere: dotfiles layouts link ~/.claude elsewhere.
		outcome, err = skill.Install(skill.Dir(base))
	} else {
		outcome, err = skill.InstallInside(skill.Dir(base), base)
	}
	switch {
	case errors.Is(err, skill.ErrForeign):
		return fail(fmt.Sprintf("%s already holds a SKILL.md that shearbolt did not write; remove or rename that folder, then run %s again.", shown, again))
	case errors.Is(err, skill.ErrOutside):
		return fail(err.Error() + ". Nothing was written. Remove that symlink, or install for your user with --global.")
	case err != nil:
		return fail(err.Error())
	}
	shown = termSafe(shown, false)
	switch outcome {
	case skill.Installed:
		fmt.Fprintf(stdout, "Installed the Shearbolt skill in %s. In Claude Code, type /shearbolt setup.\n", shown)
	case skill.Updated:
		fmt.Fprintf(stdout, "Updated the Shearbolt skill in %s. In Claude Code, type /shearbolt setup.\n", shown)
	default:
		fmt.Fprintf(stdout, "The Shearbolt skill in %s is up to date.\n", shown)
	}
	if !*global {
		fmt.Fprintf(stdout, "Commit %s to share it with your team.\n", shown)
	}
	return 0
}
