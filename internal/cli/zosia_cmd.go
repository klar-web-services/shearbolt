package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/klar-web-services/shearbolt/internal/setup"
)

const zosiaUsage = `Usage: shearbolt zosia enable|disable

Run it in the repository root, after "shearbolt setup" has added the gate.

  enable   Makes the Shearbolt gates in this repository's CI send their reports to Zosia
           (https://zosiasec.com), then says where to put the secret. On GitHub Actions it adds
           the zosia-secret input to the shearbolt steps; GitLab CI and Bitbucket Pipelines
           need no change, only the CI variable ZOSIA_SECRET.
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
	s.printChanges(p)
	s.printNotes(p)
	if len(p.Changes) > 0 {
		fmt.Fprintf(stdout, "\n%s\n", zosiaReview)
	}
	return 0
}

const zosiaReview = "Review the change with git diff, then commit it. Shearbolt never commits."
