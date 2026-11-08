package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/model"
)

func blockedRun() *model.Run {
	return &model.Run{
		RunID: "r1", Project: "my-app", Verdict: "block", ExitCode: 1, Shearbolt: "dev", Grype: "0.121.2",
		Database: &model.Database{Built: time.Date(2026, 11, 7, 6, 18, 0, 0, time.UTC)},
		Targets:  []model.TargetResult{{Name: "web", Kind: "image", Status: "scanned"}, {Name: "cli", Kind: "file", Status: "skipped", Note: "file bin/cli does not exist"}},
		Findings: []model.Finding{
			{ID: "CVE-2026-1", Target: "web", Package: "evil|pkg", Version: "1.0.0", Type: "npm", Severity: "high", FixState: "fixed", FixVersions: []string{"1.0.1"}, URLs: []string{"https://example.test/CVE-2026-1"}, Locations: []string{"/app/package-lock.json"},
				Decision: model.Decision{Action: model.Block, Reasons: []string{"fix available: upgrade to 1.0.1"}}},
			{ID: "CVE-2026-2", Target: "web", Package: "quiet", Version: "2.0.0", Severity: "low", FixState: "not-fixed",
				Decision: model.Decision{Action: model.Warn, Reasons: []string{"no fix available and no sign of exploitation"}}},
		},
	}
}

func TestMarkdownLeadsWithVerdictAndEscapes(t *testing.T) {
	md := Markdown(blockedRun())
	if !strings.HasPrefix(md, "### ❌ Blocked: 1 vulnerability must be fixed before deploying") {
		t.Fatalf("first line = %q", strings.SplitN(md, "\n", 2)[0])
	}
	for _, want := range []string{`evil\|pkg`, "1.0.0 → 1.0.1", "[CVE-2026-1](https://example.test/CVE-2026-1)", "fix available", "Skipped targets", "1 warning"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestUnsafeURLIsNotLinked(t *testing.T) {
	r := blockedRun()
	r.Findings[0].URLs = []string{"https://example.test/a\nb|c"}
	md := Markdown(r)
	if strings.Contains(md, "](https://example.test/a") {
		t.Fatalf("unsafe URL rendered as a link:\n%s", md)
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "CVE-2026-1") && !strings.HasPrefix(line, "|") {
			t.Fatalf("table row was split: %q", line)
		}
	}
}

func TestHeadlineVariants(t *testing.T) {
	pass := &model.Run{Verdict: "pass"}
	if h := Headline(pass); h != "✅ Passed: no blocking vulnerabilities" {
		t.Errorf("pass headline = %q", h)
	}
	errRun := &model.Run{Verdict: "error", Errors: []model.Diagnostic{{Code: "X"}}}
	if h := Headline(errRun); !strings.HasPrefix(h, "⛔ Scan incomplete: 1 error") {
		t.Errorf("error headline = %q", h)
	}
}

func TestTextStripsControlCharacters(t *testing.T) {
	r := blockedRun()
	r.Findings[0].Package = "evil\x1b[31mpkg\u009b"
	out := Text(r, false)
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\u009b") {
		t.Fatalf("terminal output contains a control character: %q", out)
	}
	if !strings.Contains(out, "Must fix before deploying") {
		t.Fatalf("output = %s", out)
	}
}

func TestRunJSONUsesEmptyArrays(t *testing.T) {
	data, err := RunJSON(&model.Run{Verdict: "pass"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"findings", "targets", "warnings", "errors", "exceptions"} {
		if _, ok := m[key].([]any); !ok {
			t.Errorf("%s = %v, want []", key, m[key])
		}
	}
}

func TestWriteEvidence(t *testing.T) {
	r := blockedRun()
	r.EvidenceDir = t.TempDir()
	if err := WriteEvidence(r); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run.json", "summary.md"} {
		if _, err := os.Stat(filepath.Join(r.EvidenceDir, f)); err != nil {
			t.Errorf("%s missing: %v", f, err)
		}
	}
}

func TestAppendGitHubSummaryTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary")
	big := strings.Repeat("| row |\n", 300000)
	if err := AppendGitHubSummary(path, big); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 1<<20 || !strings.Contains(string(data), "truncated") {
		t.Fatalf("summary is %d bytes; truncated note present: %v", len(data), strings.Contains(string(data), "truncated"))
	}
}

// lodashRun is blocked by the four lodash findings of testdata/projects/npm-vulnerable.
func lodashRun() *model.Run {
	r := blockedRun()
	r.Findings = append(lodashFindings(), r.Findings[1])
	for i := range r.Findings[:4] {
		r.Findings[i].Target = "repo"
		r.Findings[i].Locations = []string{"/package-lock.json"}
	}
	return r
}

func TestMarkdownUpgradeLine(t *testing.T) {
	md := Markdown(lodashRun())
	line := "- Upgrade lodash to 4.18.0 to clear all 4 of its blocking vulnerabilities.\n\n"
	head := strings.Index(md, "#### Must fix before deploying\n\n")
	at := strings.Index(md, line)
	table := strings.Index(md, "| Package | Installed → fixed |")
	if head < 0 || at < 0 || table < 0 || !(head < at && at < table) {
		t.Fatalf("upgrade line missing or misplaced (section %d, line %d, table %d):\n%s", head, at, table, md)
	}
	if !strings.Contains(md, "#### Must fix before deploying\n\n"+line+"| Package |") {
		t.Fatalf("upgrade bullets are not between the heading and the table:\n%s", md)
	}
}

func TestMarkdownUpgradeLineSingleAndEscaped(t *testing.T) {
	md := Markdown(blockedRun())
	if !strings.Contains(md, "- Upgrade evil\\|pkg to 1.0.1 to clear its blocking vulnerability.\n") {
		t.Fatalf("single upgrade line missing or unescaped:\n%s", md)
	}
}

func TestMarkdownNoUpgradeLineWithoutBlockingFindings(t *testing.T) {
	r := blockedRun()
	r.Verdict, r.ExitCode = "pass", 0
	r.Findings[0].Decision.Action = model.Warn
	md := Markdown(r)
	if strings.Contains(md, "Must fix") || strings.Contains(md, "Upgrade") {
		t.Fatalf("upgrade line without a blocking section:\n%s", md)
	}
	if out := Text(r, false); strings.Contains(out, "To unblock") || strings.Contains(out, "upgrade ") {
		t.Fatalf("terminal upgrade line without a blocking section:\n%s", out)
	}
}

func TestMarkdownNoUpgradeLineWhenAPackageHasNoFix(t *testing.T) {
	r := lodashRun()
	kev := blockF("npm", "lodash", "4.17.20", "not-fixed")
	r.Findings = append(r.Findings, kev)
	if md := Markdown(r); strings.Contains(md, "Upgrade lodash") {
		t.Fatalf("upgrade line despite a blocking finding without a fix:\n%s", md)
	}
	if out := Text(r, false); strings.Contains(out, "To unblock") {
		t.Fatalf("terminal upgrade line despite a blocking finding without a fix:\n%s", out)
	}
}

func TestTextUpgradeLine(t *testing.T) {
	out := Text(lodashRun(), false)
	want := "\nTo unblock:\n  upgrade lodash to 4.18.0 to clear all 4\n"
	at := strings.Index(out, want)
	if at < 0 {
		t.Fatalf("terminal output lacks %q:\n%s", want, out)
	}
	if last := strings.LastIndex(out, "      fix available"); last < 0 || last > at {
		t.Fatalf("upgrade block is not after the per-finding lines:\n%s", out)
	}
	single := Text(blockedRun(), false)
	if !strings.Contains(single, "\nTo unblock:\n  upgrade evil|pkg to 1.0.1 to clear it\n") {
		t.Fatalf("single upgrade line missing:\n%s", single)
	}
	if skipped := strings.Index(single, "Skipped"); skipped < strings.Index(single, "To unblock:") {
		t.Fatalf("upgrade block is not inside the blocking section:\n%s", single)
	}
}

func TestTextUpgradeLineStripsControlCharacters(t *testing.T) {
	r := blockedRun()
	r.Findings[0].Package = "evil\x1b[31mpkg\u009b"
	r.Findings[0].FixVersions = []string{"1.0.1"}
	out := Text(r, false)
	if !strings.Contains(out, "To unblock:\n  upgrade evil [31mpkg  to 1.0.1 to clear it\n") {
		t.Fatalf("upgrade line missing or not cleaned: %q", out)
	}
	if strings.ContainsAny(out, "\x1b\u009b") {
		t.Fatalf("terminal output contains a control character: %q", out)
	}
}

// The renderers compute upgrades from the blocking findings only: an excepted finding with a
// higher fix and a warned finding without a fix change neither the version nor the count.
func TestUpgradeLineIgnoresExceptedAndWarned(t *testing.T) {
	r := lodashRun()
	excepted := fixed("npm", "lodash", "4.17.20", "9.0.0")
	excepted.Decision = model.Decision{Action: model.Excepted, Reasons: []string{"accepted"}}
	warned := blockF("npm", "lodash", "4.17.20", "not-fixed")
	warned.Decision = model.Decision{Action: model.Warn, Reasons: []string{"no fix"}}
	r.Findings = append(r.Findings, excepted, warned)
	if md := Markdown(r); !strings.Contains(md, "- Upgrade lodash to 4.18.0 to clear all 4 of its blocking vulnerabilities.\n") {
		t.Fatalf("markdown upgrade line changed by non-blocking findings:\n%s", md)
	}
	if out := Text(r, false); !strings.Contains(out, "  upgrade lodash to 4.18.0 to clear all 4\n") {
		t.Fatalf("terminal upgrade line changed by non-blocking findings:\n%s", out)
	}
}

func TestMarkdownPluralUpgradeLineEscapes(t *testing.T) {
	r := blockedRun()
	second := r.Findings[0]
	second.ID, second.FixVersions = "CVE-2026-3", []string{"1.0.2"}
	r.Findings = append(r.Findings, second)
	if md := Markdown(r); !strings.Contains(md, "- Upgrade evil\\|pkg to 1.0.2 to clear all 2 of its blocking vulnerabilities.\n") {
		t.Fatalf("plural upgrade line missing or unescaped:\n%s", md)
	}
}
