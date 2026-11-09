package grype

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var errGrypeTimeout = errors.New("grype timeout")

// Request describes one Grype scan.
type Request struct {
	Source   string   // Grype source, e.g. "dir:/path", "docker:ref", "registry:ref", "file:/path"
	Exclude  []string // --exclude globs (directory sources only)
	Platform string   // --platform for image sources ("" = Grype's default: the scanning machine's)
	RawJSON  string   // where Grype writes its JSON report
	SBOM     string   // where Grype writes its CycloneDX SBOM
}

// Runner runs a verified Grype binary isolated from project and user configuration.
type Runner struct {
	Binary     string
	CacheDir   string        // Shearbolt cache; the vulnerability database lives in CacheDir/grype-db
	Timeout    time.Duration // per invocation; default 15 minutes
	Environ    []string      // parent environment, usually os.Environ()
	NoDBUpdate bool          // true after importing a database archive
}

// Scan runs Grype once, writing the JSON report and the SBOM.
func (r *Runner) Scan(ctx context.Context, req Request) error {
	var err error
	if req.RawJSON, err = filepath.Abs(req.RawJSON); err != nil {
		return err
	}
	if req.SBOM, err = filepath.Abs(req.SBOM); err != nil {
		return err
	}
	for _, prefix := range []string{"dir:", "file:"} {
		if rest, ok := strings.CutPrefix(req.Source, prefix); ok {
			abs, err := filepath.Abs(rest)
			if err != nil {
				return err
			}
			req.Source = prefix + abs
			break
		}
	}
	args := []string{req.Source, "-o", "json=" + req.RawJSON, "-o", "cyclonedx-json=" + req.SBOM}
	for _, e := range req.Exclude {
		args = append(args, "--exclude", e)
	}
	if req.Platform != "" {
		args = append(args, "--platform", req.Platform)
	}
	return r.run(ctx, args)
}

// ImportDB loads a Grype database archive into the cache.
func (r *Runner) ImportDB(ctx context.Context, archive string) error {
	archive, err := filepath.Abs(archive)
	if err != nil {
		return err
	}
	return r.run(ctx, []string{"db", "import", archive})
}

func (r *Runner) run(ctx context.Context, args []string) error {
	return r.runOutput(ctx, args, io.Discard)
}

// runOutput runs Grype with the isolated configuration, writing its stdout to stdout.
func (r *Runner) runOutput(ctx context.Context, args []string, stdout io.Writer) error {
	binary, err := filepath.Abs(r.Binary)
	if err != nil {
		return err
	}
	cacheDir, err := filepath.Abs(r.CacheDir)
	if err != nil {
		return err
	}
	environ := r.Environ
	if environ == nil {
		environ = os.Environ()
	}
	work, err := os.MkdirTemp("", "shearbolt-grype-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	cfgPath := filepath.Join(work, "grype.yaml")
	if err := os.WriteFile(cfgPath, r.configYAML(cacheDir), 0o600); err != nil {
		return err
	}
	args = append(append([]string{}, args...), "-c", cfgPath)
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errGrypeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = work
	cmd.Env = isolatedEnv(environ, work)
	var stderr tailBuffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	if err == nil {
		return nil
	}
	switch {
	case context.Cause(ctx) == errGrypeTimeout:
		return fmt.Errorf("Grype timed out after %s", timeout)
	case ctx.Err() != nil:
		return errors.New("scan cancelled")
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("Grype failed: %s", msg)
}

// configYAML is the only Grype configuration used: no app update checks, a Shearbolt-owned
// database location, and no Grype-side age check (Shearbolt enforces maxDatabaseAgeHours itself).
func (r *Runner) configYAML(cacheDir string) []byte {
	type db struct {
		CacheDir    string `yaml:"cache-dir"`
		AutoUpdate  bool   `yaml:"auto-update"`
		ValidateAge bool   `yaml:"validate-age"`
	}
	cfg := struct {
		CheckForAppUpdate bool `yaml:"check-for-app-update"`
		DB                db   `yaml:"db"`
	}{false, db{CacheDir: filepath.Join(cacheDir, "grype-db"), AutoUpdate: !r.NoDBUpdate, ValidateAge: false}}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		panic(err) // a fixed struct always marshals
	}
	return out
}

// isolatedEnv drops GRYPE_*/SYFT_* settings and points XDG_CONFIG_HOME at the private work dir.
func isolatedEnv(parent []string, work string) []string {
	env := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "GRYPE_") || strings.HasPrefix(kv, "SYFT_") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "XDG_CONFIG_HOME="+filepath.Join(work, "xdg"))
}

// tailBuffer keeps the last 8 KiB written to it.
type tailBuffer struct{ buf []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 8192 {
		t.buf = t.buf[len(t.buf)-8192:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
