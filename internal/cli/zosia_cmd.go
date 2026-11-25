package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const zosiaUsage = `Usage: shearbolt zosia enable [--policy] [--projects <path>[,<path>...] | --all-projects] [--connect] [--secret-name <name>]
       shearbolt zosia disable [--policy]

Run it in the repository root, after "shearbolt setup" has added the gate.

  enable   Makes the Shearbolt gates in this repository's CI send their reports to Zosia
           (https://zosiasec.com). On GitHub Actions it adds the zosia-secret input to the
           shearbolt steps, and ZOSIA_SECRET to the env of run: steps that run shearbolt; GitLab
           CI and Bitbucket Pipelines need no change, only the CI variable ZOSIA_SECRET.

           On a terminal it then has Zosia make the secret: it asks for a name, shows a link and
           a code, and waits up to 10 minutes while you approve the code where you are logged in
           to Zosia. In a repository with only GitHub workflows whose secrets the GitHub CLI (gh)
           can set, it stores the secret as the repository secret ZOSIA_SECRET through gh;
           everywhere else it shows the secret once and says where to put it. Where it cannot tell
           whether a secret is there, it asks before it makes one, and before it replaces one that
           is. With ZOSIA_SECRET in the environment it makes none and uses that one.

           With a secret at hand it asks Zosia how many projects your plan has room for. A
           project is one shearbolt.yaml that a gate reads. When more of them send than fit, it
           lists them and asks which send; each of the others gets "zosia:" with "send: false"
           below it in its shearbolt.yaml, and sends nothing from then on. A project whose gate
           runs a Shearbolt release before 1.12.0 cannot be switched off this way and goes on
           sending; Zosia refuses the reports it has no place for, which fails no build.

           Its last line names the gates that still cannot send, such as one that pins a
           Shearbolt release from before 1.4.0.
  disable  Removes what enable added to the CI files again, and the policy line. The secret
           itself stays until you delete it, and so does a "send: false".

  --policy With enable: the projects also take their policy from Zosia. It adds "zosia:" with
           "policy: true" below it to each shearbolt.yaml that a gate reads, so that a CI scan
           asks Zosia for the policy and uses it in place of the file's policy block. The block
           is not changed, and a scan that gets no policy from Zosia uses it. Needs Shearbolt
           1.11.0 in the gate. With disable: removes only that line, and reports go on.
  --projects <path>[,<path>...]
           The shearbolt.yaml files whose projects send; every other project of the repository
           gets "send: false". With the plan's room known, a choice that does not fit is an
           error and switches nothing.
  --all-projects
           Every project sends: removes each "send: false".
  --connect
           Makes the secret without asking anything, with or without a terminal: prints the link
           and the code and waits. It replaces a secret that is there, and needs gh to store the
           new one, which it never prints.
  --secret-name <name>
           The name of the secret in Zosia, in place of the question. At most 64 characters.

All of it writes the files directly and shows each change as a diff. It never commits.

Exit codes: 0 success, 2 error, 3 a selection is needed and there is no terminal to ask on
(the output lists the projects; run it again with --projects).
`

// zosiaOpts is what the arguments of `shearbolt zosia` say.
type zosiaOpts struct {
	on, policy, connect, all bool
	projects                 []string // nil unless --projects was given
	secretName               string
}

func parseZosiaArgs(args []string) (zosiaOpts, bool) {
	var o zosiaOpts
	if len(args) == 0 || (args[0] != "enable" && args[0] != "disable") {
		return o, false
	}
	o.on = args[0] == "enable"
	rest := args[1:]
	// value is what a flag that takes one is given, as "--flag value" or "--flag=value".
	value := func(i *int, name string) (string, bool) {
		if v, ok := strings.CutPrefix(rest[*i], name+"="); ok {
			return v, v != ""
		}
		if *i+1 >= len(rest) {
			return "", false
		}
		*i++
		return rest[*i], rest[*i] != ""
	}
	is := func(a, name string) bool { return a == name || strings.HasPrefix(a, name+"=") }
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--policy" && !o.policy:
			o.policy = true
		case !o.on:
			return o, false
		case a == "--connect":
			o.connect = true
		case a == "--all-projects":
			o.all = true
		case is(a, "--projects") && o.projects == nil:
			v, ok := value(&i, "--projects")
			if !ok {
				return o, false
			}
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p == "" {
					return o, false
				}
				o.projects = append(o.projects, filepath.ToSlash(filepath.Clean(p)))
			}
		case is(a, "--secret-name"):
			v, ok := value(&i, "--secret-name")
			if v = strings.TrimSpace(v); !ok || v == "" || len([]rune(v)) > maxSecretName {
				return o, false
			}
			o.secretName = v
		default:
			return o, false
		}
	}
	return o, !(o.all && o.projects != nil)
}

func runZosia(args []string, stdout, stderr io.Writer, d deps) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprint(stdout, zosiaUsage)
		return 0
	}
	o, ok := parseZosiaArgs(args)
	if !ok {
		fmt.Fprint(stderr, zosiaUsage)
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "shearbolt zosia: %v\n", err)
		return 2
	}
	p := setup.Zosia(root, o.on, o.policy)
	if p.Error != nil {
		fmt.Fprintf(stderr, "shearbolt zosia: %s\n", noteLine(*p.Error)[2:])
		return 2
	}
	if len(p.Changes) > 0 {
		if err := setup.Write(root, p); err != nil {
			fmt.Fprintf(stderr, "shearbolt zosia: %v\n", err)
			return 2
		}
	}
	s := setupRun{stdout: stdout, stderr: stderr, d: d}
	if o.on {
		return s.zosiaEnable(root, p, o)
	}
	s.printChanges(p)
	s.printNotes(p)
	if len(p.Changes) > 0 {
		fmt.Fprintf(stdout, "\n%s\n", zosiaReview)
	}
	return 0
}

const zosiaReview = "Review the change with git diff, then commit it. Shearbolt never commits."

func runGH(dir string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, stdin, stdout, stderr
	return cmd.Run()
}

// canPaste reports whether the user can paste a secret here: gh hides it only when its output is
// a terminal too.
func (s setupRun) canPaste() bool {
	if f, isFile := s.stdout.(*os.File); isFile && !isTerminal(f) {
		return false
	}
	return s.d.stdinTTY()
}

// secretRepo asks the GitHub CLI about the repository's ZOSIA_SECRET: ok when every CI file is a
// GitHub workflow and gh is logged in to an account that may set the repository's secrets
// (collaborator access, GitHub's rule for the secrets API). It returns the repository as
// OWNER/NAME and whether the secret exists already. Any failure means ok is false: the note that
// says where to add the secret by hand stays.
func (s setupRun) secretRepo(root string, p *plan.Plan) (repo string, has, ok bool) {
	if slices.ContainsFunc(p.CI, func(c plan.CIFile) bool { return c.System != "github" }) {
		return "", false, false
	}
	var out bytes.Buffer
	var view struct{ NameWithOwner, ViewerPermission string }
	if s.d.gh(root, nil, &out, io.Discard, "repo", "view", "--json", "nameWithOwner,viewerPermission") != nil ||
		json.Unmarshal(out.Bytes(), &view) != nil || view.NameWithOwner == "" ||
		!slices.Contains([]string{"ADMIN", "MAINTAIN", "WRITE"}, view.ViewerPermission) {
		return "", false, false
	}
	out.Reset()
	var secrets []struct{ Name string }
	if s.d.gh(root, nil, &out, io.Discard, "secret", "list", "--repo", view.NameWithOwner, "--json", "name") != nil ||
		json.Unmarshal(out.Bytes(), &secrets) != nil {
		return "", false, false
	}
	for _, sec := range secrets {
		has = has || sec.Name == "ZOSIA_SECRET"
	}
	return view.NameWithOwner, has, true
}
