package report

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/klar-web-services/shearbolt/internal/model"
)

// Headline is the one-line verdict shown first in every report.
func Headline(r *model.Run) string {
	blocked, _, warned := Split(r.Findings)
	switch r.Verdict {
	case "error":
		h := fmt.Sprintf("⛔ Scan incomplete: %s, so nothing is approved", plural(len(r.Errors), "error", "errors"))
		if len(blocked) > 0 {
			h += fmt.Sprintf(" (%s also found)", plural(len(blocked), "blocking vulnerability", "blocking vulnerabilities"))
		}
		return h
	case "block":
		return fmt.Sprintf("❌ Blocked: %s must be fixed before deploying", plural(len(blocked), "vulnerability", "vulnerabilities"))
	default:
		if len(warned) > 0 {
			return fmt.Sprintf("✅ Passed: no blocking vulnerabilities (%s)", plural(len(warned), "warning", "warnings"))
		}
		return "✅ Passed: no blocking vulnerabilities"
	}
}

// Split groups findings by decision.
func Split(findings []model.Finding) (blocked, excepted, warned []model.Finding) {
	for _, f := range findings {
		switch f.Decision.Action {
		case model.Block:
			blocked = append(blocked, f)
		case model.Excepted:
			excepted = append(excepted, f)
		default:
			warned = append(warned, f)
		}
	}
	return blocked, excepted, warned
}

// Markdown renders summary.md (also used for the GitHub job summary).
func Markdown(r *model.Run) string {
	blocked, excepted, warned := Split(r.Findings)
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n", Headline(r))
	fmt.Fprintf(&b, "Project `%s` · %s · %s\n\n", code(r.Project), plural(len(r.Targets), "target", "targets"), databaseLine(r))
	if len(blocked) > 0 {
		b.WriteString("#### Must fix before deploying\n\n")
		if ups := Upgrades(blocked); len(ups) > 0 {
			for _, u := range ups {
				if u.Count == 1 {
					fmt.Fprintf(&b, "- Upgrade %s to %s to clear its blocking vulnerability.\n", md(u.Package), md(u.Version))
				} else {
					fmt.Fprintf(&b, "- Upgrade %s to %s to clear all %d of its blocking vulnerabilities.\n", md(u.Package), md(u.Version), u.Count)
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("| Package | Installed → fixed | Vulnerability | Why it blocks | Severity | Where |\n|---|---|---|---|---|---|\n")
		for _, f := range blocked {
			fmt.Fprintf(&b, "| %s | %s → %s | %s | %s | %s | %s |\n", md(f.Package), md(f.Version), md(fixedText(f)), vulnLink(f), md(strings.Join(f.Decision.Reasons, "; ")), md(f.Severity), md(Where(f)))
		}
		b.WriteString("\n")
	}
	if len(excepted) > 0 {
		b.WriteString("#### Accepted by exceptions\n\n| Package | Vulnerability | Why it is accepted |\n|---|---|---|\n")
		for _, f := range excepted {
			fmt.Fprintf(&b, "| %s %s | %s | %s |\n", md(f.Package), md(f.Version), vulnLink(f), md(strings.Join(f.Decision.Reasons, "; ")))
		}
		b.WriteString("\n")
	}
	var skipped []model.TargetResult
	for _, t := range r.Targets {
		if t.Status == "skipped" {
			skipped = append(skipped, t)
		}
	}
	if len(skipped) > 0 {
		b.WriteString("#### Skipped targets\n\n")
		for _, t := range skipped {
			fmt.Fprintf(&b, "- `%s`: %s\n", code(t.Name), md(t.Note))
		}
		b.WriteString("\n")
	}
	if len(r.Errors) > 0 {
		b.WriteString("#### Errors\n\n")
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "- **%s**%s: %s\n", md(e.Code), md(targetSuffix(e.Target)), md(e.Message))
		}
		b.WriteString("\n")
	}
	if n := len(warned) + len(r.Warnings); n > 0 {
		fmt.Fprintf(&b, "<details><summary>%s, not blocking</summary>\n\n", plural(n, "warning", "warnings"))
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "- %s%s\n", md(w.Message), md(targetSuffix(w.Target)))
		}
		if len(warned) > 0 {
			b.WriteString("\n| Package | Vulnerability | Severity | EPSS | Note |\n|---|---|---|---|---|\n")
			for _, f := range warned {
				fmt.Fprintf(&b, "| %s %s | %s | %s | %.1f%% | %s |\n", md(f.Package), md(f.Version), vulnLink(f), md(f.Severity), f.EPSS*100, md(strings.Join(f.Decision.Reasons, "; ")))
			}
		}
		b.WriteString("\n</details>\n\n")
	}
	if r.DBArchive != "" {
		fmt.Fprintf(&b, "Vulnerability data was loaded from `SHEARBOLT_DB_ARCHIVE` (`%s`).\n\n", code(r.DBArchive))
	}
	fmt.Fprintf(&b, "<sub>Shearbolt %s · Grype %s · run %s</sub>\n", md(r.Shearbolt), md(r.Grype), md(r.RunID))
	return b.String()
}

// Text renders a run for a terminal. Colors are used only when color is true.
func Text(r *model.Run, color bool) string {
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	blocked, excepted, warned := Split(r.Findings)
	var b strings.Builder
	head := "1;32"
	switch r.Verdict {
	case "block":
		head = "1;31"
	case "error":
		head = "1;33"
	}
	b.WriteString(paint(head, Headline(r)) + "\n")
	if len(blocked) > 0 {
		b.WriteString("\nMust fix before deploying:\n")
		for _, f := range blocked {
			fmt.Fprintf(&b, "  %s %s → %s  %s (%s)  [%s]\n", clean(f.Package), clean(f.Version), clean(fixedText(f)), clean(f.ID), clean(f.Severity), clean(Where(f)))
			fmt.Fprintf(&b, "      %s\n", clean(strings.Join(f.Decision.Reasons, "; ")))
		}
		if ups := Upgrades(blocked); len(ups) > 0 {
			b.WriteString("\nTo unblock:\n")
			for _, u := range ups {
				clears := "to clear it"
				if u.Count > 1 {
					clears = fmt.Sprintf("to clear all %d", u.Count)
				}
				fmt.Fprintf(&b, "  upgrade %s to %s %s\n", clean(u.Package), clean(u.Version), clears)
			}
		}
	}
	if len(excepted) > 0 {
		fmt.Fprintf(&b, "\nAccepted by exceptions (%d):\n", len(excepted))
		for _, f := range excepted {
			fmt.Fprintf(&b, "  %s %s  %s\n", clean(f.Package), clean(f.Version), clean(f.ID))
		}
	}
	for _, t := range r.Targets {
		if t.Status == "skipped" {
			fmt.Fprintf(&b, "\n%s %s: %s\n", paint("33", "Skipped"), clean(t.Name), clean(t.Note))
		}
	}
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "\n%s %s%s: %s\n", paint("31", "Error"), clean(e.Code), clean(targetSuffix(e.Target)), clean(e.Message))
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "\n%s %s%s\n", paint("33", "Warning"), clean(w.Message), clean(targetSuffix(w.Target)))
	}
	if len(warned) > 0 {
		fmt.Fprintf(&b, "\n%s do not block under your policy. summary.md says why for each one.\n", plural(len(warned), "other finding", "other findings"))
	}
	if r.EvidenceDir != "" {
		fmt.Fprintf(&b, "\nEvidence: %s\n", r.EvidenceDir)
	}
	return b.String()
}

// Where names the target and the first location of a finding.
func Where(f model.Finding) string {
	if len(f.Locations) == 0 {
		return f.Target
	}
	w := f.Target + ": " + f.Locations[0]
	if extra := len(f.Locations) - 1; extra > 0 {
		w += fmt.Sprintf(" (+%d more)", extra)
	}
	return w
}

func fixedText(f model.Finding) string {
	if f.FixState == "fixed" && len(f.FixVersions) > 0 {
		return strings.Join(f.FixVersions, " or ")
	}
	return "no fix"
}

func databaseLine(r *model.Run) string {
	if r.Database == nil {
		return "no vulnerability data loaded"
	}
	return "vulnerability data built " + r.Database.Built.UTC().Format("2006-01-02 15:04 UTC")
}

func vulnLink(f model.Finding) string {
	id := md(f.ID)
	for _, u := range f.URLs {
		if strings.HasPrefix(u, "https://") && !strings.ContainsFunc(u, unsafeURLRune) {
			return "[" + id + "](" + u + ")"
		}
	}
	return id
}

func unsafeURLRune(r rune) bool {
	return r <= ' ' || r >= 0x7f || strings.ContainsRune("()<>\"'|\\`[]", r)
}

func targetSuffix(target string) string {
	if target == "" {
		return ""
	}
	return " (" + target + ")"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// clean replaces control characters so untrusted text cannot drive the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

var mdEscaper = strings.NewReplacer(`\`, `\\`, "|", `\|`, "*", `\*`, "_", `\_`, "`", "\\`", "[", `\[`, "]", `\]`, "<", "&lt;", ">", "&gt;")

// md escapes untrusted text for Markdown tables and lists.
func md(s string) string { return mdEscaper.Replace(clean(s)) }

// code makes text safe inside an inline code span.
func code(s string) string { return strings.ReplaceAll(clean(s), "`", "'") }
