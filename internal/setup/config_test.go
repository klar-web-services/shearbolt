package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

var noEnv = func(string) (string, bool) { return "", false }

func codes(notes []plan.Note) []string {
	var out []string
	for _, n := range notes {
		out = append(out, n.Code)
	}
	return out
}

func targetNames(ts []config.Target) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// loadConfig writes content to a temp shearbolt.yaml and loads it with an empty environment.
func loadConfig(t *testing.T, content string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shearbolt.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p, noEnv, time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("config.Load: %v\n%s", err, content)
	}
	return cfg
}

func TestConfigNewFile(t *testing.T) {
	r := repo(t, nil)
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "@acme/web"}
	planned := []config.Target{
		{Name: "source", Source: "."},
		{Name: "web", Image: "${SHEARBOLT_IMAGE:-web:local}"},
	}
	ch, merged, names, vars, notes := configChange(r, p, planned)
	if ch == nil || len(notes) != 0 {
		t.Fatalf("change %v, notes %v", ch, notes)
	}
	want := "# Shearbolt deployment gate: https://github.com/klar-web-services/shearbolt#readme\n" +
		"version: 1\n" +
		"project: '@acme/web'\n" +
		"targets:\n" +
		"  - name: web\n" +
		"    image: ${SHEARBOLT_IMAGE:-web:local}\n" +
		"  - name: source\n" +
		"    source: .\n"
	if ch.Content != want {
		t.Fatalf("content:\n%s\nwant:\n%s", ch.Content, want)
	}
	if ch.Path != "shearbolt.yaml" || ch.Action != "create" || ch.Base != nil || !strings.HasPrefix(ch.Diff, "--- /dev/null\n+++ b/shearbolt.yaml\n") {
		t.Fatalf("change: %+v", ch)
	}
	if got := targetNames(merged); !slices.Equal(got, []string{"web", "source"}) {
		t.Fatalf("merged: %v", got)
	}
	if names["web"] != "web" || names["source"] != "source" || vars["web"] != "SHEARBOLT_IMAGE" {
		t.Fatalf("names %v, vars %v", names, vars)
	}
	cfg := loadConfig(t, ch.Content)
	if cfg.Targets[0].Image != "web:local" || cfg.Project != "@acme/web" {
		t.Fatalf("loaded: %+v", cfg)
	}
}

func TestConfigLayoutAndExcludes(t *testing.T) {
	r := repo(t, nil)
	p := plan.Project{Ecosystem: "rust", Dir: "crates/cli", Name: "cli", Excludes: []string{"tools/cli/", "testdata/"}}
	planned := []config.Target{
		{Name: "source", Source: ".", Exclude: []string{"tools/cli/", "testdata/"}},
		{Name: "cli", File: "target/release/cli", LockfileFallback: "Cargo.lock"},
		{Name: "cli-image", Image: "${SHEARBOLT_IMAGE:-cli-image:local}"},
	}
	ch, _, _, _, notes := configChange(r, p, planned)
	if ch == nil || len(notes) != 0 {
		t.Fatalf("change %v, notes %v", ch, notes)
	}
	want := "# Shearbolt deployment gate: https://github.com/klar-web-services/shearbolt#readme\n" +
		"version: 1\n" +
		"project: cli\n" +
		"targets:\n" +
		"  - name: cli-image\n" +
		"    image: ${SHEARBOLT_IMAGE:-cli-image:local}\n" +
		"  - name: cli\n" +
		"    file: target/release/cli\n" +
		"    lockfileFallback: Cargo.lock\n" +
		"  - name: source\n" +
		"    source: .\n" +
		"    exclude: [testdata/, tools/cli/]\n"
	if ch.Content != want || ch.Path != "crates/cli/shearbolt.yaml" {
		t.Fatalf("%s:\n%s\nwant:\n%s", ch.Path, ch.Content, want)
	}
	cfg := loadConfig(t, ch.Content)
	if src := cfg.Targets[2]; !slices.Equal(src.Exclude, []string{"testdata/", "tools/cli/"}) {
		t.Fatalf("excludes: %v", src.Exclude)
	}
	// Entries that config.Load would reject as globs are left out (the scan covers more).
	p.Excludes = []string{"a[1]/", "b/"}
	planned[0].Exclude = p.Excludes
	ch, _, _, _, _ = configChange(r, p, planned)
	if !strings.Contains(ch.Content, "exclude: [b/]\n") {
		t.Fatalf("glob-like exclude:\n%s", ch.Content)
	}
	loadConfig(t, ch.Content)
}

const shopConfig = `version: 1
project: shop   # owned by platform team
targets:
  - name: repo
    source: .
policy:
  blockFixable: high
exceptions:
  - id: CVE-2026-1
    package: lodash@4.17.20
    reason: build only
    approvedBy: alice
    expires: 2026-12-01
`

func TestConfigMerge(t *testing.T) {
	r := repo(t, map[string]string{"shearbolt.yaml": shopConfig})
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "shop"}
	planned := []config.Target{
		{Name: "shop", Image: "${SHEARBOLT_IMAGE:-shop:local}"},
		{Name: "source", Source: "."},
	}
	ch, merged, names, vars, notes := configChange(r, p, planned)
	if ch == nil || len(notes) != 0 {
		t.Fatalf("change %v, notes %v", ch, notes)
	}
	want := strings.Replace(shopConfig, "    source: .\n", "    source: .\n  - name: shop\n    image: ${SHEARBOLT_IMAGE:-shop:local}\n", 1)
	if ch.Content != want {
		t.Fatalf("content:\n%s\nwant:\n%s", ch.Content, want)
	}
	if ch.Action != "modify" || string(ch.Base) != shopConfig {
		t.Fatalf("change: %+v", ch)
	}
	if names["source"] != "repo" || names["shop"] != "shop" || vars["shop"] != "SHEARBOLT_IMAGE" {
		t.Fatalf("names %v, vars %v", names, vars)
	}
	if got := targetNames(merged); !slices.Equal(got, []string{"repo", "shop"}) {
		t.Fatalf("merged: %v", got)
	}

	// A second run changes nothing.
	r2 := repo(t, map[string]string{"shearbolt.yaml": ch.Content})
	ch2, merged2, names2, _, notes2 := configChange(r2, p, planned)
	if ch2 != nil || len(notes2) != 0 || names2["source"] != "repo" || len(merged2) != 2 {
		t.Fatalf("re-run: %+v %v %v %v", ch2, notes2, names2, merged2)
	}
}

func TestConfigMergeCRLFAndIndentation(t *testing.T) {
	src := "version: 1\r\nproject: x\r\ntargets:\r\n- name: source\r\n  source: .\r\n# trailing comment\r\n"
	r := repo(t, map[string]string{"shearbolt.yaml": src})
	p := plan.Project{Ecosystem: "rust", Dir: ".", Name: "x"}
	ch, _, _, _, notes := configChange(r, p, []config.Target{
		{Name: "source", Source: "."},
		{Name: "x", File: "target/release/x", LockfileFallback: "Cargo.lock"},
	})
	if ch == nil || len(notes) != 0 {
		t.Fatalf("change %v, notes %v", ch, notes)
	}
	want := "version: 1\r\nproject: x\r\ntargets:\r\n- name: source\r\n  source: .\r\n- name: x\r\n  file: target/release/x\r\n  lockfileFallback: Cargo.lock\r\n# trailing comment\r\n"
	if ch.Content != want {
		t.Fatalf("content:\n%q\nwant:\n%q", ch.Content, want)
	}
}

func TestConfigExistingImageTargets(t *testing.T) {
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "shop"}
	planned := []config.Target{
		{Name: "shop", Image: "${SHEARBOLT_IMAGE:-shop:local}"},
		{Name: "source", Source: "."},
	}
	fixed := "version: 1\nproject: shop\ntargets:\n  - name: shop\n    image: ghcr.io/acme/shop:${GITHUB_SHA:-local}\n  - name: source\n    source: .\n"
	ch, merged, names, vars, notes := configChange(repo(t, map[string]string{"shearbolt.yaml": fixed}), p, planned)
	if ch != nil {
		t.Fatalf("fixed image reference: unexpected change:\n%s", ch.Content)
	}
	if got := codes(notes); !slices.Equal(got, []string{"TARGET_EXISTS", "IMAGE_REF_FIXED"}) {
		t.Fatalf("notes: %v", notes)
	}
	if !strings.Contains(notes[1].Message, "${SHEARBOLT_IMAGE:-shop:local}") || notes[1].File != "shearbolt.yaml" {
		t.Fatalf("IMAGE_REF_FIXED: %+v", notes[1])
	}
	if _, ok := vars["shop"]; ok || names["shop"] != "shop" || len(merged) != 2 {
		t.Fatalf("names %v, vars %v, merged %v", names, vars, merged)
	}

	named := "version: 1\nproject: shop\ntargets:\n  - name: shop\n    image: ${SHEARBOLT_IMAGE_SHOP:-shop:local}\n  - name: source\n    source: .\n"
	ch, _, names, vars, notes = configChange(repo(t, map[string]string{"shearbolt.yaml": named}), p, planned)
	if ch != nil || len(notes) != 0 || vars["shop"] != "SHEARBOLT_IMAGE_SHOP" || names["shop"] != "shop" {
		t.Fatalf("existing variable: %v %v %v %v", ch, notes, names, vars)
	}
}

func TestConfigNameCollisions(t *testing.T) {
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "web"}
	planned := []config.Target{
		{Name: "web", Image: "${SHEARBOLT_IMAGE:-web:local}"},
		{Name: "source", Source: "."},
	}
	// An existing target with the planned image's value under another name is reused.
	src := "version: 1\nproject: web\ntargets:\n  - name: app\n    image: ${SHEARBOLT_IMAGE:-web:local}\n  - name: source\n    source: ./\n"
	ch, _, names, vars, notes := configChange(repo(t, map[string]string{"shearbolt.yaml": src}), p, planned)
	if ch != nil || len(notes) != 0 || names["web"] != "app" || vars["app"] != "SHEARBOLT_IMAGE" || names["source"] != "source" {
		t.Fatalf("value match: %v %v %v %v", ch, notes, names, vars)
	}
	// Same name, other kind: the planned target is added under a free name, so the image is
	// still scanned.
	src = "version: 1\nproject: web\ntargets:\n  - name: web\n    dir: public\n  - name: source\n    source: .\n"
	ch, merged, names, vars, notes := configChange(repo(t, map[string]string{"shearbolt.yaml": src}), p, planned)
	if ch == nil || names["web"] != "web-2" || vars["web-2"] != "SHEARBOLT_IMAGE" || !slices.Equal(codes(notes), []string{"TARGET_EXISTS"}) {
		t.Fatalf("kind collision: %v %v %v %v", ch, notes, names, vars)
	}
	if got := targetNames(merged); !slices.Equal(got, []string{"web", "source", "web-2"}) {
		t.Fatalf("merged: %v", got)
	}
	loadConfig(t, ch.Content)
	// Re-run: the added target is found by its value.
	ch, _, names, _, notes = configChange(repo(t, map[string]string{"shearbolt.yaml": ch.Content}), p, planned)
	if ch != nil || len(notes) != 0 || names["web"] != "web-2" {
		t.Fatalf("kind collision re-run: %v %v %v", ch, notes, names)
	}
	// Same name and kind, other value (not an image): left alone.
	src = "version: 1\nproject: web\ntargets:\n  - name: source\n    source: app\n"
	ch, _, names, _, notes = configChange(repo(t, map[string]string{"shearbolt.yaml": src}), p, planned[1:])
	if ch != nil || names["source"] != "source" || !slices.Equal(codes(notes), []string{"TARGET_EXISTS"}) {
		t.Fatalf("value collision: %v %v %v", ch, notes, names)
	}
}

func TestConfigUnsupported(t *testing.T) {
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "web"}
	planned := []config.Target{{Name: "web", Image: "${SHEARBOLT_IMAGE:-web:local}"}, {Name: "source", Source: "."}}
	for name, src := range map[string]string{
		"flow":       "version: 1\nproject: web\ntargets: [{name: source, source: .}]\n",
		"no targets": "version: 1\nproject: web\n",
		"mapping":    "version: 1\nproject: web\ntargets:\n  source: .\n",
		"invalid":    "version: 1\nproject: [web\n",
		"not a map":  "- a\n- b\n",
	} {
		ch, merged, names, vars, notes := configChange(repo(t, map[string]string{"shearbolt.yaml": src}), p, planned)
		if ch != nil || !slices.Equal(codes(notes), []string{"UNSUPPORTED_EDIT"}) {
			t.Errorf("%s: change %v, notes %v", name, ch, notes)
			continue
		}
		if !strings.Contains(notes[0].Message, "- name: web\n    image: ${SHEARBOLT_IMAGE:-web:local}") || notes[0].File != "shearbolt.yaml" {
			t.Errorf("%s: message %q", name, notes[0].Message)
		}
		// The gates then scan every target of whatever config there is.
		if merged != nil || names["web"] != "web" || vars["web"] != "SHEARBOLT_IMAGE" {
			t.Errorf("%s: merged %v, names %v, vars %v", name, merged, names, vars)
		}
	}
}

func TestIgnoreGitignore(t *testing.T) {
	r := repo(t, map[string]string{".gitignore": "node_modules/"})
	ch := ignoreChange(r, ".gitignore")
	if ch == nil || ch.Content != "node_modules/\n.shearbolt/\n" || ch.Action != "modify" {
		t.Fatalf("append: %+v", ch)
	}
	for _, have := range []string{".shearbolt", ".shearbolt/", ".shearbolt/  "} {
		r := repo(t, map[string]string{".gitignore": "node_modules/\n" + have + "\n"})
		if ch := ignoreChange(r, ".gitignore"); ch != nil {
			t.Errorf("%q present: %+v", have, ch)
		}
	}
	// An anchored entry covers only the root, not nested projects' .shearbolt/.
	for _, anchored := range []string{"/.shearbolt", "/.shearbolt/"} {
		r := repo(t, map[string]string{".gitignore": anchored + "\n"})
		if ch := ignoreChange(r, ".gitignore"); ch == nil || ch.Content != anchored+"\n.shearbolt/\n" {
			t.Errorf("%q: %+v", anchored, ch)
		}
	}
	r = repo(t, nil)
	if ch := ignoreChange(r, ".gitignore"); ch == nil || ch.Content != ".shearbolt/\n" || ch.Action != "create" || ch.Base != nil {
		t.Fatalf("create: %+v", ch)
	}
	r = repo(t, map[string]string{".gitignore": "a\r\nb"})
	if ch := ignoreChange(r, ".gitignore"); ch == nil || ch.Content != "a\r\nb\r\n.shearbolt/\r\n" {
		t.Fatalf("CRLF: %+v", ch)
	}
	r = repo(t, map[string]string{".gitignore": ""})
	if ch := ignoreChange(r, ".gitignore"); ch == nil || ch.Content != ".shearbolt/\n" || ch.Action != "modify" {
		t.Fatalf("empty: %+v", ch)
	}
}

func TestIgnoreDockerignore(t *testing.T) {
	r := repo(t, map[string]string{
		"apps/web/Dockerfile":    "FROM node\n",
		"apps/blog/package.json": "{}\n",
		"svc/Containerfile":      "FROM x\n",
		"ctx/.dockerignore":      ".shearbolt/\n",
		"ctx2/x":                 "",
	})
	projects := []plan.Project{
		{Ecosystem: "npm", Dir: "apps/web"},
		{Ecosystem: "npm", Dir: "apps/blog"},
		{Ecosystem: "npm", Dir: "svc"},
	}
	gated := []plan.Build{
		{Kind: plan.BuildImage, Image: &plan.Image{Tool: "railpack", Context: "apps/blog"}},
		{Kind: plan.BuildImage, Image: &plan.Image{Tool: "docker", Context: "ctx"}},
		{Kind: plan.BuildImage, Image: &plan.Image{Tool: "build-push-action", Context: "ctx2"}},
		{Kind: plan.BuildImage, Image: &plan.Image{Tool: "buildx", Context: "missing"}},
		{Kind: plan.BuildImage, Image: &plan.Image{Tool: "docker", Context: ""}},
	}
	var paths []string
	for _, ch := range ignoreChanges(r, projects, gated) {
		paths = append(paths, ch.Path)
		if !strings.HasSuffix(ch.Content, ".shearbolt/\n") {
			t.Errorf("%s: %q", ch.Path, ch.Content)
		}
	}
	want := []string{".gitignore", "apps/web/.dockerignore", "ctx2/.dockerignore", "svc/.dockerignore"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
}

// Review 4: an invalid target name is caught by the read-back check, for a new file and for a
// merge; excludes with variables are left out so the file loads with an empty environment.
func TestConfigReadBackSafetyNet(t *testing.T) {
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "web"}
	bad := []config.Target{{Name: "a b", Image: "${SHEARBOLT_IMAGE:-web:local}"}, {Name: "source", Source: "."}}
	for name, files := range map[string]map[string]string{
		"new":   nil,
		"merge": {"shearbolt.yaml": "version: 1\nproject: web\ntargets:\n  - name: source\n    source: .\n"},
	} {
		ch, merged, _, _, notes := configChange(repo(t, files), p, bad)
		if ch != nil || merged != nil || !slices.Equal(codes(notes), []string{"UNSUPPORTED_EDIT"}) {
			t.Errorf("%s: change %v, notes %v", name, ch, notes)
		}
	}
	ch, _, _, _, notes := configChange(repo(t, nil), p, []config.Target{{Name: "source", Source: ".", Exclude: []string{"a${X}/", "b$c/", "x/"}}})
	if ch == nil || len(notes) != 0 || !strings.Contains(ch.Content, "exclude: [x/]\n") {
		t.Fatalf("variable excludes: %v %v", ch, notes)
	}
	loadConfig(t, ch.Content)
}

const composeNote = "setup gates the images of %s in shearbolt.yaml. A compose file that only runs locally or in tests ships nothing: remove its target, which setup will not add back, or exclude single services with exclude: [service]."

func TestConfigNewFileCompose(t *testing.T) {
	r := repo(t, nil)
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "web"}
	planned := []config.Target{
		{Name: "source", Source: "."},
		{Name: "docker-compose", Compose: "docker-compose.yml"},
		{Name: "web", Image: "${SHEARBOLT_IMAGE:-web:local}"},
		{Name: "dev", Compose: "test/dev.yml"},
	}
	ch, merged, names, _, notes := configChange(r, p, planned)
	if ch == nil {
		t.Fatal("no change")
	}
	want := "# Shearbolt deployment gate: https://github.com/klar-web-services/shearbolt#readme\n" +
		"version: 1\n" +
		"project: web\n" +
		"targets:\n" +
		"  - name: web\n" +
		"    image: ${SHEARBOLT_IMAGE:-web:local}\n" +
		"  - name: docker-compose\n" +
		"    compose: docker-compose.yml\n" +
		"  - name: dev\n" +
		"    compose: test/dev.yml\n" +
		"  - name: source\n" +
		"    source: .\n"
	if ch.Content != want {
		t.Fatalf("content:\n%s\nwant:\n%s", ch.Content, want)
	}
	if got := targetNames(merged); !slices.Equal(got, []string{"web", "docker-compose", "dev", "source"}) || names["dev"] != "dev" {
		t.Fatalf("merged %v, names %v", got, names)
	}
	if len(notes) != 1 || notes[0].Code != "COMPOSE_ADDED" || notes[0].File != "shearbolt.yaml" ||
		notes[0].Message != fmt.Sprintf(composeNote, "docker-compose.yml, test/dev.yml") {
		t.Fatalf("notes: %+v", notes)
	}
	if cfg := loadConfig(t, ch.Content); cfg.Targets[1].Compose == "" {
		t.Fatalf("loaded: %+v", cfg.Targets)
	}
}

func TestConfigNewFileComposeOnly(t *testing.T) {
	r := repo(t, nil)
	p := composeOnly("docker-compose.yml", "test/compose.yml")
	planned := []config.Target{{Name: "docker-compose", Compose: "docker-compose.yml"}, {Name: "test-compose", Compose: "test/compose.yml"}}
	ch, _, _, _, notes := configChange(r, p, planned)
	if ch == nil || len(notes) != 1 || notes[0].Code != "COMPOSE_ADDED" {
		t.Fatalf("change %v, notes %+v", ch, notes)
	}
	t.Logf("compose-only config:\n%s", ch.Content)
	cfg := loadConfig(t, ch.Content)
	if len(cfg.Targets) != 2 || cfg.Targets[0].Kind() != "compose" || !strings.HasSuffix(cfg.Targets[1].Compose, "/test/compose.yml") {
		t.Fatalf("loaded: %+v", cfg.Targets)
	}
}

func TestConfigExistingComposeNotGated(t *testing.T) {
	existing := "version: 1\ntargets:\n  - name: source\n    source: .\n"
	r := repo(t, map[string]string{"shearbolt.yaml": existing})
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "web"}
	planned := []config.Target{{Name: "docker-compose", Compose: "docker-compose.yml", Exclude: []string{"db"}}, {Name: "source", Source: "."}}
	ch, merged, names, _, notes := configChange(r, p, planned)
	if ch != nil {
		t.Fatalf("change: %+v", ch)
	}
	if got := targetNames(merged); !slices.Equal(got, []string{"source"}) || names["source"] != "source" {
		t.Fatalf("merged %v, names %v", got, names)
	}
	want := "docker-compose.yml is not gated: shearbolt.yaml has no compose target for it. If its images ship, add this to targets:\n" +
		"  - name: docker-compose\n    compose: docker-compose.yml\n    exclude: [db]"
	if len(notes) != 1 || notes[0].Code != "COMPOSE_NOT_GATED" || notes[0].File != "shearbolt.yaml" || notes[0].Message != want {
		t.Fatalf("notes: %+v", notes)
	}
}

func TestConfigExistingComposeOtherName(t *testing.T) {
	existing := "version: 1\ntargets:\n  - name: stack\n    compose: ./docker-compose.yml\n  - name: source\n    source: .\n"
	r := repo(t, map[string]string{"shearbolt.yaml": existing})
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "web"}
	planned := []config.Target{{Name: "docker-compose", Compose: "docker-compose.yml"}, {Name: "source", Source: "."}}
	ch, merged, names, _, notes := configChange(r, p, planned)
	if ch != nil || len(notes) != 0 || names["docker-compose"] != "stack" || len(merged) != 2 {
		t.Fatalf("change %v, notes %+v, names %v, merged %v", ch, notes, names, merged)
	}
}

// A compose target next to a missing image target: the image is appended, the compose target
// is not.
func TestConfigExistingComposeNeverAppended(t *testing.T) {
	existing := "version: 1\ntargets:\n  - name: source\n    source: .\n"
	r := repo(t, map[string]string{"shearbolt.yaml": existing})
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "web"}
	planned := []config.Target{{Name: "web", Image: "${SHEARBOLT_IMAGE:-web:local}"}, {Name: "compose", Compose: "compose.yml"}, {Name: "source", Source: "."}}
	ch, merged, _, _, notes := configChange(r, p, planned)
	if ch == nil || strings.Contains(ch.Content, "compose") || !slices.Equal(targetNames(merged), []string{"source", "web"}) {
		t.Fatalf("change %+v, merged %v", ch, targetNames(merged))
	}
	if !slices.Equal(codes(notes), []string{"COMPOSE_NOT_GATED"}) {
		t.Fatalf("notes: %v", codes(notes))
	}
}

func TestSortTargetsCompose(t *testing.T) {
	got := sortTargets([]config.Target{{Name: "s", Source: "."}, {Name: "c", Compose: "c.yml"}, {Name: "f", File: "f"}, {Name: "d", Dir: "d"}, {Name: "i", Image: "i"}})
	if n := targetNames(got); !slices.Equal(n, []string{"i", "d", "f", "c", "s"}) {
		t.Fatalf("order: %v", n)
	}
}

func TestReadsBackCompose(t *testing.T) {
	if readsBack([]byte("targets:\n  - name: a\n"), []config.Target{{Name: "a", Compose: "a.yml"}}) {
		t.Fatal("a bare item must not read back as a compose target")
	}
}

// Compose targets are create-only, so the by-hand snippet of an existing config that cannot be
// edited does not list them, and they get no note of their own.
func TestConfigExistingUnsupportedOmitsCompose(t *testing.T) {
	existing := "version: 1\ntargets: [{name: source, source: .}]\n"
	r := repo(t, map[string]string{"shearbolt.yaml": existing})
	p := plan.Project{Ecosystem: "npm", Dir: ".", Name: "web"}
	planned := []config.Target{{Name: "web", Image: "${SHEARBOLT_IMAGE:-web:local}"}, {Name: "docker-compose", Compose: "docker-compose.yml"}, {Name: "source", Source: "."}}
	ch, merged, _, _, notes := configChange(r, p, planned)
	if ch != nil || merged != nil || !slices.Equal(codes(notes), []string{"UNSUPPORTED_EDIT"}) {
		t.Fatalf("change %v, merged %v, notes %+v", ch, merged, notes)
	}
	if m := notes[0].Message; strings.Contains(m, "compose") || !strings.Contains(m, "image: ${SHEARBOLT_IMAGE:-web:local}") {
		t.Fatalf("message: %q", m)
	}
}
