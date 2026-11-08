// Package report writes and renders scan results.
package report

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/model"
)

const githubSummaryLimit = 1_000_000 // GitHub allows 1 MiB per step

// WriteEvidence writes run.json and summary.md into the run's evidence folder.
func WriteEvidence(r *model.Run) error {
	if r.EvidenceDir == "" {
		return errors.New("no evidence folder")
	}
	data, err := RunJSON(r)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.EvidenceDir, "summary.md"), []byte(Markdown(r)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.EvidenceDir, "run.json"), data, 0o644)
}

// RunJSON serializes a run as run.json, with empty arrays instead of nulls.
func RunJSON(r *model.Run) ([]byte, error) {
	if r.Targets == nil {
		r.Targets = []model.TargetResult{}
	}
	if r.Findings == nil {
		r.Findings = []model.Finding{}
	}
	if r.Exceptions == nil {
		r.Exceptions = []model.ExceptionStatus{}
	}
	if r.Warnings == nil {
		r.Warnings = []model.Diagnostic{}
	}
	if r.Errors == nil {
		r.Errors = []model.Diagnostic{}
	}
	return json.MarshalIndent(r, "", "  ")
}

// AppendGitHubSummary appends Markdown to $GITHUB_STEP_SUMMARY, truncated to GitHub's limit.
func AppendGitHubSummary(path, md string) error {
	if len(md) > githubSummaryLimit {
		cut := strings.LastIndex(md[:githubSummaryLimit-300], "\n")
		if cut < 0 {
			cut = githubSummaryLimit - 300
		}
		md = md[:cut] + "\n\n_Report truncated to fit GitHub's job summary limit. The full report is summary.md in the uploaded evidence artifact._\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(md + "\n")
	return err
}
