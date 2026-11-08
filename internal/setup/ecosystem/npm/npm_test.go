package npm

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

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func find(t *testing.T, files map[string]string) (string, []plan.Project, []plan.Note) {
	t.Helper()
	root := t.TempDir()
	write(t, root, files)
	projects, notes, err := Plugin{}.Find(plan.Repo{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return root, projects, notes
}

func TestFindNextProject(t *testing.T) {
	_, projects, notes := find(t, map[string]string{
		"package.json":      `{"name":"@acme/web","dependencies":{"next":"15.0.0"}}`,
		"package-lock.json": "{}",
	})
	want := []plan.Project{{Ecosystem: "npm", Dir: ".", Name: "@acme/web", Lockfiles: []string{"package-lock.json"}, Framework: "nextjs"}}
	if !reflect.DeepEqual(projects, want) || len(notes) != 0 {
		t.Fatalf("projects = %+v, notes = %+v", projects, notes)
	}
}

func TestFindNameFallbackAndDevDependencies(t *testing.T) {
	root, projects, _ := find(t, map[string]string{
		"package.json":           `{"private":true}`,
		"pnpm-lock.yaml":         "",
		"apps/site/package.json": `{"devDependencies":{"next":"15"}}`,
		"apps/site/yarn.lock":    "",
		"apps/bad/package.json":  `{not json`,
		"apps/bad/bun.lock":      "",
	})
	want := []plan.Project{
		{Ecosystem: "npm", Dir: ".", Name: filepath.Base(root), Lockfiles: []string{"pnpm-lock.yaml"}},
		{Ecosystem: "npm", Dir: "apps/bad", Name: "bad", Lockfiles: []string{"bun.lock"}},
		{Ecosystem: "npm", Dir: "apps/site", Name: "site", Lockfiles: []string{"yarn.lock"}, Framework: "nextjs"},
	}
	if !reflect.DeepEqual(projects, want) {
		t.Fatalf("projects = %+v\nwant %+v", projects, want)
	}
}

func TestFindBunLockb(t *testing.T) {
	_, projects, notes := find(t, map[string]string{
		"package.json": `{"name":"x"}`,
		"bun.lockb":    "\x00binary",
	})
	if len(projects) != 0 {
		t.Fatalf("bun.lockb alone gave projects %+v", projects)
	}
	if len(notes) != 1 || notes[0].Code != "BUN_LOCKB" || !strings.Contains(notes[0].Message, "bun install --save-text-lockfile") || notes[0].File != "bun.lockb" {
		t.Fatalf("notes = %+v", notes)
	}

	_, projects, notes = find(t, map[string]string{
		"package.json": `{"name":"x"}`,
		"bun.lockb":    "\x00binary",
		"bun.lock":     "{}",
	})
	if len(projects) != 1 || !slices.Equal(projects[0].Lockfiles, []string{"bun.lock"}) || len(notes) != 0 {
		t.Fatalf("bun.lockb plus bun.lock: projects = %+v, notes = %+v", projects, notes)
	}
}

func TestFindWorkspaces(t *testing.T) {
	files := map[string]string{
		"package.json":            `{"name":"mono","private":true,"workspaces":["packages/*"]}`,
		"yarn.lock":               "",
		"packages/a/package.json": `{"name":"a"}`,
	}
	_, projects, _ := find(t, files)
	if len(projects) != 1 || projects[0].Dir != "." {
		t.Fatalf("a workspace member without a lockfile is a project: %+v", projects)
	}
	files["nested/app/package.json"] = `{"name":"app"}`
	files["nested/app/yarn.lock"] = ""
	_, projects, _ = find(t, files)
	if len(projects) != 2 || projects[1].Dir != "nested/app" || projects[1].Name != "app" {
		t.Fatalf("projects = %+v", projects)
	}
}

func TestFindIgnoresSkippedDirectories(t *testing.T) {
	_, projects, notes := find(t, map[string]string{
		"node_modules/x/package.json":      `{"name":"x"}`,
		"node_modules/x/package-lock.json": "{}",
		".cache/y/package.json":            `{"name":"y"}`,
		".cache/y/package-lock.json":       "{}",
		"node_modules/z/package.json":      `{"name":"z"}`,
		"node_modules/z/bun.lockb":         "",
	})
	if len(projects) != 0 || len(notes) != 0 {
		t.Fatalf("projects = %+v, notes = %+v", projects, notes)
	}
}

func recognize(t *testing.T, script string) *plan.Build {
	t.Helper()
	cmds, _ := shell.Parse(script, shell.State{Dir: "."})
	if len(cmds) != 1 {
		t.Fatalf("%q parsed into %d commands", script, len(cmds))
	}
	return Plugin{}.Recognize(plan.Repo{Root: t.TempDir()}, cmds[0], plan.Context{Dir: "."})
}

func TestRecognizeBuilds(t *testing.T) {
	for _, s := range []string{
		"npm run build", "npm run build:prod", "npm run-script build", "pnpm build", "pnpm run build",
		"pnpm build:web", "yarn build", "yarn run build", "bun run build", "npx next build", "next build",
		"pnpm exec next build", "bunx next build", "npm run build -- --debug", "CI=1 npm run build",
		"npm --silent run build", "./node_modules/.bin/next build",
	} {
		b := recognize(t, s)
		if b == nil {
			t.Errorf("%q: not a build", s)
			continue
		}
		if b.Kind != plan.BuildOther || b.Dir != "." || b.Image != nil || b.Paths != nil {
			t.Errorf("%q: build = %+v", s, b)
		}
		if b.Cmd.Words[0].Start != 0 {
			t.Errorf("%q: Cmd is not the recognized command", s)
		}
	}
}

func TestRecognizeBuildDirectory(t *testing.T) {
	for s, dir := range map[string]string{
		"npm --prefix apps/web run build":   "apps/web",
		"npm run build --prefix=apps/web":   "apps/web",
		"pnpm -C apps/web build":            "apps/web",
		"pnpm --dir apps/web run build":     "apps/web",
		"yarn --cwd apps/web build":         "apps/web",
		"bun run --cwd apps/web build":      "apps/web",
		"npm --prefix ./apps/web run build": "apps/web",
		"npm --prefix $APP run build":       "",
		"npm --prefix /srv/app run build":   "",
		"npm --prefix ../x run build":       "",
	} {
		b := recognize(t, s)
		if b == nil || b.Dir != dir {
			t.Errorf("%q: build = %+v, want Dir %q", s, b, dir)
		}
	}
	cmds, _ := shell.Parse("cd $X && npm run build", shell.State{Dir: "."})
	b := Plugin{}.Recognize(plan.Repo{Root: t.TempDir()}, cmds[1], plan.Context{Dir: cmds[1].Before.Dir})
	if b == nil || b.Dir != "" {
		t.Fatalf("unknown directory: build = %+v", b)
	}
}

func TestRecognizeNonBuilds(t *testing.T) {
	for _, s := range []string{
		"npm test", "npm run lint", "bun build ./index.ts", "npm ci", "npm install", "npm run",
		"pnpm install", "yarn", "yarn test", "npm run build-storybook", "npx eslint .", "next dev",
		"npm publish", "echo npm run build", "pnpm exec next lint", "bun run test",
	} {
		if b := recognize(t, s); b != nil {
			t.Errorf("%q: recognized as a build: %+v", s, b)
		}
	}
}

func TestPrepareDoesNothing(t *testing.T) {
	b := recognize(t, "npm run build")
	prep, notes := Plugin{}.Prepare(plan.Repo{Root: t.TempDir()}, plan.Project{Dir: "."}, *b)
	if !reflect.DeepEqual(prep, plan.Prep{}) || notes != nil {
		t.Fatalf("Prepare = %+v, %+v", prep, notes)
	}
}

func TestLockfilesAreScanned(t *testing.T) {
	for _, l := range lockfiles {
		if !slices.Contains(scan.Lockfiles, l) {
			t.Errorf("%s is not in scan.Lockfiles", l)
		}
	}
	if (Plugin{}).Name() != "npm" {
		t.Fatal("Name")
	}
}
