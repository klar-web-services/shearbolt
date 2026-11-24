package setup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
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
			p := Zosia(root, true, false)
			if p.Error != nil || len(p.Changes) != 0 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_SECRET"}) || !strings.Contains(p.Notes[0].Message, want) {
				t.Errorf("enable: error %+v, %d changes, notes %+v", p.Error, len(p.Changes), p.Notes)
			}
			p = Zosia(root, false, false)
			if p.Error != nil || len(p.Changes) != 0 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_VARIABLE", "ZOSIA_SECRET_STAYS"}) {
				t.Errorf("disable: error %+v, %d changes, notes %+v", p.Error, len(p.Changes), p.Notes)
			}
		})
	}
}

func TestZosiaWithoutAGate(t *testing.T) {
	root := t.TempDir()
	copyTree(t, filepath.Join(goldenDir, "gitlab", "before"), root)
	if p := Zosia(root, true, false); p.Error == nil || p.Error.Code != "NO_GATE" || !strings.Contains(p.Error.Message, "shearbolt setup") {
		t.Errorf("without a gate: %+v", p.Error)
	}
	if p := Zosia(t.TempDir(), true, false); p.Error == nil || p.Error.Code != "NO_CI" {
		t.Errorf("without CI: %+v", p.Error)
	}
}

// rewrite replaces old by new in a file of root, which must hold old.
func rewrite(t *testing.T, root, rel, old, new string) {
	t.Helper()
	path := filepath.Join(root, rel)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.ReplaceAll(string(src), old, new)
	if out == string(src) {
		t.Fatalf("%s does not hold %q", rel, old)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pinned is the gated GitLab fixture with its Shearbolt release moved to version.
func pinned(t *testing.T, version string) string {
	t.Helper()
	root := gated(t, "gitlab")
	rewrite(t, root, ".gitlab-ci.yml", "SHEARBOLT_VERSION=1.2.3", "SHEARBOLT_VERSION="+version)
	return root
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestZosiaPolicyAddsAndRemovesTheBlock(t *testing.T) {
	root := pinned(t, "1.11.0")
	before := read(t, root, "shearbolt.yaml")

	// Without --policy, enable leaves shearbolt.yaml alone.
	if p := Zosia(root, true, false); p.Error != nil || len(p.Changes) != 0 {
		t.Fatalf("enable: error %+v, %d changes", p.Error, len(p.Changes))
	}
	on := Zosia(root, true, true)
	if on.Error != nil || len(on.Changes) != 1 || on.Changes[0].Path != "shearbolt.yaml" ||
		!slices.Equal(codes(on.Notes), []string{"ZOSIA_SECRET", "ZOSIA_POLICY_ADDED", "ZOSIA_POLICY_FILE_COUNTS"}) {
		t.Fatalf("enable --policy: error %+v, changes %+v, notes %+v", on.Error, on.Changes, on.Notes)
	}
	if got := on.Changes[0].Content; got != before+"zosia:\n  policy: true\n" || !strings.Contains(on.Changes[0].Diff, "+zosia:\n+  policy: true\n") {
		t.Fatalf("shearbolt.yaml would become:\n%s\ndiff:\n%s", got, on.Changes[0].Diff)
	}
	if err := Write(root, on); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "shearbolt.yaml"), func(string) (string, bool) { return "", false }, time.Now())
	if err != nil || !cfg.Zosia.Policy || cfg.Policy != config.DefaultPolicy() {
		t.Fatalf("the edited file: %v, %+v", err, cfg)
	}
	if again := Zosia(root, true, true); again.Error != nil || len(again.Changes) != 0 || !slices.Contains(codes(again.Notes), "ZOSIA_POLICY_ALREADY") {
		t.Errorf("enable --policy again: error %+v, %d changes, notes %+v", again.Error, len(again.Changes), again.Notes)
	}

	// disable --policy removes the block and says nothing about the secret, which stays in use.
	off := Zosia(root, false, true)
	if off.Error != nil || len(off.Changes) != 1 || off.Changes[0].Content != before || !slices.Equal(codes(off.Notes), []string{"ZOSIA_POLICY_REMOVED"}) {
		t.Fatalf("disable --policy: error %+v, changes %+v, notes %+v", off.Error, off.Changes, off.Notes)
	}
	// Plain disable removes it as well, with what enable added.
	all := Zosia(root, false, false)
	if all.Error != nil || len(all.Changes) != 1 || all.Changes[0].Content != before ||
		!slices.Equal(codes(all.Notes), []string{"ZOSIA_VARIABLE", "ZOSIA_POLICY_REMOVED", "ZOSIA_SECRET_STAYS"}) {
		t.Fatalf("disable: error %+v, changes %+v, notes %+v", all.Error, all.Changes, all.Notes)
	}
	if err := Write(root, off); err != nil {
		t.Fatal(err)
	}
	if p := Zosia(root, false, true); p.Error != nil || len(p.Changes) != 0 || len(p.Notes) != 0 {
		t.Errorf("disable --policy with nothing to remove: error %+v, changes %+v, notes %+v", p.Error, p.Changes, p.Notes)
	}
}

func TestZosiaPolicyLeavesAFileAloneThatAnOlderGateReads(t *testing.T) {
	// The fixture pins 1.2.3, which would fail on the block.
	root := gated(t, "gitlab")
	p := Zosia(root, true, true)
	if p.Error != nil || len(p.Changes) != 0 || !slices.Equal(codes(p.Notes), []string{"ZOSIA_SECRET", "ZOSIA_POLICY_GATE_TOO_OLD"}) {
		t.Fatalf("error %+v, changes %+v, notes %+v", p.Error, p.Changes, p.Notes)
	}
	n := p.Notes[1]
	if n.File != ".gitlab-ci.yml" || n.Line == 0 || !strings.Contains(n.Message, "1.2.3") || !strings.Contains(n.Message, "1.11.0") || !strings.Contains(n.Message, "shearbolt.yaml was left as it is") {
		t.Errorf("note %+v", n)
	}
	// Removing is always safe: an older gate reads the file without the block.
	if err := os.WriteFile(filepath.Join(root, "shearbolt.yaml"), []byte(read(t, root, "shearbolt.yaml")+"zosia:\n  policy: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if off := Zosia(root, false, true); off.Error != nil || len(off.Changes) != 1 || !slices.Equal(codes(off.Notes), []string{"ZOSIA_POLICY_REMOVED"}) {
		t.Errorf("disable --policy: error %+v, changes %+v, notes %+v", off.Error, off.Changes, off.Notes)
	}
}

func TestZosiaPolicyOnBitbucket(t *testing.T) {
	root := gated(t, "bitbucket")
	if p := Zosia(root, true, true); p.Error != nil || len(p.Changes) != 0 || !slices.Contains(codes(p.Notes), "ZOSIA_POLICY_GATE_TOO_OLD") {
		t.Fatalf("pinned to 1.2.3: error %+v, changes %+v, notes %+v", p.Error, p.Changes, p.Notes)
	}
	rewrite(t, root, "bitbucket-pipelines.yml", "SHEARBOLT_VERSION=1.2.3", "SHEARBOLT_VERSION=1.11.0")
	p := Zosia(root, true, true)
	if p.Error != nil || len(p.Changes) != 1 || p.Changes[0].Path != "shearbolt.yaml" || !strings.HasSuffix(p.Changes[0].Content, "\nzosia:\n  policy: true\n") {
		t.Fatalf("error %+v, changes %+v, notes %+v", p.Error, p.Changes, p.Notes)
	}
}

func TestZosiaPolicyWhenAGatesConfigIsNotCertain(t *testing.T) {
	root := pinned(t, "1.11.0")
	rewrite(t, root, ".gitlab-ci.yml", `--config "$CI_PROJECT_DIR/shearbolt.yaml"`, `--config "$SOMEWHERE/shearbolt.yaml"`)
	on := Zosia(root, true, true)
	if on.Error != nil || len(on.Changes) != 0 || !slices.Equal(codes(on.Notes), []string{"ZOSIA_SECRET", "ZOSIA_POLICY_BY_HAND"}) {
		t.Fatalf("enable --policy: error %+v, changes %+v, notes %+v", on.Error, on.Changes, on.Notes)
	}
	if n := on.Notes[1]; n.File != ".gitlab-ci.yml" || n.Line == 0 || !strings.Contains(n.Message, "not certain which shearbolt.yaml") || !strings.Contains(n.Message, "policy: true") {
		t.Errorf("note %+v", n)
	}
	// Asked about the policy, disable says the same; plain disable is as quiet as it was before 1.11.0.
	if off := Zosia(root, false, true); off.Error != nil || !slices.Equal(codes(off.Notes), []string{"ZOSIA_POLICY_BY_HAND"}) {
		t.Errorf("disable --policy: error %+v, notes %+v", off.Error, off.Notes)
	}
	if off := Zosia(root, false, false); off.Error != nil || len(off.Changes) != 0 || !slices.Equal(codes(off.Notes), []string{"ZOSIA_VARIABLE", "ZOSIA_SECRET_STAYS"}) {
		t.Errorf("disable: error %+v, changes %+v, notes %+v", off.Error, off.Changes, off.Notes)
	}
}

func TestZosiaDisableSaysWhenABlockStays(t *testing.T) {
	root := pinned(t, "1.11.0")
	// An anchor on the block: removing the entry would break whatever refers to it.
	path := filepath.Join(root, "shearbolt.yaml")
	if err := os.WriteFile(path, []byte(read(t, root, "shearbolt.yaml")+"zosia: &z\n  policy: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []bool{false, true} {
		off := Zosia(root, false, policy)
		if off.Error != nil || len(off.Changes) != 0 || !slices.Contains(codes(off.Notes), "ZOSIA_POLICY_KEPT") {
			t.Fatalf("disable (--policy %v): error %+v, changes %+v, notes %+v", policy, off.Error, off.Changes, off.Notes)
		}
	}
}

func TestZosiaPolicyWithTheActionAtACommit(t *testing.T) {
	root := gated(t, "go-github")
	rewrite(t, root, ".github/workflows/release.yml", "klar-web-services/shearbolt@v1", "klar-web-services/shearbolt@"+strings.Repeat("a1b2", 10))
	p := Zosia(root, true, true)
	if p.Error != nil || !slices.Contains(codes(p.Notes), "ZOSIA_POLICY_GATE_UNKNOWN") || !slices.Contains(codes(p.Notes), "ZOSIA_POLICY_ADDED") {
		t.Fatalf("error %+v, notes %+v", p.Error, p.Notes)
	}
	// Without --policy nobody is told about a release that does not matter then.
	if p := Zosia(root, true, false); slices.Contains(codes(p.Notes), "ZOSIA_POLICY_GATE_UNKNOWN") {
		t.Errorf("enable: notes %+v", p.Notes)
	}
}

func TestZosiaPolicyInAMonorepo(t *testing.T) {
	root := gated(t, "monorepo")
	p := Zosia(root, true, true)
	if p.Error != nil {
		t.Fatal(p.Error)
	}
	var files []string
	for _, c := range p.Changes {
		if strings.HasSuffix(c.Path, "shearbolt.yaml") {
			files = append(files, c.Path)
			if !strings.HasSuffix(c.Content, "\nzosia:\n  policy: true\n") {
				t.Errorf("%s would become:\n%s", c.Path, c.Content)
			}
		}
	}
	if !slices.Equal(files, []string{"apps/api/shearbolt.yaml", "apps/web/shearbolt.yaml", "crates/cli/shearbolt.yaml"}) {
		t.Fatalf("changed %v, notes %+v", files, p.Notes)
	}
}

func TestInstallPinIsTheOldestPinnedRelease(t *testing.T) {
	for src, want := range map[string]string{
		"":                                "",
		"- curl … | sh":                   "",
		"| SHEARBOLT_VERSION=1.10.0 sh":   "1.10.0",
		"SHEARBOLT_VERSION=\"1.11.0\" sh": "1.11.0",
		"SHEARBOLT_VERSION=1.11.0\nSHEARBOLT_VERSION=1.9.2\nSHEARBOLT_VERSION=2.0.0": "1.9.2",
		"SHEARBOLT_VERSION=$VERSION":                 "",
		"variables:\n  SHEARBOLT_VERSION: \"1.9.0\"": "1.9.0",
		"env:\n  SHEARBOLT_VERSION : 1.10.3":         "1.10.3",
	} {
		if got := installPin([]byte(src)); got != want {
			t.Errorf("%q: %q, want %q", src, got, want)
		}
	}
	for v, want := range map[string]bool{"": true, "1.11.0": true, "1.12.3": true, "2.0.0": true, "1.10.0": false, "1.9.9": false, "0.9.0": false} {
		if knowsZosiaPolicy(v) != want {
			t.Errorf("knowsZosiaPolicy(%q) = %v", v, !want)
		}
	}
}

func TestSetZosiaPolicy(t *testing.T) {
	const base = "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\n"
	for name, c := range map[string]struct {
		src   string
		on    bool
		want  string
		state string
	}{
		"add":                           {base, true, base + "zosia:\n  policy: true\n", "added"},
		"add, no newline at the end":    {strings.TrimSuffix(base, "\n"), true, base + "zosia:\n  policy: true", "added"},
		"add, Windows line ends":        {strings.ReplaceAll(base, "\n", "\r\n"), true, strings.ReplaceAll(base+"zosia:\n  policy: true\n", "\n", "\r\n"), "added"},
		"add before a trailing comment": {base + "# the end\n", true, base + "zosia:\n  policy: true\n# the end\n", "added"},
		"switch on":                     {base + "zosia:\n  policy: false # not yet\n", true, base + "zosia:\n  policy: true # not yet\n", "added"},
		"already":                       {base + "zosia:\n  policy: true\n", true, base + "zosia:\n  policy: true\n", "already"},
		"remove":                        {base + "zosia:\n  policy: true\n", false, base, "removed"},
		"remove in the middle":          {"version: 1\nzosia:\n  policy: true\nproject: p\n", false, "version: 1\nproject: p\n", "removed"},
		"remove one that is off":        {base + "zosia:\n  policy: false\n", false, base, "removed"},
		"remove a flow block":           {base + "zosia: {policy: true}\n", false, base, "removed"},
		"nothing to remove":             {base, false, base, ""},
	} {
		got, state, err := setZosiaPolicy([]byte(c.src), c.on)
		if err != nil || string(got) != c.want || state != c.state {
			t.Errorf("%s: %q, state %q, error %v; want %q, %q", name, got, state, err, c.want, c.state)
		}
	}
	for name, src := range map[string]string{
		"a block without a policy line": base + "zosia: {}\n",
		"a flow block that is off":      base + "zosia: {policy: false}\n",
		"no mapping":                    "- a\n",
	} {
		if got, _, err := setZosiaPolicy([]byte(src), true); err == nil {
			t.Errorf("%s: no error, got %q", name, got)
		}
	}
}
