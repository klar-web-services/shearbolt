// Package scan runs Grype over a config's targets and turns the results into a decision.
package scan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/buildinfo"
	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/grype"
	"github.com/klar-web-services/shearbolt/internal/model"
	"github.com/klar-web-services/shearbolt/internal/policy"
	"github.com/klar-web-services/shearbolt/internal/report"
)

// Scanner runs one Grype invocation. *grype.Runner implements it.
type Scanner interface {
	Scan(ctx context.Context, req grype.Request) error
}

// Options configures one scan.
type Options struct {
	Config     *config.Config
	BaseDir    string // the default evidence folder is BaseDir/.shearbolt/runs/<run-id>
	OutputDir  string // explicit evidence folder; must be new or empty
	CIMode     bool
	Scanner    Scanner
	SetupError error // Grype could not be prepared; recorded, nothing is scanned
	HasImage   DockerInspector
	Env        config.Env // compose variables; nil means os.LookupEnv
	DBArchive  string
	Now        func() time.Time
}

type scanned struct {
	index         int
	needsFallback bool
}

type state struct {
	opts          Options
	run           *model.Run
	rawDir        string
	start         time.Time
	findings      []model.Finding
	scanned       map[string]*scanned
	staleReported bool
}

// Run scans every target and always returns a result, even when scanning failed.
// The evidence folder is written before Run returns.
func Run(ctx context.Context, o Options) *model.Run {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HasImage == nil {
		o.HasImage = DockerHasImage
	}
	if o.Env == nil {
		o.Env = os.LookupEnv
	}
	cfg := o.Config
	start := o.Now().UTC()
	r := &model.Run{
		SchemaVersion: "1", RunID: newRunID(start), Project: cfg.Project, StartedAt: start, CIMode: o.CIMode,
		Shearbolt: buildinfo.Version, Grype: grype.Version, Policy: cfg.Policy, DBArchive: o.DBArchive,
	}
	r.EvidenceDir = o.OutputDir
	if r.EvidenceDir == "" {
		r.EvidenceDir = filepath.Join(o.BaseDir, ".shearbolt", "runs", r.RunID)
	}
	s := &state{opts: o, run: r, rawDir: filepath.Join(r.EvidenceDir, "raw"), start: start, scanned: map[string]*scanned{}}
	evidenceOK := true
	if err := prepareEvidenceDir(r.EvidenceDir); err != nil {
		evidenceOK = false
		s.fail("EVIDENCE", "", fmt.Sprintf("cannot use evidence folder %s: %v", r.EvidenceDir, err))
		r.EvidenceDir = ""
	}
	if o.SetupError != nil {
		s.fail("SETUP", "", o.SetupError.Error())
	}
	if len(r.Errors) == 0 {
		for _, t := range cfg.Targets {
			if ctx.Err() != nil {
				break
			}
			s.scanTarget(ctx, t)
		}
		s.resolveFallbacks(ctx)
		switch {
		case ctx.Err() != nil:
			s.fail("CANCELLED", "", "scan cancelled")
		case len(r.Targets) > 0 && allSkipped(r.Targets):
			s.fail("ALL_SKIPPED", "", "every target was skipped, so nothing was checked")
		}
	}
	findings, statuses, warnings := policy.Evaluate(s.findings, cfg.Policy, cfg.Exceptions, start)
	sortFindings(findings)
	r.Findings, r.Exceptions = findings, statuses
	r.Warnings = append(r.Warnings, warnings...)
	r.FinishedAt = o.Now().UTC()
	r.Verdict, r.ExitCode = policy.Verdict(r.Findings, r.Errors)
	if evidenceOK {
		if err := report.WriteEvidence(r); err != nil {
			s.fail("EVIDENCE", "", "cannot write evidence: "+err.Error())
			r.EvidenceDir = ""
			r.Verdict, r.ExitCode = policy.Verdict(r.Findings, r.Errors)
		}
	}
	return r
}

func (s *state) scanTarget(ctx context.Context, t config.Target) {
	i := len(s.run.Targets)
	kind, ref := t.Kind(), t.Ref()
	s.run.Targets = append(s.run.Targets, model.TargetResult{Name: t.Name, Kind: kind, Ref: ref, Exclude: t.Exclude, Status: "scanned"})
	if kind == "compose" {
		s.scanCompose(ctx, i, t)
		return
	}
	req := s.request(t.Name)
	var lockfiles []string
	switch kind {
	case "image":
		if s.opts.HasImage(ctx, ref) {
			req.Source = "docker:" + ref
		} else {
			req.Source = "registry:" + ref
		}
	case "source":
		if info, err := os.Stat(ref); err != nil || !info.IsDir() {
			s.targetError(i, "MISSING_SOURCE", fmt.Sprintf("source directory %s does not exist", ref))
			return
		}
		var err error
		lockfiles, err = FindLockfiles(ref, t.Exclude)
		if err != nil {
			s.targetError(i, "SOURCE_UNREADABLE", err.Error())
			return
		}
		if len(lockfiles) == 0 {
			s.targetError(i, "NO_LOCKFILE", fmt.Sprintf("no lockfile found in %s, so its dependencies cannot be checked (supported: %s)", ref, strings.Join(Lockfiles, ", ")))
			return
		}
		req.Source = "dir:" + ref
		req.Exclude = GrypeExcludes(t.Exclude)
	case "dir":
		if info, err := os.Stat(ref); err != nil || !info.IsDir() {
			s.missing(i, fmt.Sprintf("directory %s does not exist", ref))
			return
		}
		req.Source = "dir:" + ref
	case "file":
		info, err := os.Stat(ref)
		if err != nil || !info.Mode().IsRegular() {
			s.missing(i, fmt.Sprintf("file %s does not exist", ref))
			return
		}
		sum, err := fileDigest(ref)
		if err != nil {
			s.targetError(i, "FILE_UNREADABLE", err.Error())
			return
		}
		s.run.Targets[i].Identity.SHA256 = sum
		req.Source = "file:" + ref
	}
	if err := s.opts.Scanner.Scan(ctx, req); err != nil {
		// Only an image that is not in the local daemon and cannot be pulled may be skipped,
		// and only outside CI mode. A Grype failure on a local image is a real error.
		if kind == "image" && strings.HasPrefix(req.Source, "registry:") && !s.opts.CIMode && ctx.Err() == nil {
			s.skip(i, fmt.Sprintf("image %s is not in your local Docker and could not be pulled (%s). Build it with `docker build -t %s .` to include it.", ref, firstLine(err.Error()), ref))
			return
		}
		s.targetError(i, "SCAN_FAILED", err.Error())
		return
	}
	s.run.Targets[i].Raw = append(s.run.Targets[i].Raw, s.raw(req)...)
	res, inv, err := s.readOutputs(req)
	if err != nil {
		s.targetError(i, "BAD_OUTPUT", err.Error())
		return
	}
	if kind == "source" && !s.mergeNested(ctx, i, t.Name, ref, t.Exclude, lockfiles, res, &inv) {
		return
	}
	s.run.Targets[i].Packages = inv.Packages
	if kind == "image" {
		s.run.Targets[i].Identity = res.Identity
	}
	sc := &scanned{index: i}
	s.scanned[t.Name] = sc
	s.add(t.Name, res.Findings)
	if kind == "file" && t.LockfileFallback != "" && inv.Cargo == 0 {
		sc.needsFallback = true
		return
	}
	if kind == "source" {
		// Packages from other files (such as workflow `uses:` lines) must not hide a lockfile
		// that Grype could not read.
		var unread []string
		for _, lf := range lockfiles {
			if inv.Locations["/"+lf] != 0 {
				continue
			}
			base := path.Base(lf)
			yarnOrBun := base == "yarn.lock" || base == "bun.lock"
			if yarnOrBun && devOnlyNodeProject(filepath.Join(ref, filepath.FromSlash(path.Dir(lf)))) {
				s.warn("DEV_ONLY_LOCKFILE", t.Name, fmt.Sprintf("%s lists only devDependencies, which Grype does not check for yarn and bun projects; nothing from it ships, so it was not counted as unread", lf))
				continue
			}
			unread = append(unread, lf)
		}
		if len(unread) > 0 {
			s.targetError(i, "LOCKFILE_UNREAD", fmt.Sprintf("Grype read no packages from %s, so its dependencies were not checked. Common causes: a broken or unsupported file, a requirements.txt without pinned versions, or a go.mod without requirements. If nothing from it ships, add its folder to this target's exclude list (for the project root, remove the source target).", strings.Join(unread, ", ")))
			return
		}
		return // every lockfile yielded packages or was a dev-only yarn or bun project, so the NO_PACKAGES rule below cannot apply
	}
	if inv.Packages == 0 {
		s.targetError(i, "NO_PACKAGES", fmt.Sprintf("Grype found no packages in %s, so nothing could be checked", ref))
	}
}

// resolveFallbacks covers non-auditable Rust binaries with their lockfile (spec section 8).
func (s *state) resolveFallbacks(ctx context.Context) {
	const advice = "binary is not auditable; rebuild with cargo-auditable"
	for _, t := range s.opts.Config.Targets {
		sc := s.scanned[t.Name]
		if sc == nil || !sc.needsFallback {
			continue
		}
		lock := t.LockfileFallback
		if cover := s.coveringSource(lock); cover != "" {
			s.warn("NOT_AUDITABLE", t.Name, fmt.Sprintf("%s. Its crates are covered by %s in target %q.", advice, lock, cover))
			continue
		}
		if info, err := os.Stat(lock); err != nil || !info.Mode().IsRegular() {
			s.targetError(sc.index, "MISSING_LOCKFILE", fmt.Sprintf("lockfileFallback %s does not exist", lock))
			continue
		}
		req := s.request(t.Name + "+lockfile")
		req.Source = "file:" + lock
		if err := s.opts.Scanner.Scan(ctx, req); err != nil {
			s.targetError(sc.index, "SCAN_FAILED", err.Error())
			continue
		}
		s.run.Targets[sc.index].Raw = append(s.run.Targets[sc.index].Raw, s.raw(req)...)
		res, inv, err := s.readOutputs(req)
		if err != nil {
			s.targetError(sc.index, "BAD_OUTPUT", err.Error())
			continue
		}
		if inv.Packages == 0 {
			s.targetError(sc.index, "NO_PACKAGES", fmt.Sprintf("Grype found no packages in %s", lock))
			continue
		}
		s.run.Targets[sc.index].Packages += inv.Packages
		s.add(t.Name, res.Findings)
		s.warn("NOT_AUDITABLE", t.Name, fmt.Sprintf("%s. Its crates were checked from %s instead.", advice, lock))
	}
}

func (s *state) coveringSource(lock string) string {
	for _, t := range s.opts.Config.Targets {
		if t.Source == "" {
			continue
		}
		if sc := s.scanned[t.Name]; sc != nil && s.run.Targets[sc.index].Status == "scanned" && Covers(t, lock) {
			return t.Name
		}
	}
	return ""
}

// raw lists a request's output files as recorded in run.json.
func (s *state) raw(req grype.Request) []string {
	return []string{s.rel(req.RawJSON), s.rel(req.SBOM)}
}

func (s *state) readOutputs(req grype.Request) (*grype.Result, grype.Inventory, error) {
	data, err := os.ReadFile(req.RawJSON)
	if err != nil {
		return nil, grype.Inventory{}, fmt.Errorf("Grype wrote no report: %w", err)
	}
	res, err := grype.Parse(data)
	if err != nil {
		return nil, grype.Inventory{}, err
	}
	sbom, err := os.ReadFile(req.SBOM)
	if err != nil {
		return nil, grype.Inventory{}, fmt.Errorf("Grype wrote no SBOM: %w", err)
	}
	inv, err := grype.ParseSBOM(sbom)
	if err != nil {
		return nil, grype.Inventory{}, err
	}
	s.recordDatabase(res)
	return res, inv, nil
}

func (s *state) recordDatabase(res *grype.Result) {
	if s.run.Database == nil {
		s.run.Database = &model.Database{Built: res.DBBuilt, SchemaVersion: res.DBSchema, Source: res.DBSource}
	}
	limit := s.opts.Config.Policy.MaxDatabaseAgeHours
	if age := s.start.Sub(res.DBBuilt); age > time.Duration(limit)*time.Hour && !s.staleReported {
		s.staleReported = true
		s.fail("STALE_DATABASE", "", fmt.Sprintf("the vulnerability data was built %s ago, which is older than policy.maxDatabaseAgeHours (%d). Check that this machine can reach grype.anchore.io, then scan again.", fmt.Sprintf("%.0f hours", age.Hours()), limit))
	}
}

func (s *state) request(name string) grype.Request {
	return grype.Request{RawJSON: filepath.Join(s.rawDir, name+".grype.json"), SBOM: filepath.Join(s.rawDir, name+".cdx.json")}
}

func (s *state) add(target string, list []model.Finding) {
	for _, f := range list {
		f.Target = target
		s.findings = append(s.findings, f)
	}
}

// missing handles an artifact (file/dir) that does not exist: skipped locally, an error in CI.
func (s *state) missing(i int, what string) {
	if s.opts.CIMode {
		s.targetError(i, "MISSING_TARGET", what)
		return
	}
	s.skip(i, what+". Build it first, then scan again.")
}

func (s *state) skip(i int, note string) {
	s.run.Targets[i].Status = "skipped"
	s.run.Targets[i].Note = note
}

func (s *state) targetError(i int, code, msg string) {
	s.run.Targets[i].Status = "error"
	s.run.Targets[i].Note = msg
	s.run.Errors = append(s.run.Errors, model.Diagnostic{Code: code, Target: s.run.Targets[i].Name, Message: msg})
}

func (s *state) fail(code, target, msg string) {
	s.run.Errors = append(s.run.Errors, model.Diagnostic{Code: code, Target: target, Message: msg})
}

func (s *state) warn(code, target, msg string) {
	s.run.Warnings = append(s.run.Warnings, model.Diagnostic{Code: code, Target: target, Message: msg})
}

func (s *state) rel(p string) string {
	if r, err := filepath.Rel(s.run.EvidenceDir, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

func prepareEvidenceDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case err == nil && len(entries) > 0:
		return errors.New("the folder is not empty; use a new folder for every run")
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return os.MkdirAll(filepath.Join(dir, "raw"), 0o755)
}

func allSkipped(targets []model.TargetResult) bool {
	for _, t := range targets {
		if t.Status != "skipped" {
			return false
		}
	}
	return true
}

func sortFindings(list []model.Finding) {
	sort.SliceStable(list, func(a, b int) bool {
		x, y := list[a], list[b]
		if x.Target != y.Target {
			return x.Target < y.Target
		}
		if x.Package != y.Package {
			return x.Package < y.Package
		}
		if x.Version != y.Version {
			return x.Version < y.Version
		}
		return x.ID < y.ID
	})
}

func newRunID(t time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return t.Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
