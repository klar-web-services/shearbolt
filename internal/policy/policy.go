// Package policy decides whether each finding blocks a deployment.
package policy

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/model"
)

var severityRank = map[string]int{"negligible": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}

// Evaluate decides block, warn or excepted for every finding (spec section 6).
// It returns the decided findings, the status of every exception, and warnings
// for expired exceptions that would otherwise have matched.
func Evaluate(findings []model.Finding, p config.Policy, exceptions []config.Exception, now time.Time) ([]model.Finding, []model.ExceptionStatus, []model.Diagnostic) {
	statuses := make([]model.ExceptionStatus, len(exceptions))
	active := make([]bool, len(exceptions))
	for i, e := range exceptions {
		statuses[i] = model.ExceptionStatus{ID: e.ID, Package: e.Package, Target: e.Target, Expires: e.Expires, Status: "unused"}
		end, err := e.ExpiresAt()
		active[i] = err == nil && now.Before(end)
		if !active[i] {
			statuses[i].Status = "expired"
		}
	}
	var warnings []model.Diagnostic
	warned := map[int]bool{}
	out := make([]model.Finding, len(findings))
	for fi, f := range findings {
		if i, ok := matchException(f, exceptions, active); ok {
			if active[i] {
				e := exceptions[i]
				statuses[i].Status = "applied"
				f.Decision = model.Decision{
					Action:    model.Excepted,
					Exception: e.ID,
					Reasons:   []string{fmt.Sprintf("exception approved by %s until %s: %s", e.ApprovedBy, e.Expires, e.Reason)},
				}
				out[fi] = f
				continue
			}
			if !warned[i] {
				warned[i] = true
				warnings = append(warnings, model.Diagnostic{
					Code:    "EXCEPTION_EXPIRED",
					Target:  exceptions[i].Target,
					Message: fmt.Sprintf("the exception for %s on %s expired on %s, so the finding is evaluated normally", exceptions[i].ID, exceptions[i].Package, exceptions[i].Expires),
				})
			}
		}
		var reasons []string
		if p.BlockKnownExploited && f.KnownExploited {
			reasons = append(reasons, "known exploited (CISA KEV)")
		}
		if f.EPSS >= p.BlockEpssAtLeast {
			reasons = append(reasons, fmt.Sprintf("exploit likelihood %s (EPSS, threshold %s)", percent(f.EPSS), percent(p.BlockEpssAtLeast)))
		}
		if f.FixState == "fixed" && meetsFixable(f.Severity, p.BlockFixable) {
			reasons = append(reasons, "fix available: upgrade to "+fixText(f.FixVersions))
		}
		if len(reasons) > 0 {
			f.Decision = model.Decision{Action: model.Block, Reasons: reasons}
		} else {
			f.Decision = model.Decision{Action: model.Warn, Reasons: []string{warnReason(f, p)}}
		}
		out[fi] = f
	}
	return out, statuses, warnings
}

// Verdict aggregates a run: error over block over pass.
func Verdict(findings []model.Finding, errs []model.Diagnostic) (string, int) {
	if len(errs) > 0 {
		return "error", 2
	}
	for _, f := range findings {
		if f.Decision.Action == model.Block {
			return "block", 1
		}
	}
	return "pass", 0
}

// matchException returns the best matching exception, preferring an active one.
func matchException(f model.Finding, exceptions []config.Exception, active []bool) (int, bool) {
	best := -1
	for i, e := range exceptions {
		if e.PackageName() != f.Package || e.PackageVersion() != f.Version {
			continue
		}
		if e.Target != "" && e.Target != f.Target {
			continue
		}
		if e.ID != f.ID && !slices.Contains(f.Aliases, e.ID) {
			continue
		}
		if active[i] {
			return i, true
		}
		if best < 0 {
			best = i
		}
	}
	return best, best >= 0
}

func meetsFixable(severity, threshold string) bool {
	switch threshold {
	case "never":
		return false
	case "any":
		return true
	}
	rank, known := severityRank[severity]
	if !known {
		return true // unknown severity counts as meeting any threshold
	}
	return rank >= severityRank[threshold]
}

func warnReason(f model.Finding, p config.Policy) string {
	switch {
	case f.KnownExploited:
		return "known exploited (CISA KEV), but policy.blockKnownExploited is false"
	case f.FixState == "fixed" && p.BlockFixable == "never":
		return "fix available, but policy.blockFixable is never"
	case f.FixState == "fixed":
		return "fix available, but the severity is below policy.blockFixable"
	}
	return "no fix available, not known exploited, and EPSS below the policy threshold"
}

func fixText(versions []string) string {
	if len(versions) == 0 {
		return "a fixed version"
	}
	return strings.Join(versions, " or ")
}

// percent renders 0.83 as "83%" and 0.105 as "10.5%".
func percent(v float64) string {
	s := strconv.FormatFloat(v*100, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + "%"
}
