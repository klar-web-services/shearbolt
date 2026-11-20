package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/klar-web-services/shearbolt"
)

const updateUsage = `Usage: shearbolt update

Update shearbolt to the latest release, in the folder it is installed in. It runs the installer
that came with this version, so it needs curl or wget and checks the download's SHA-256 checksum.

Exit codes: 0 updated or already up to date, 2 error.
`

func runUpdate(args []string, stdout, stderr io.Writer, d deps) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprint(stdout, updateUsage)
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "shearbolt update: unexpected argument %q\n\n%s", args[0], updateUsage)
		return 2
	}
	if err := d.update(d.version, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "shearbolt update: %v\n", err)
		return 2
	}
	return 0
}

func updateSelf(version string, stdout, stderr io.Writer) error {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fmt.Errorf("cannot find shearbolt's own binary: %v", err)
	}
	return updateBinary(exe, version, stdout, stderr)
}

func updateBinary(exe, version string, stdout, stderr io.Writer) error {
	if filepath.Base(exe) != "shearbolt" {
		return fmt.Errorf("%s is not named shearbolt, so the installer cannot replace it", exe)
	}
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = bytes.NewReader(shearbolt.InstallScript)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = append(os.Environ(), "SHEARBOLT_INSTALL_DIR="+filepath.Dir(exe), "SHEARBOLT_UPDATE_FROM="+version)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return errors.New("nothing was updated")
	}
	return err
}
