package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/grype"
	"github.com/klar-web-services/shearbolt/internal/report"
	"github.com/klar-web-services/shearbolt/internal/scan"
	"github.com/klar-web-services/shearbolt/internal/scanlock"
)

const scanUsage = `Usage: shearbolt scan [flags]

Scans the targets in ./shearbolt.yaml (or --config). Ad-hoc flags scan only what
they name, with the default policy, and ignore shearbolt.yaml.

Exit codes: 0 passed, 1 blocked by policy, 2 error or incomplete scan.

Flags:
`

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runScan(args []string, stdout, stderr io.Writer, d deps) int {
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, scanUsage); flags.PrintDefaults() }
	configPath := flags.String("config", "", "path to shearbolt.yaml (default ./shearbolt.yaml)")
	var only, images, dirs, files, sources stringList
	flags.Var(&only, "target", "scan only this target from the config (repeatable)")
	flags.Var(&images, "image", "ad-hoc: scan a container image (repeatable)")
	flags.Var(&dirs, "dir", "ad-hoc: scan everything in a directory (repeatable)")
	flags.Var(&files, "file", "ad-hoc: scan one file or binary (repeatable)")
	flags.Var(&sources, "source", "ad-hoc: scan the lockfiles in a source tree (repeatable)")
	outputDir := flags.String("output-dir", "", "evidence folder (default .shearbolt/runs/<run-id>)")
	jsonOut := flags.Bool("json", false, "print only run.json to stdout")
	ciFlag := flags.Bool("ci", false, "CI mode: missing targets are errors (also on when the CI variable is set)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "shearbolt scan: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	// configError reports a problem found before scanning. In GitHub Actions it also goes to the
	// job summary, so a broken config is explained there like any other failed scan.
	configError := func(msg string) int {
		fmt.Fprintf(stderr, "shearbolt scan: %s\n", msg)
		if path := d.getenv("GITHUB_STEP_SUMMARY"); path != "" {
			md := "### ⛔ Scan incomplete: invalid configuration, so nothing is approved\n\n```text\n" + strings.ReplaceAll(msg, "```", "'''") + "\n```\n"
			if err := report.AppendGitHubSummary(path, md); err != nil {
				fmt.Fprintf(stderr, "shearbolt: could not write the GitHub job summary: %v\n", err)
			}
		}
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return configError(err.Error())
	}
	now := d.now()
	env := config.Env(d.lookupEnv)
	adHoc := len(images)+len(dirs)+len(files)+len(sources) > 0
	var cfg *config.Config
	switch {
	case adHoc && *configPath != "":
		return configError("use either --config or ad-hoc target flags, not both")
	case adHoc:
		cfg, err = config.AdHoc(filepath.Base(cwd), cwd, images, dirs, files, sources, env, now)
	default:
		path := *configPath
		explicit := path != ""
		if !explicit {
			path = "shearbolt.yaml"
		}
		if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
			what := "there is no shearbolt.yaml here"
			if explicit {
				what = "the config file " + path + " does not exist"
			}
			return configError(what + ". Run `shearbolt setup` to create one and add the gate to your CI (or see https://github.com/klar-web-services/shearbolt#readme), or scan ad hoc with --image, --dir, --file or --source.")
		}
		cfg, err = config.Load(path, env, now)
	}
	if err != nil {
		return configError(err.Error())
	}
	if len(only) > 0 {
		if cfg.Targets, err = selectTargets(cfg.Targets, only); err != nil {
			return configError(err.Error())
		}
	}
	cacheDir, err := defaultCacheDir(d)
	if err != nil {
		return configError(err.Error())
	}
	out := ""
	if *outputDir != "" {
		if out, err = filepath.Abs(*outputDir); err != nil {
			fmt.Fprintf(stderr, "shearbolt scan: %v\n", err)
			return 2
		}
		if within(out, cacheDir) || within(cacheDir, out) {
			return configError(fmt.Sprintf("--output-dir %s overlaps the Shearbolt cache %s; use a folder outside it", out, cacheDir))
		}
	}
	// One scan at a time per cache. Taken before Ctrl-C is caught, so a waiting scan can still be
	// interrupted; the lock lasts until this function returns.
	release, err := scanlock.Acquire(cacheDir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "shearbolt scan: %v\n", err)
		return 2
	}
	defer release()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := scan.Options{
		Config: cfg, BaseDir: cwd, OutputDir: out, CIMode: *ciFlag || isCI(d.getenv("CI")), Env: env,
		DBArchive: d.getenv("SHEARBOLT_DB_ARCHIVE"), Now: d.now,
	}
	if opts.DBArchive != "" {
		// Grype runs in a private folder, so a relative archive path must be made absolute here.
		if abs, err := filepath.Abs(opts.DBArchive); err == nil {
			opts.DBArchive = abs
		}
	}
	if bin, err := d.ensureGrype(ctx, cacheDir); err != nil {
		opts.SetupError = err
	} else {
		runner := &grype.Runner{Binary: bin, CacheDir: cacheDir, Timeout: 15 * time.Minute, Environ: d.environ()}
		if opts.DBArchive != "" {
			// The archive's database replaces Grype's own updates. It is imported only when the
			// cache does not already hold exactly this archive's database (grype.ImportDBIfNeeded).
			runner.NoDBUpdate = true
			if _, err := runner.ImportDBIfNeeded(ctx, opts.DBArchive); err != nil {
				opts.SetupError = fmt.Errorf("import vulnerability database %s: %w", opts.DBArchive, err)
			}
		}
		opts.Scanner = runner
	}
	result := scan.Run(ctx, opts)
	if path := d.getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if err := report.AppendGitHubSummary(path, report.Markdown(result)); err != nil {
			fmt.Fprintf(stderr, "shearbolt: could not write the GitHub job summary: %v\n", err)
		}
	}
	if *jsonOut {
		data, err := report.RunJSON(result)
		if err != nil {
			fmt.Fprintf(stderr, "shearbolt scan: %v\n", err)
			return 2
		}
		fmt.Fprintln(stdout, string(data))
	} else {
		fmt.Fprint(stdout, report.Text(result, d.color(stdout)))
	}
	return result.ExitCode
}

func selectTargets(all []config.Target, names []string) ([]config.Target, error) {
	var out []config.Target
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		found := false
		for _, t := range all {
			if t.Name == n {
				out = append(out, t)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("target %q is not defined in the config", n)
		}
	}
	return out, nil
}

func isCI(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v != "" && v != "false" && v != "0"
}

// within reports whether path is dir or inside it (both absolute and clean).
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// defaultCacheDir is SHEARBOLT_CACHE_DIR, else $XDG_CACHE_HOME/shearbolt, else ~/.cache/shearbolt
// (on every OS, so CI cache paths in generated pipelines are the same everywhere).
func defaultCacheDir(d deps) (string, error) {
	if dir := d.getenv("SHEARBOLT_CACHE_DIR"); dir != "" {
		return filepath.Abs(dir) // relative values (common on GitLab) must not depend on Grype's private cwd
	}
	if xdg := d.getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Abs(filepath.Join(xdg, "shearbolt"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no cache directory: %v (set SHEARBOLT_CACHE_DIR)", err)
	}
	return filepath.Join(home, ".cache", "shearbolt"), nil
}
