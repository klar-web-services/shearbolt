package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const zosiaUsage = `Usage: shearbolt zosia enable|disable

Run it in the repository root, after "shearbolt setup" has added the gate.

  enable   Makes the Shearbolt gates in this repository's CI send their reports to Zosia
           (https://zosiasec.com), then says where to put the secret. On GitHub Actions it adds
           the zosia-secret input to the shearbolt steps; GitLab CI and Bitbucket Pipelines
           need no change, only the CI variable ZOSIA_SECRET. On a terminal, in a repository
           with only GitHub workflows whose secrets the GitHub CLI (gh) can set, it then asks
           for the secret and stores it as the repository secret ZOSIA_SECRET through gh.
  disable  Removes the zosia-secret input again. The secret itself stays until you delete it.

Both write the CI files directly and show the change as a diff. They never commit.

Exit codes: 0 success, 2 error.
`

func runZosia(args []string, stdout, stderr io.Writer, d deps) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprint(stdout, zosiaUsage)
		return 0
	}
	if len(args) != 1 || (args[0] != "enable" && args[0] != "disable") {
		fmt.Fprint(stderr, zosiaUsage)
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "shearbolt zosia: %v\n", err)
		return 2
	}
	p := setup.Zosia(root, args[0] == "enable")
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
	repo, has, offer := "", false, false
	var byHand []plan.Note
	if args[0] == "enable" {
		if repo, has, offer = s.secretRepo(root, p); offer {
			byHand = slices.DeleteFunc(slices.Clone(p.Notes), func(n plan.Note) bool { return n.Code != "ZOSIA_SECRET" })
			p.Notes = slices.DeleteFunc(p.Notes, func(n plan.Note) bool { return n.Code == "ZOSIA_SECRET" })
		}
	}
	s.printChanges(p)
	s.printNotes(p)
	if len(p.Changes) > 0 {
		fmt.Fprintf(stdout, "\n%s\n", zosiaReview)
	}
	if offer && !s.storeSecret(root, repo, has) {
		for _, n := range byHand {
			fmt.Fprintln(stdout, noteLine(n))
		}
	}
	return 0
}

const zosiaReview = "Review the change with git diff, then commit it. Shearbolt never commits."

func runGH(dir string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, stdin, stdout, stderr
	return cmd.Run()
}

// secretRepo reports whether enable can store ZOSIA_SECRET itself: on a terminal, when every CI
// file is a GitHub workflow and the GitHub CLI is logged in to an account that may set the
// repository's secrets (collaborator access, GitHub's rule for the secrets API). It returns the
// repository as OWNER/NAME and whether the secret exists already. Any failure means no offer: the
// note that says where to add the secret by hand stays.
func (s setupRun) secretRepo(root string, p *plan.Plan) (repo string, has, ok bool) {
	if !s.d.stdinTTY() || slices.ContainsFunc(p.CI, func(c plan.CIFile) bool { return c.System != "github" }) {
		return "", false, false
	}
	// gh hides the pasted secret only when its output is a terminal too.
	if f, isFile := s.stdout.(*os.File); isFile && !isTerminal(f) {
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

// storeSecret reports whether ZOSIA_SECRET is in place afterwards. gh reads the secret from the
// terminal itself, without echo, and encrypts it; Shearbolt never sees it.
func (s setupRun) storeSecret(root, repo string, has bool) bool {
	name := termSafe(repo, false)
	if has {
		fmt.Fprintf(s.stdout, "\nZOSIA_SECRET is already set for %s. Replace it? [y/N] ", name)
		line, _ := readLine(bufio.NewReaderSize(s.d.stdin, maxAnswer+2))
		if l := strings.ToLower(line); l != "y" && l != "yes" {
			fmt.Fprintln(s.stdout, "Kept the existing ZOSIA_SECRET.")
			return true
		}
	}
	fmt.Fprintf(s.stdout, "\nCreate a secret at https://zosiasec.com/app/secrets and paste it below; the GitHub CLI stores it as the repository secret ZOSIA_SECRET for %s. Ctrl-C skips this: `gh secret set ZOSIA_SECRET` sets it later.\n", name)
	if err := s.d.gh(root, s.d.stdin, s.stdout, s.stderr, "secret", "set", "ZOSIA_SECRET", "--repo", repo); err != nil {
		fmt.Fprintf(s.stdout, "ZOSIA_SECRET was not set (gh: %v).\n", err)
		return false
	}
	fmt.Fprintf(s.stdout, "ZOSIA_SECRET is set for %s.\n", name)
	return true
}
