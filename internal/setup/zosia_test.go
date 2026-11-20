package setup

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gated is a golden fixture after setup ran on it: before/ with after/ on top.
func gated(t *testing.T, fixture string) string {
	t.Helper()
	root := t.TempDir()
	copyTree(t, filepath.Join(goldenDir, fixture, "before"), root)
	copyTree(t, filepath.Join(goldenDir, fixture, "after"), root)
	return root
}

func TestZosiaOnGitLabAndBitbucketChangesNoFile(t *testing.T) {
	for fixture, want := range map[string]string{"gitlab": "masked CI/CD variable ZOSIA_SECRET", "bitbucket": "secured repository variable ZOSIA_SECRET"} {
		t.Run(fixture, func(t *testing.T) {
			root := gated(t, fixture)
			p := Zosia(root, true)
			if p.Error != nil || len(p.Changes) != 0 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_SECRET"}) || !strings.Contains(p.Notes[0].Message, want) {
				t.Errorf("enable: error %+v, %d changes, notes %+v", p.Error, len(p.Changes), p.Notes)
			}
			p = Zosia(root, false)
			if p.Error != nil || len(p.Changes) != 0 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_VARIABLE", "ZOSIA_SECRET_STAYS"}) {
				t.Errorf("disable: error %+v, %d changes, notes %+v", p.Error, len(p.Changes), p.Notes)
			}
		})
	}
}

func TestZosiaWithoutAGate(t *testing.T) {
	root := t.TempDir()
	copyTree(t, filepath.Join(goldenDir, "gitlab", "before"), root)
	if p := Zosia(root, true); p.Error == nil || p.Error.Code != "NO_GATE" || !strings.Contains(p.Error.Message, "shearbolt setup") {
		t.Errorf("without a gate: %+v", p.Error)
	}
	if p := Zosia(t.TempDir(), true); p.Error == nil || p.Error.Code != "NO_CI" {
		t.Errorf("without CI: %+v", p.Error)
	}
}
