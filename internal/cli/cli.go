package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/klar-web-services/shearbolt/internal/buildinfo"
	"github.com/klar-web-services/shearbolt/internal/grype"
	"github.com/klar-web-services/shearbolt/internal/zosia"
)

const usage = `Shearbolt stops a deployment when what you are about to ship contains
a vulnerability that is exploited, likely to be exploited, or already fixed upstream.

Usage:
  shearbolt setup [flags]              Add the Shearbolt gate to this repository's CI
  shearbolt scan [flags]               Scan the targets in shearbolt.yaml, or ad-hoc targets
  shearbolt skill install [--project]  Install the coding-agent skill (/shearbolt setup, /shearbolt fix)
  shearbolt zosia enable|disable       Send this repository's CI reports to Zosia (zosiasec.com), or stop
  shearbolt update                     Update shearbolt to the latest release
  shearbolt version                    Print version information

Run "shearbolt setup -h", "shearbolt scan -h" or "shearbolt skill install -h" for their flags,
and "shearbolt zosia -h" for what it changes.
`

type deps struct {
	getenv      func(string) string
	lookupEnv   func(string) (string, bool)
	environ     func() []string
	now         func() time.Time
	ensureGrype func(ctx context.Context, cacheDir string) (string, error)
	buildImage  func(ctx context.Context, ref, dir string, out io.Writer) error
	color       func(io.Writer) bool
	stdin       io.Reader
	stdinTTY    func() bool
	version     string
	sendZosia   func(ctx context.Context, r zosia.Request) (zosia.Result, error)
	update      func(version string, stdout, stderr io.Writer) error
}

func defaultDeps() deps {
	return deps{
		getenv:    os.Getenv,
		lookupEnv: os.LookupEnv,
		environ:   os.Environ,
		now:       time.Now,
		ensureGrype: func(ctx context.Context, cacheDir string) (string, error) {
			return grype.NewInstaller(cacheDir).Ensure(ctx)
		},
		buildImage: dockerBuild,
		color: func(w io.Writer) bool {
			return os.Getenv("NO_COLOR") == "" && isTerminal(w)
		},
		stdin:    os.Stdin,
		stdinTTY: func() bool { return isTerminal(os.Stdin) },
		version:  buildinfo.Version,
		sendZosia: func(ctx context.Context, r zosia.Request) (zosia.Result, error) {
			return zosia.Send(ctx, &http.Client{}, zosiaPolicy, r)
		},
		update: updateSelf,
	}
}

func Run(args []string, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr, defaultDeps())
}

func run(args []string, stdout, stderr io.Writer, d deps) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "setup":
		return runSetup(args[1:], stdout, stderr, d)
	case "scan":
		return runScan(args[1:], stdout, stderr, d)
	case "skill":
		return runSkill(args[1:], stdout, stderr, d)
	case "zosia":
		return runZosia(args[1:], stdout, stderr, d)
	case "update":
		return runUpdate(args[1:], stdout, stderr, d)
	case "version", "--version":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "shearbolt version: unexpected argument %q\n\n%s", args[1], usage)
			return 2
		}
		fmt.Fprintf(stdout, "shearbolt %s\ngrype %s\n", buildinfo.Version, grype.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "shearbolt: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// It checks for a character device other than the null device, which is
// one too (CI runners often give stdin </dev/null).
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return false
	}
	return true
}
