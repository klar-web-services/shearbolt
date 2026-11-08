package plan

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestParseAnswers(t *testing.T) {
	got, err := ParseAnswers([]string{"projects=apps/web,crates/cli", "gate-job:apps/api=.github/workflows/ci.yml#test"})
	want := map[string]string{"projects": "apps/web,crates/cli", "gate-job:apps/api": ".github/workflows/ci.yml#test"}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := ParseAnswers([]string{"projects"}); err == nil || !strings.Contains(err.Error(), "id=value") {
		t.Fatalf("missing '=': err = %v", err)
	}
	if _, err := ParseAnswers([]string{"=x"}); err == nil {
		t.Fatal("empty ID: no error")
	}
	if _, err := ParseAnswers([]string{"projects=a", "projects=b"}); err == nil || !strings.Contains(err.Error(), "projects") {
		t.Fatalf("duplicate: err = %v", err)
	}
	if got, err := ParseAnswers(nil); err != nil || len(got) != 0 {
		t.Fatalf("nil: %v, %v", got, err)
	}
}

func resolveQuestions() []Question {
	return []Question{
		{ID: "projects", Kind: "multi", Options: []Option{{Value: "a"}, {Value: "b"}}, Default: "a,b"},
		{ID: "build-project:ci.yml:7", Kind: "choice", Options: []Option{{Value: "a"}, {Value: "b"}, {Value: "none"}}, Default: "a"},
		{ID: "gate-job:b", Kind: "choice", Options: []Option{{Value: "ci.yml#test"}, {Value: "ci.yml#web"}}, Default: "ci.yml#test", Project: "b", UnlessChosen: []string{"build-project:ci.yml:7"}},
	}
}

func TestResolve(t *testing.T) {
	qs := resolveQuestions()
	cases := []struct {
		name        string
		answers     map[string]string
		yes         bool
		wantValues  map[string]string
		wantMissing []string
	}{
		{"b not selected", map[string]string{"projects": "a", "build-project:ci.yml:7": "a"}, false,
			map[string]string{"projects": "a", "build-project:ci.yml:7": "a"}, nil},
		{"build goes to b", map[string]string{"build-project:ci.yml:7": "b"}, false,
			map[string]string{"projects": "a,b", "build-project:ci.yml:7": "b"}, []string{"projects"}},
		{"nothing answered", map[string]string{}, false,
			map[string]string{"projects": "a,b", "build-project:ci.yml:7": "a", "gate-job:b": "ci.yml#test"},
			[]string{"projects", "build-project:ci.yml:7", "gate-job:b"}},
		{"defaults with yes", nil, true,
			map[string]string{"projects": "a,b", "build-project:ci.yml:7": "a", "gate-job:b": "ci.yml#test"}, nil},
		{"multi normalized", map[string]string{"projects": "b, a,b", "build-project:ci.yml:7": "none", "gate-job:b": "ci.yml#web"}, false,
			map[string]string{"projects": "a,b", "build-project:ci.yml:7": "none", "gate-job:b": "ci.yml#web"}, nil},
		{"inapplicable answer ignored", map[string]string{"projects": "a", "build-project:ci.yml:7": "a", "gate-job:b": "ci.yml#web"}, false,
			map[string]string{"projects": "a", "build-project:ci.yml:7": "a"}, nil},
	}
	for _, c := range cases {
		values, missing, err := Resolve(qs, c.answers, c.yes)
		if err != nil || !maps.Equal(values, c.wantValues) || !slices.Equal(missing, c.wantMissing) {
			t.Errorf("%s: values = %v, missing = %v, err = %v", c.name, values, missing, err)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	qs := resolveQuestions()
	cases := []struct {
		answers map[string]string
		want    string
	}{
		{map[string]string{"projects": "c"}, "a, b"},
		{map[string]string{"projects": ""}, "projects"},
		{map[string]string{"projects": "a,,b"}, "projects"},
		{map[string]string{"nope": "1"}, "nope"},
		{map[string]string{"build-project:ci.yml:7": "c"}, "a, b, none"},
		{map[string]string{"projects": "a", "build-project:ci.yml:7": "a", "gate-job:b": "x"}, "ci.yml#test, ci.yml#web"},
	}
	for _, c := range cases {
		if _, _, err := Resolve(qs, c.answers, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want it to mention %q", c.answers, err, c.want)
		}
	}
}

func TestResolveWithoutProjectsQuestionSelectsEveryProject(t *testing.T) {
	qs := []Question{{ID: "gate-job:.", Kind: "choice", Options: []Option{{Value: "j"}}, Default: "j", Project: "."}}
	values, missing, err := Resolve(qs, nil, false)
	if err != nil || values["gate-job:."] != "j" || !slices.Equal(missing, []string{"gate-job:."}) {
		t.Fatalf("values = %v, missing = %v, err = %v", values, missing, err)
	}
}

// An unlessChosen question that does not apply has no value, so it cannot make another question
// inapplicable, even though its default names that question's project.
func TestResolveIgnoresInapplicableUnlessChosen(t *testing.T) {
	qs := []Question{
		{ID: "projects", Kind: "multi", Options: []Option{{Value: "a"}, {Value: "b"}, {Value: "c"}}, Default: "a,b,c"},
		{ID: "build-project:x", Kind: "choice", Options: []Option{{Value: "a"}, {Value: "b"}, {Value: "none"}}, Default: "b", Project: "c"},
		{ID: "gate-job:b", Kind: "choice", Options: []Option{{Value: "j"}}, Default: "j", Project: "b", UnlessChosen: []string{"build-project:x"}},
	}
	values, missing, err := Resolve(qs, map[string]string{"projects": "a,b"}, false)
	if err != nil || !maps.Equal(values, map[string]string{"projects": "a,b", "gate-job:b": "j"}) || !slices.Equal(missing, []string{"gate-job:b"}) {
		t.Fatalf("values = %v, missing = %v, err = %v", values, missing, err)
	}
}
