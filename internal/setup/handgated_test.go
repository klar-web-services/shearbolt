package setup

import (
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const (
	testGoMod    = "module github.com/acme/tool\n\ngo 1.27\n\nrequire go.yaml.in/yaml/v3 v3.0.6\n"
	handConfig   = ".github/release-gate.yaml"
	handGateLine = "gate: curl -fsSL https://example.com/shearbolt.tar.gz | tar -xz; ./shearbolt scan --ci --config " + handConfig + "\n"
)

func goTree(extra map[string]string) map[string]string {
	files := map[string]string{
		"go.mod":   testGoMod,
		fakeCIFile: "build: go build -o bin/tool ./cmd/tool\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

func notesWith(p *plan.Plan, code string) []plan.Note {
	var out []plan.Note
	for _, n := range p.Notes {
		if n.Code == code {
			out = append(out, n)
		}
	}
	return out
}

func TestSetupGoProject(t *testing.T) {
	root := writeTree(t, goTree(map[string]string{"testdata/npm/package-lock.json": "{}"}))
	ci := &fakeCI{files: []string{fakeCIFile}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatalf("error = %+v", p.Error)
	}
	if len(p.Projects) != 1 || p.Projects[0].Ecosystem != "go" || p.Projects[0].Name != "tool" {
		t.Fatalf("projects = %+v", p.Projects)
	}
	cfg := change(p, "shearbolt.yaml")
	if cfg == nil {
		t.Fatalf("no shearbolt.yaml in %v", changePaths(p))
	}
	for _, want := range []string{"file: bin/tool", "source: .", "testdata/"} {
		if !strings.Contains(cfg.Content, want) {
			t.Errorf("shearbolt.yaml lacks %q:\n%s", want, cfg.Content)
		}
	}
	if strings.Contains(cfg.Content, "lockfileFallback") {
		t.Errorf("a Go binary target has a lockfileFallback:\n%s", cfg.Content)
	}
	if len(ci.gates) != 1 || ci.gates[0].Job != fakeCIFile+"#build" || len(ci.gates[0].Files) != 1 {
		t.Fatalf("gates = %+v", ci.gates)
	}
}

// Two builds of one binary for different platforms: the second target is named after its folder.
func TestSetupGoCrossBuilds(t *testing.T) {
	root := writeTree(t, goTree(map[string]string{
		fakeCIFile: "build: GOARCH=amd64 go build -o build/x86_64-unknown-linux-gnu/tool ./cmd/tool; GOARCH=arm64 go build -o build/aarch64-unknown-linux-gnu/tool ./cmd/tool\n",
	}))
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error != nil {
		t.Fatalf("error = %+v", p.Error)
	}
	cfg := change(p, "shearbolt.yaml")
	if cfg == nil {
		t.Fatalf("no shearbolt.yaml in %v", changePaths(p))
	}
	for _, want := range []string{
		"- name: tool\n    file: build/x86_64-unknown-linux-gnu/tool\n",
		"- name: tool-aarch64-unknown-linux-gnu\n    file: build/aarch64-unknown-linux-gnu/tool\n",
	} {
		if !strings.Contains(cfg.Content, want) {
			t.Errorf("shearbolt.yaml lacks %q:\n%s", want, cfg.Content)
		}
	}
}

func TestSetupHandGated(t *testing.T) {
	root := writeTree(t, goTree(map[string]string{
		fakeCIFile: "build: go build -o bin/tool ./cmd/tool\n" + handGateLine + "publish: gh release create v1 bin/tool\n",
		handConfig: "version: 1\nproject: tool-release\ntargets:\n  - name: linux-amd64\n    file: ${CANDIDATE_DIR}/tool\n  - name: repo\n    source: ..\n    exclude: [testdata/]\n",
	}))
	ci := &fakeCI{files: []string{fakeCIFile}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatalf("error = %+v", p.Error)
	}
	if len(p.Changes) != 0 || len(ci.gates) != 0 {
		t.Fatalf("changes = %v, gates = %+v", changePaths(p), ci.gates)
	}
	if len(p.Questions) != 0 {
		t.Errorf("questions = %+v, want none: there is nothing left to decide", p.Questions)
	}
	notes := notesWith(p, "ALREADY_GATED")
	if len(notes) != 1 || notes[0].File != fakeCIFile || notes[0].Line != 2 || !strings.Contains(notes[0].Message, handConfig) {
		t.Fatalf("notes = %+v", p.Notes)
	}
	if len(p.Notes) != 1 {
		t.Errorf("notes = %+v, want only ALREADY_GATED", p.Notes)
	}
}

func TestSetupHandGateElsewhereDoesNotCover(t *testing.T) {
	tests := map[string]string{
		"another directory": "version: 1\nproject: x\ntargets:\n  - name: docs\n    source: ../docs\n",
		"project excluded":  "version: 1\nproject: x\ntargets:\n  - name: repo\n    source: ../..\n    exclude: [app/]\n",
		"no source target":  "version: 1\nproject: x\ntargets:\n  - name: bin\n    file: ../bin/tool\n",
		"not YAML":          "targets: [\n",
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"app/go.mod":       testGoMod,
				fakeCIFile:         "build: cd app && go build -o bin/tool ./cmd/tool\n" + strings.Replace(handGateLine, handConfig, "app/ci/gate.yaml", 1),
				"app/ci/gate.yaml": config,
			})
			ci := &fakeCI{files: []string{fakeCIFile}}
			p := computeWith(t, root, ci, nil)
			if p.Error != nil {
				t.Fatalf("error = %+v", p.Error)
			}
			if len(ci.gates) != 1 || change(p, "app/shearbolt.yaml") == nil || len(notesWith(p, "ALREADY_GATED")) != 0 {
				t.Fatalf("gates = %+v, changes = %v, notes = %+v", ci.gates, changePaths(p), p.Notes)
			}
		})
	}
}

func TestSetupHandGatedOnlyInOneWorkflow(t *testing.T) {
	const deploy = ".github/workflows/deploy.yml"
	root := writeTree(t, goTree(map[string]string{
		fakeCIFile: "build: go build -o bin/tool ./cmd/tool\n" + handGateLine,
		deploy:     "ship: go build -o bin/tool ./cmd/tool; gh release create v1 bin/tool\n",
		handConfig: "version: 1\nproject: tool-release\ntargets:\n  - name: repo\n    source: ..\n",
	}))
	ci := &fakeCI{files: []string{fakeCIFile, deploy}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil {
		t.Fatalf("error = %+v", p.Error)
	}
	if len(ci.gates) != 1 || ci.gates[0].Job != deploy+"#ship" {
		t.Fatalf("gates = %+v, want one in %s", ci.gates, deploy)
	}
	if change(p, "shearbolt.yaml") == nil || change(p, fakeCIFile) != nil {
		t.Errorf("changes = %v", changePaths(p))
	}
	if len(notesWith(p, "ALREADY_GATED")) != 1 {
		t.Errorf("notes = %+v", p.Notes)
	}
	if !slices.ContainsFunc(p.Questions, func(q plan.Question) bool { return q.ID == "zosia" }) {
		t.Errorf("questions = %+v, want the zosia question for the gate setup adds", p.Questions)
	}
}

func TestSetupHandGatedWithoutBuild(t *testing.T) {
	root := writeTree(t, goTree(map[string]string{
		fakeCIFile: "test: go test ./...\n" + handGateLine,
		handConfig: "version: 1\nproject: tool-release\ntargets:\n  - name: repo\n    source: ..\n",
	}))
	ci := &fakeCI{files: []string{fakeCIFile}}
	p := computeWith(t, root, ci, nil)
	if p.Error != nil || len(p.Changes) != 0 || len(p.Questions) != 0 || len(notesWith(p, "ALREADY_GATED")) != 1 {
		t.Fatalf("error = %+v, changes = %v, questions = %+v, notes = %+v", p.Error, changePaths(p), p.Questions, p.Notes)
	}
}

func TestNoProjectNamesWhatScanCanCheck(t *testing.T) {
	root := writeTree(t, map[string]string{
		fakeCIFile:        "test: pytest\n",
		"poetry.lock":     "",
		"svc/uv.lock":     "",
		"docs/README.md":  "hi\n",
		"cli/go.mod":      "module example.com/cli\n",
		"cli/cmd/main.go": "package main\n",
	})
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error == nil || p.Error.Code != "NO_PROJECT" {
		t.Fatalf("error = %+v, want NO_PROJECT", p.Error)
	}
	msg := p.Error.Message
	if strings.Contains(msg, "nothing for Shearbolt to protect") {
		t.Errorf("the message says there is nothing to protect:\n%s", msg)
	}
	for _, want := range []string{"reads these lockfiles: poetry.lock, svc/uv.lock.", "shearbolt scan", "by hand", "cli/go.mod requires no modules"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q:\n%s", want, msg)
		}
	}
}

func TestNoProjectNamesExistingGates(t *testing.T) {
	root := writeTree(t, map[string]string{
		fakeCIFile:    "test: pytest\n" + handGateLine,
		"poetry.lock": "",
		handConfig:    "version: 1\nproject: x\ntargets:\n  - name: repo\n    source: ..\n",
	})
	p := computeWith(t, root, &fakeCI{files: []string{fakeCIFile}}, nil)
	if p.Error == nil || p.Error.Code != "NO_PROJECT" {
		t.Fatalf("error = %+v, want NO_PROJECT", p.Error)
	}
	for _, want := range []string{"CI already runs Shearbolt", fakeCIFile + ":2", "leaves"} {
		if !strings.Contains(p.Error.Message, want) {
			t.Errorf("the message lacks %q:\n%s", want, p.Error.Message)
		}
	}
}
