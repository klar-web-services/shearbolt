package setup

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

const composeImage = "services:\n  web:\n    image: nginx:1.27\n"
const composeBuild = "services:\n  web:\n    build: .\n"

func noteCodes(ns []plan.Note, code string) []plan.Note {
	var out []plan.Note
	for _, n := range ns {
		if n.Code == code {
			out = append(out, n)
		}
	}
	return out
}

func TestFindComposeWalk(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docker-compose.yml":               composeImage,
		"test/compose.yml":                 composeImage,
		"docker-compose.override.yml":      composeBuild,
		".devcontainer/docker-compose.yml": composeImage,
	})
	files, notes, err := findCompose(plan.Repo{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"docker-compose.yml", "test/compose.yml"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
	ni := noteCodes(notes, "COMPOSE_NO_IMAGES")
	if len(notes) != 1 || len(ni) != 1 || !strings.Contains(ni[0].Message, "docker-compose.override.yml pulls no image") {
		t.Errorf("notes = %+v, want one COMPOSE_NO_IMAGES for the override", notes)
	}
}

func TestFindComposeRejected(t *testing.T) {
	root := writeTree(t, map[string]string{"compose.yml": "include:\n  - other.yml\nservices:\n  a:\n    image: x\n"})
	files, notes, err := findCompose(plan.Repo{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	nr := noteCodes(notes, "COMPOSE_NOT_READ")
	if len(files) != 0 || len(nr) != 1 || !strings.HasPrefix(nr[0].Message, "compose.yml was not gated: ") {
		t.Errorf("files = %v, notes = %+v", files, notes)
	}
}

func TestFindComposeVariable(t *testing.T) {
	root := writeTree(t, map[string]string{"compose.yml": "services:\n  web:\n    image: app:${TAG}\n"})
	files, notes, err := findCompose(plan.Repo{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	nv := noteCodes(notes, "COMPOSE_VARIABLE")
	if !slices.Equal(files, []string{"compose.yml"}) || len(nv) != 1 || nv[0].File != "compose.yml" || nv[0].Line != 3 {
		t.Errorf("files = %v, notes = %+v", files, notes)
	}
}

func TestAttachCompose(t *testing.T) {
	proj := func(eco, dir string) plan.Project {
		return plan.Project{Ecosystem: eco, Dir: dir, Name: dir, Lockfiles: []string{"x"}}
	}
	got := attachCompose([]plan.Project{proj("npm", ".")}, []string{"a.yml", "test/compose.yml"}, "/r/repo")
	if len(got) != 1 || !slices.Equal(got[0].Compose, []string{"a.yml", "test/compose.yml"}) {
		t.Errorf("root npm: %+v", got)
	}

	got = attachCompose([]plan.Project{proj("npm", "web")}, []string{"compose.yml", "web/compose.yml"}, "/r/repo")
	if len(got) != 2 || got[0].Dir != "." || got[0].Ecosystem != "compose" || !slices.Equal(got[0].Compose, []string{"compose.yml"}) ||
		got[1].Dir != "web" || !slices.Equal(got[1].Compose, []string{"compose.yml"}) {
		t.Errorf("web: %+v", got)
	}
	if got[0].Name != "repo" || got[0].Lockfiles == nil || len(got[0].Lockfiles) != 0 {
		t.Errorf("compose-only project = %+v", got[0])
	}

	got = attachCompose([]plan.Project{proj("rust", "cli"), proj("npm", "cli/ui")}, []string{"cli/ui/dev/compose.yml"}, "/r/repo")
	if len(got) != 2 || len(got[0].Compose) != 0 || !slices.Equal(got[1].Compose, []string{"dev/compose.yml"}) {
		t.Errorf("deepest: %+v", got)
	}

	got = attachCompose([]plan.Project{proj("npm", "."), proj("npm", "a")}, []string{"a/compose.yml"}, "/r/repo")
	if len(got) != 2 || len(got[0].Compose) != 0 || !slices.Equal(got[1].Compose, []string{"compose.yml"}) {
		t.Errorf("one-letter dir: %+v", got)
	}

	got = attachCompose(nil, []string{"compose.yml"}, "/r/shop")
	if len(got) != 1 || got[0].Name != "shop" || got[0].Dir != "." || got[0].Ecosystem != "compose" {
		t.Errorf("none: %+v", got)
	}
}

func TestComputeComposeProjects(t *testing.T) {
	ci := &fakeCI{files: []string{fakeCIFile}}
	root := writeTree(t, map[string]string{"compose.yml": composeImage, fakeCIFile: "build: make\n"})
	p := computeWith(t, root, ci, nil)
	if p.Error != nil || len(p.Projects) != 1 || p.Projects[0].Ecosystem != "compose" ||
		!slices.Equal(p.Projects[0].Compose, []string{"compose.yml"}) || p.Projects[0].Name != filepath.Base(root) {
		t.Fatalf("plan = %+v, projects = %+v", p.Error, p.Projects)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"compose":["compose.yml"]`) || !strings.Contains(string(b), `"lockfiles":[]`) {
		t.Errorf("plan JSON lacks compose: %s", b)
	}

	root = writeTree(t, map[string]string{"compose.yml": composeBuild, fakeCIFile: "build: make\n"})
	p = computeWith(t, root, ci, nil)
	if p.Error == nil || p.Error.Code != "NO_PROJECT" || !strings.Contains(p.Error.Message, "compose.yml pulls no image") ||
		!strings.Contains(p.Error.Message, "no Docker Compose file that pulls images") {
		t.Fatalf("error = %+v", p.Error)
	}

	root = writeTree(t, npmTree(nil))
	p = computeWith(t, root, ci, nil)
	b, _ = json.Marshal(p)
	if strings.Contains(string(b), `"compose":`) {
		t.Errorf("npm plan JSON has a compose key: %s", b)
	}
}
