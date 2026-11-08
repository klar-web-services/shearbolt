package plan

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPlanJSON(t *testing.T) {
	p := Plan{
		SchemaVersion:    1,
		ShearboltVersion: "1.2.3",
		Projects:         []Project{{Ecosystem: "npm", Dir: ".", Name: "web", Lockfiles: []string{"package-lock.json"}, Binaries: []string{"x"}, Excludes: []string{"y/"}}},
		CI:               []CIFile{{System: "github", File: ".github/workflows/ci.yml"}},
		Questions: []Question{{ID: "gate-job:.", Prompt: "Which job should check the lockfiles of .?", Kind: "choice",
			Options: []Option{{Value: ".github/workflows/ci.yml#test", Label: "ci.yml: test"}}, Default: ".github/workflows/ci.yml#test", Project: "."}},
		Changes: []FileChange{{Path: "shearbolt.yaml", Action: "create", Diff: "--- /dev/null\n", Content: "version: 1\n", Base: []byte("x"), Mode: 0o644}},
		Notes:   []Note{{Code: "NO_BUILD_FOUND", Message: "No build step was found for .", File: ".github/workflows/ci.yml"}},
	}
	got, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":1,"shearboltVersion":"1.2.3",` +
		`"projects":[{"ecosystem":"npm","dir":".","name":"web","lockfiles":["package-lock.json"]}],` +
		`"ci":[{"system":"github","file":".github/workflows/ci.yml"}],` +
		`"questions":[{"id":"gate-job:.","prompt":"Which job should check the lockfiles of .?","kind":"choice","options":[{"value":".github/workflows/ci.yml#test","label":"ci.yml: test"}],"default":".github/workflows/ci.yml#test","project":"."}],` +
		`"complete":false,` +
		`"changes":[{"path":"shearbolt.yaml","action":"create","diff":"--- /dev/null\n","content":"version: 1\n"}],` +
		`"notes":[{"code":"NO_BUILD_FOUND","message":"No build step was found for .","file":".github/workflows/ci.yml"}]}`
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlanJSONEmptyListsAndError(t *testing.T) {
	p := Plan{SchemaVersion: 1, ShearboltVersion: "dev", Error: &Note{Code: "NO_CI", Message: "a && b"}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":1,"shearboltVersion":"dev","projects":[],"ci":[],"questions":[],"complete":false,"changes":[],"notes":[],"error":{"code":"NO_CI","message":"a && b"}}` + "\n"
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestImageGateable(t *testing.T) {
	if !(Image{Platforms: 1, Push: true}).Gateable() || (Image{Platforms: 2}).Gateable() || (Image{OutputPush: true}).Gateable() || !(Image{}).Gateable() {
		t.Fatal("Gateable is wrong")
	}
}
