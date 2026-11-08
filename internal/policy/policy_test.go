package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/model"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func finding(mod func(*model.Finding)) model.Finding {
	f := model.Finding{ID: "CVE-2026-1", Target: "web", Package: "lib", Version: "1.0.0", Severity: "medium", FixState: "not-fixed"}
	if mod != nil {
		mod(&f)
	}
	return f
}

func decide(t *testing.T, f model.Finding, p config.Policy, ex ...config.Exception) (model.Decision, []model.ExceptionStatus, []model.Diagnostic) {
	t.Helper()
	out, statuses, warnings := Evaluate([]model.Finding{f}, p, ex, now)
	return out[0].Decision, statuses, warnings
}

func TestKnownExploitedBlocksWithoutFix(t *testing.T) {
	d, _, _ := decide(t, finding(func(f *model.Finding) { f.KnownExploited = true }), config.DefaultPolicy())
	if d.Action != model.Block || !strings.Contains(strings.Join(d.Reasons, ";"), "CISA KEV") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestHighEPSSBlocksWithoutFix(t *testing.T) {
	d, _, _ := decide(t, finding(func(f *model.Finding) { f.EPSS = 0.83 }), config.DefaultPolicy())
	if d.Action != model.Block || !strings.Contains(strings.Join(d.Reasons, ";"), "83%") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestFixAvailableBlocksAtAnySeverity(t *testing.T) {
	d, _, _ := decide(t, finding(func(f *model.Finding) {
		f.Severity, f.FixState, f.FixVersions = "low", "fixed", []string{"1.0.1"}
	}), config.DefaultPolicy())
	if d.Action != model.Block || !strings.Contains(strings.Join(d.Reasons, ";"), "upgrade to 1.0.1") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestFixableBelowThresholdWarns(t *testing.T) {
	p := config.DefaultPolicy()
	p.BlockFixable = "high"
	d, _, _ := decide(t, finding(func(f *model.Finding) { f.FixState, f.FixVersions = "fixed", []string{"1.0.1"} }), p)
	if d.Action != model.Warn {
		t.Fatalf("decision = %+v", d)
	}
}

func TestUnknownSeverityMeetsAnyThreshold(t *testing.T) {
	p := config.DefaultPolicy()
	p.BlockFixable = "critical"
	d, _, _ := decide(t, finding(func(f *model.Finding) { f.Severity, f.FixState = "unknown", "fixed" }), p)
	if d.Action != model.Block {
		t.Fatalf("decision = %+v", d)
	}
}

func TestUnfixedWithoutSignalWarns(t *testing.T) {
	d, _, _ := decide(t, finding(func(f *model.Finding) { f.EPSS = 0.01 }), config.DefaultPolicy())
	if d.Action != model.Warn {
		t.Fatalf("decision = %+v", d)
	}
}

func TestMultipleReasonsAreListed(t *testing.T) {
	d, _, _ := decide(t, finding(func(f *model.Finding) {
		f.KnownExploited, f.FixState, f.FixVersions = true, "fixed", []string{"2.0.0"}
	}), config.DefaultPolicy())
	if len(d.Reasons) != 2 {
		t.Fatalf("reasons = %v", d.Reasons)
	}
}

func exception(mod func(*config.Exception)) config.Exception {
	e := config.Exception{ID: "CVE-2026-1", Package: "lib@1.0.0", Reason: "not reachable", ApprovedBy: "alice", Expires: "2026-12-01"}
	if mod != nil {
		mod(&e)
	}
	return e
}

func TestExactExceptionApplies(t *testing.T) {
	f := finding(func(f *model.Finding) { f.KnownExploited = true })
	d, statuses, _ := decide(t, f, config.DefaultPolicy(), exception(nil))
	if d.Action != model.Excepted || statuses[0].Status != "applied" {
		t.Fatalf("decision = %+v, statuses = %+v", d, statuses)
	}
}

func TestExceptionMatchesAlias(t *testing.T) {
	f := finding(func(f *model.Finding) { f.ID, f.Aliases, f.KnownExploited = "GHSA-aaaa", []string{"CVE-2026-1"}, true })
	if d, _, _ := decide(t, f, config.DefaultPolicy(), exception(nil)); d.Action != model.Excepted {
		t.Fatalf("decision = %+v", d)
	}
}

func TestExceptionForOtherVersionOrTargetDoesNotApply(t *testing.T) {
	f := finding(func(f *model.Finding) { f.KnownExploited = true })
	for _, e := range []config.Exception{
		exception(func(e *config.Exception) { e.Package = "lib@1.0.1" }),
		exception(func(e *config.Exception) { e.Target = "api" }),
	} {
		d, statuses, _ := decide(t, f, config.DefaultPolicy(), e)
		if d.Action != model.Block || statuses[0].Status != "unused" {
			t.Fatalf("exception %+v: decision = %+v, statuses = %+v", e, d, statuses)
		}
	}
}

func TestExpiredExceptionBlocksAndWarns(t *testing.T) {
	f := finding(func(f *model.Finding) { f.KnownExploited = true })
	d, statuses, warnings := decide(t, f, config.DefaultPolicy(), exception(func(e *config.Exception) { e.Expires = "2026-11-07" }))
	if d.Action != model.Block || statuses[0].Status != "expired" || len(warnings) != 1 || warnings[0].Code != "EXCEPTION_EXPIRED" || warnings[0].Target != "" {
		t.Fatalf("decision = %+v, statuses = %+v, warnings = %+v", d, statuses, warnings)
	}
}

func TestExceptionValidThroughExpiryDay(t *testing.T) {
	f := finding(func(f *model.Finding) { f.KnownExploited = true })
	if d, _, _ := decide(t, f, config.DefaultPolicy(), exception(func(e *config.Exception) { e.Expires = "2026-11-08" })); d.Action != model.Excepted {
		t.Fatalf("an exception expiring today must still apply: %+v", d)
	}
}

func TestVerdict(t *testing.T) {
	block := []model.Finding{{Decision: model.Decision{Action: model.Block}}}
	warn := []model.Finding{{Decision: model.Decision{Action: model.Warn}}}
	errs := []model.Diagnostic{{Code: "X"}}
	cases := []struct {
		f    []model.Finding
		e    []model.Diagnostic
		want string
		code int
	}{{nil, nil, "pass", 0}, {warn, nil, "pass", 0}, {block, nil, "block", 1}, {block, errs, "error", 2}, {nil, errs, "error", 2}}
	for _, c := range cases {
		if v, code := Verdict(c.f, c.e); v != c.want || code != c.code {
			t.Errorf("Verdict = %s/%d, want %s/%d", v, code, c.want, c.code)
		}
	}
}

func TestKEVWarnsWhenBlockingDisabled(t *testing.T) {
	p := config.DefaultPolicy()
	p.BlockKnownExploited = false
	d, _, _ := decide(t, finding(func(f *model.Finding) { f.KnownExploited = true }), p)
	if d.Action != model.Warn || !strings.Contains(strings.Join(d.Reasons, ";"), "blockKnownExploited is false") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestFixableNeverWarns(t *testing.T) {
	p := config.DefaultPolicy()
	p.BlockFixable = "never"
	d, _, _ := decide(t, finding(func(f *model.Finding) { f.FixState, f.FixVersions = "fixed", []string{"1.0.1"} }), p)
	if d.Action != model.Warn || !strings.Contains(strings.Join(d.Reasons, ";"), "blockFixable is never") {
		t.Fatalf("decision = %+v", d)
	}
}
