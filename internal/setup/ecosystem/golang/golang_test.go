package golang

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/scan"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

const goMod = "module github.com/acme/tool\n\ngo 1.27\n\nrequire go.yaml.in/yaml/v3 v3.0.6\n"

func repo(t *testing.T, files map[string]string) plan.Repo {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return plan.Repo{Root: root}
}

func find(t *testing.T, files map[string]string) (plan.Repo, []plan.Project, []plan.Note) {
	t.Helper()
	r := repo(t, files)
	projects, notes, err := Plugin{}.Find(r)
	if err != nil {
		t.Fatal(err)
	}
	return r, projects, notes
}

func TestLockfileIsScanned(t *testing.T) {
	if !slices.Contains(scan.Lockfiles, lockfile) {
		t.Fatalf("%s is not in scan.Lockfiles", lockfile)
	}
}

func TestFind(t *testing.T) {
	r, projects, notes := find(t, map[string]string{
		"go.mod":                    goMod,
		"tools/gen/go.mod":          "module example.com/gen/v2\n\nrequire (\n\tgolang.org/x/mod v0.20.0\n)\n",
		"anon/go.mod":               "require golang.org/x/mod v0.20.0\n",
		"testdata/fixture/go.mod":   goMod,
		"vendor/example.com/go.mod": goMod,
	})
	want := []plan.Project{
		{Ecosystem: "go", Dir: ".", Name: "tool", Lockfiles: []string{"go.mod"}},
		{Ecosystem: "go", Dir: "anon", Name: "anon", Lockfiles: []string{"go.mod"}},
		{Ecosystem: "go", Dir: "tools/gen", Name: "gen", Lockfiles: []string{"go.mod"}},
	}
	if !reflect.DeepEqual(projects, want) || len(notes) != 0 {
		t.Fatalf("root %s: projects = %+v, notes = %+v\nwant %+v", r.Root, projects, notes, want)
	}
}

func TestFindModuleWithoutRequirements(t *testing.T) {
	_, projects, notes := find(t, map[string]string{"cli/go.mod": "module example.com/cli\n\ngo 1.27\n"})
	if len(projects) != 0 {
		t.Fatalf("a go.mod without requirements gave projects %+v", projects)
	}
	if len(notes) != 1 || notes[0].Code != "GO_NO_REQUIREMENTS" || notes[0].File != "cli/go.mod" || !strings.Contains(notes[0].Message, "cli/go.mod requires no modules") {
		t.Fatalf("notes = %+v", notes)
	}
}

func recognize(t *testing.T, r plan.Repo, dir, script string) *plan.Build {
	t.Helper()
	cmds, _ := shell.Parse(script, shell.State{Dir: dir})
	var found *plan.Build
	for _, cmd := range cmds {
		if b := (Plugin{}).Recognize(r, cmd, plan.Context{Dir: cmd.Before.Dir}); b != nil {
			if found != nil {
				t.Fatalf("%q holds two builds", script)
			}
			found = b
		}
	}
	return found
}

func TestRecognize(t *testing.T) {
	r := repo(t, map[string]string{"go.mod": goMod, "svc/api/go.mod": goMod})
	tests := []struct {
		dir, script string
		kind        plan.BuildKind
		buildDir    string
		paths       []string
		why         string // part of the GO_PATH_UNKNOWN message; "" = no note
	}{
		{".", "go build -o bin/tool ./cmd/tool", plan.BuildBinary, ".", []string{"bin/tool"}, ""},
		{".", `CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.v=1" -o=bin/tool ./cmd/tool`, plan.BuildBinary, ".", []string{"bin/tool"}, ""},
		{".", "go build -tags netgo -o ./tool", plan.BuildBinary, ".", []string{"tool"}, ""},
		{"svc/api", "go build -o ../../bin/api .", plan.BuildBinary, "svc/api", nil, "outside its module"},
		{"svc/api", "go build -o out/api .", plan.BuildBinary, "svc/api", []string{"out/api"}, ""},
		{".", "cd svc/api && go build -o out/api .", plan.BuildBinary, "svc/api", []string{"out/api"}, ""},
		{".", "go build -C svc/api -o out/api .", plan.BuildBinary, "svc/api", []string{"out/api"}, ""},
		{".", `go build -o "build/$triple/tool" ./cmd/tool`, plan.BuildBinary, ".", nil, "-o uses a variable"},
		{".", "go build -o bin/ ./cmd/...", plan.BuildBinary, ".", nil, "-o names a directory"},
		{".", "go build -o /usr/local/bin/tool ./cmd/tool", plan.BuildBinary, ".", nil, "absolute path"},
		{".", "go build ./cmd/tool", plan.BuildBinary, ".", nil, "names no output"},
		{".", "go build", plan.BuildBinary, ".", nil, "names no output"},
		{".", "go build ./...", plan.BuildOther, ".", nil, ""},
		{".", "go build -v ./cmd/... ./internal/...", plan.BuildOther, ".", nil, ""},
		{".", "for a in amd64 arm64; do GOARCH=$a go build -o bin/tool-$a ./cmd/tool; done", plan.BuildOther, ".", nil, "conditional or loop"},
		{".", "if true; then go build -o bin/tool ./cmd/tool; fi", plan.BuildOther, ".", nil, "conditional or loop"},
	}
	for _, tt := range tests {
		b := recognize(t, r, tt.dir, tt.script)
		if b == nil {
			t.Errorf("%q: not recognized", tt.script)
			continue
		}
		if b.Kind != tt.kind || b.Dir != tt.buildDir || !slices.Equal(b.Paths, tt.paths) {
			t.Errorf("%q: kind %s, dir %q, paths %v; want %s, %q, %v", tt.script, b.Kind, b.Dir, b.Paths, tt.kind, tt.buildDir, tt.paths)
		}
		switch {
		case tt.why == "" && len(b.Notes) != 0:
			t.Errorf("%q: unexpected notes %+v", tt.script, b.Notes)
		case tt.why != "" && (len(b.Notes) != 1 || b.Notes[0].Code != "GO_PATH_UNKNOWN" || !strings.Contains(b.Notes[0].Message, tt.why)):
			t.Errorf("%q: notes %+v, want GO_PATH_UNKNOWN with %q", tt.script, b.Notes, tt.why)
		}
	}
}

func TestRecognizeUnknownDirectory(t *testing.T) {
	r := repo(t, map[string]string{"go.mod": goMod})
	b := recognize(t, r, ".", `cd "$WORK" && go build -o bin/tool ./cmd/tool`)
	if b == nil || b.Dir != "" || b.Paths != nil || len(b.Notes) != 1 || !strings.Contains(b.Notes[0].Message, "its directory is not known") {
		t.Fatalf("build = %+v", b)
	}
	if b := recognize(t, repo(t, nil), ".", "go build -o bin/tool ./cmd/tool"); b == nil || b.Paths != nil || !strings.Contains(b.Notes[0].Message, "no go.mod") {
		t.Fatalf("without a go.mod: build = %+v", b)
	}
}

func TestRecognizeIgnoresOtherCommands(t *testing.T) {
	r := repo(t, map[string]string{"go.mod": goMod})
	for _, script := range []string{
		"go test ./...",
		"go vet ./...",
		"go test -c -o live.test ./internal/integration/",
		"go install ./cmd/tool",
		"go run ./cmd/tool build",
		"cargo build --release",
		"echo go build",
	} {
		if b := recognize(t, r, ".", script); b != nil {
			t.Errorf("%q recognized as %+v", script, b)
		}
	}
}
