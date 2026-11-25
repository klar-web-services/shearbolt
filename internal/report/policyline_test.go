package report

import (
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/model"
)

func TestPolicyLineIsOnlyThereForARunThatAskedZosia(t *testing.T) {
	seven := 7
	for name, c := range map[string]struct {
		source model.PolicySource
		want   string
	}{
		"the file":       {model.PolicySource{From: "file"}, ""},
		"ad-hoc flags":   {model.PolicySource{From: "default"}, ""},
		"from Zosia":     {model.PolicySource{From: "zosia", Zosia: "used", Version: &seven, Scope: "project"}, "Policy: from Zosia (project policy, version 7)"},
		"Zosia has none": {model.PolicySource{From: "file", Zosia: "none", Version: &seven}, "Policy: shearbolt.yaml (Zosia has no policy for this project)"},
		"unreachable":    {model.PolicySource{From: "file", Zosia: "unreachable"}, "Policy: shearbolt.yaml (Zosia's policy could not be fetched, see the warnings)"},
		"no CI named":    {model.PolicySource{From: "file", Zosia: "no-ci"}, "Policy: shearbolt.yaml (Zosia's policy could not be fetched, see the warnings)"},
	} {
		r := &model.Run{Project: "p", Verdict: "pass", PolicySource: c.source}
		if got := PolicyLine(r); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
		for kind, text := range map[string]string{"Markdown": Markdown(r), "Text": Text(r, false)} {
			if c.want == "" && strings.Contains(text, "Policy:") || c.want != "" && !strings.Contains(text, c.want+"\n") {
				t.Errorf("%s, %s:\n%s", name, kind, text)
			}
		}
	}
}
