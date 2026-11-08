package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func env(vars map[string]string) Env {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shearbolt.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaultsExpandsAndResolves(t *testing.T) {
	path := write(t, `
version: 1
project: my-app
targets:
  - name: web
    image: ghcr.io/acme/my-app:${GITHUB_SHA:-local}
  - name: cli
    file: target/release/my-cli
    lockfileFallback: Cargo.lock
  - name: repo
    source: .
    exclude: [examples/]
policy:
  blockFixable: high
exceptions:
  - id: CVE-2026-12345
    package: "@scope/lib@2.0.2"
    target: web
    reason: only used at build time
    approvedBy: alice
    expires: 2026-12-01
`)
	cfg, err := Load(path, env(nil), now)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if got := cfg.Targets[0].Image; got != "ghcr.io/acme/my-app:local" {
		t.Errorf("image = %q", got)
	}
	if got := cfg.Targets[1].File; got != filepath.Join(dir, "target/release/my-cli") {
		t.Errorf("file = %q", got)
	}
	if got := cfg.Targets[1].LockfileFallback; got != filepath.Join(dir, "Cargo.lock") {
		t.Errorf("lockfileFallback = %q", got)
	}
	if got := cfg.Targets[2].Source; got != dir {
		t.Errorf("source = %q", got)
	}
	p := cfg.Policy
	if !p.BlockKnownExploited || p.BlockEpssAtLeast != 0.10 || p.BlockFixable != "high" || p.MaxDatabaseAgeHours != 48 || p.MaxExceptionDays != 90 {
		t.Errorf("policy = %+v", p)
	}
	e := cfg.Exceptions[0]
	if e.PackageName() != "@scope/lib" || e.PackageVersion() != "2.0.2" {
		t.Errorf("package split = %q / %q", e.PackageName(), e.PackageVersion())
	}
	if cfg.Targets[0].Kind() != "image" || cfg.Targets[2].Kind() != "source" {
		t.Errorf("kinds = %s, %s", cfg.Targets[0].Kind(), cfg.Targets[2].Kind())
	}
}

func TestLoadUsesEnvironmentValue(t *testing.T) {
	path := write(t, "version: 1\nproject: p\ntargets:\n  - name: web\n    image: app:${GITHUB_SHA:-local}\n")
	cfg, err := Load(path, env(map[string]string{"GITHUB_SHA": "abc123"}), now)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets[0].Image != "app:abc123" {
		t.Fatalf("image = %q", cfg.Targets[0].Image)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := write(t, "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\n    sauce: oops\n")
	if _, err := Load(path, env(nil), now); err == nil || !strings.Contains(err.Error(), "sauce") {
		t.Fatalf("err = %v, want unknown field error", err)
	}
}

func TestLoadCollectsAllProblems(t *testing.T) {
	path := write(t, `
version: 2
targets:
  - name: a
    image: x
    file: y
  - name: a
    source: .
  - name: c
    image: app:${MISSING}
policy:
  blockEpssAtLeast: 0
  blockFixable: sometimes
exceptions:
  - id: CVE-1
    package: lodash@^4.0.0
    expires: soon
`)
	_, err := Load(path, env(nil), now)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"version must be 1", "project is required", "set exactly one of", "duplicate name",
		"MISSING is not set", "blockEpssAtLeast", "blockFixable", "reason is required",
		"approvedBy is required", "exact name@version", "expires must be a date",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestExceptionExpiryLimit(t *testing.T) {
	base := "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\nexceptions:\n  - {id: CVE-1, package: a@1.0.0, reason: r, approvedBy: b, expires: %s}\n"
	if _, err := Load(write(t, strings.Replace(base, "%s", "2027-02-06", 1)), env(nil), now); err != nil {
		t.Fatalf("90 days away should be accepted: %v", err)
	}
	if _, err := Load(write(t, strings.Replace(base, "%s", "2027-02-07", 1)), env(nil), now); err == nil || !strings.Contains(err.Error(), "maxExceptionDays") {
		t.Fatalf("91 days away should be rejected, got %v", err)
	}
}

func TestExpiresAtIsEndOfDayUTC(t *testing.T) {
	end, err := Exception{Expires: "2026-12-01"}.ExpiresAt()
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", end, want)
	}
}

func TestAdHocBuildsNamedTargets(t *testing.T) {
	dir := t.TempDir()
	cfg, err := AdHoc("demo", dir, []string{"a:1", "b:2"}, nil, nil, []string{"."}, env(nil), now)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tg := range cfg.Targets {
		names = append(names, tg.Name)
	}
	if strings.Join(names, ",") != "image,image-2,source" {
		t.Fatalf("names = %v", names)
	}
	if cfg.Targets[2].Source != dir {
		t.Fatalf("source = %q", cfg.Targets[2].Source)
	}
}

func TestDebianVersionWithTildeIsAccepted(t *testing.T) {
	path := write(t, "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\nexceptions:\n  - {id: CVE-1, package: \"libssl3@3.0.15-1~deb12u1\", reason: r, approvedBy: b, expires: 2026-12-01}\n")
	if _, err := Load(path, env(nil), now); err != nil {
		t.Fatalf("Debian version should be accepted: %v", err)
	}
}

func TestExpandRejectsMalformedReference(t *testing.T) {
	if _, err := Expand("app:${GITHUB_SHA:local}", env(nil)); err == nil || !strings.Contains(err.Error(), "malformed variable reference") {
		t.Fatalf("err = %v", err)
	}
}

func TestExpandListsMissingOnceAndSaysEmpty(t *testing.T) {
	_, err := Expand("${A}-${A}-${B}", env(map[string]string{"B": ""}))
	if err == nil || !strings.Contains(err.Error(), "A, B is not set or is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	path := write(t, "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\n---\nversion: 1\n")
	if _, err := Load(path, env(nil), now); err == nil || !strings.Contains(err.Error(), "single YAML document") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRequiresTargets(t *testing.T) {
	path := write(t, "version: 1\nproject: p\n")
	if _, err := Load(path, env(nil), now); err == nil || !strings.Contains(err.Error(), "at least one target is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadReportsMoreProblems(t *testing.T) {
	path := write(t, `
version: 1
project: p
targets:
  - name: "bad name"
    source: .
  - image: x
  - name: img
    image: x
    exclude: [a/]
  - name: src
    source: .
    lockfileFallback: Cargo.lock
  - name: empty
    image: ${EMPTY:-}
policy:
  maxDatabaseAgeHours: 0
  maxExceptionDays: 0
exceptions:
  - {id: CVE-1, package: a@1.0.0, target: nope, reason: r, approvedBy: b, expires: 2026-12-01}
`)
	_, err := Load(path, env(nil), now)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		`name "bad name" may only contain`, "name is required", "exclude is only allowed on source targets",
		"lockfileFallback is only allowed on file targets", "is empty after expanding variables",
		"maxDatabaseAgeHours must be a positive", "maxExceptionDays must be a positive", "target nope is not defined",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestLoadRejectsGlobExcludes(t *testing.T) {
	path := write(t, `
version: 1
project: p
targets:
  - name: src
    source: .
    exclude: ["examples/", "**/fixtures/**"]
`)
	_, err := Load(path, env(nil), now)
	if err == nil || !strings.Contains(err.Error(), "not globs") {
		t.Fatalf("err = %v, want glob rejection", err)
	}
	backslash := write(t, `
version: 1
project: p
targets:
  - name: src
    source: .
    exclude: ['a\\b']
`)
	if _, err := Load(backslash, env(nil), now); err == nil || !strings.Contains(err.Error(), "not globs") {
		t.Fatalf("backslash err = %v, want glob rejection", err)
	}
}

// imageConfig is a config with one image target named web whose image field is value.
func imageConfig(t *testing.T, value string) string {
	t.Helper()
	return write(t, "version: 1\nproject: p\ntargets:\n  - name: web\n    image: "+value+"\n")
}

func TestLoadAcceptsImageReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, ref := range []string{
		"ghcr.io/acme/web:1.2.3",
		"localhost:5000/x/y:tag",
		"repo@" + digest,
		"docker.io/library/node@" + digest,
		"ghcr.io/acme/web:1.2.3@" + digest,
		"web:local",
		"registry.example.com:443/team/app_x:v1.2.3-rc.1_build.7",
		"UPPER/Case:Tag",
	} {
		if _, err := Load(imageConfig(t, ref), env(nil), now); err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
	// Expansion results: the default, and a value from the environment.
	vars := map[string]string{"SHEARBOLT_IMAGE": "ghcr.io/acme/web:shearbolt-0123abc", "TAG": "1.2.3"}
	for _, value := range []string{
		"${SHEARBOLT_IMAGE:-web:local}",
		"${UNSET_IMAGE:-web:local}",
		"ghcr.io/acme/web:${TAG}",
		"${SHEARBOLT_IMAGE}",
	} {
		cfg, err := Load(imageConfig(t, value), env(vars), now)
		if err != nil {
			t.Errorf("%s: %v", value, err)
			continue
		}
		if strings.Contains(cfg.Targets[0].Image, "$") {
			t.Errorf("%s: not expanded: %q", value, cfg.Targets[0].Image)
		}
	}
}

// unsafeImageRefs could inject a command or an option when an image reference reaches a shell.
var unsafeImageRefs = []string{
	"app; rm -rf /", "app:1&&id", "app|tee", "app&", "$(id)", "app:`id`", "app'x", `app"x`,
	`app\x`, "app<x", "app>x", "app(x)", "app{x}", "app*", "app?", "app!", "app#x", "app~x",
	"app,x", "app=x", "app%x", "app+x", "app[0]", "a b", "app\tx", "app\nx", "app\x1b[31m",
	"app\x7f", "app\u00a0x", "-oops", "--output=type=local", ".hidden", "/abs", ":tag", "@digest",
}

func TestLoadRejectsUnsafeImageReferences(t *testing.T) {
	for _, ref := range unsafeImageRefs {
		// Written literally (as a YAML double-quoted string) and arriving through a variable.
		literal := imageConfig(t, strconv.Quote(ref))
		expanded := imageConfig(t, "${IMG}")
		for _, c := range []struct {
			path string
			vars map[string]string
		}{{literal, nil}, {expanded, map[string]string{"IMG": ref}}} {
			_, err := Load(c.path, env(c.vars), now)
			if err == nil {
				t.Errorf("%q (vars %v): accepted", ref, c.vars)
				continue
			}
			msg := err.Error()
			if !strings.Contains(msg, `target "web"`) || !strings.Contains(msg, "image reference") {
				t.Errorf("%q: error does not name the target and the image reference:\n%v", ref, err)
			}
			for _, r := range msg {
				if unicode.IsControl(r) && r != '\n' {
					t.Errorf("%q: error message holds control character %U:\n%q", ref, r, msg)
				}
			}
		}
	}
}

func TestAdHocRejectsUnsafeImageReferences(t *testing.T) {
	for _, ref := range unsafeImageRefs {
		if _, err := AdHoc("demo", t.TempDir(), []string{ref}, nil, nil, nil, env(nil), now); err == nil {
			t.Errorf("%q: accepted", ref)
		}
	}
	ok := "docker.io/library/node@sha256:" + strings.Repeat("cd", 32)
	if _, err := AdHoc("demo", t.TempDir(), []string{ok}, nil, nil, nil, env(nil), now); err != nil {
		t.Errorf("%s: %v", ok, err)
	}
}

func TestUnexpandableImageGivesOneProblem(t *testing.T) {
	_, err := Load(imageConfig(t, "app:${MISSING}"), env(nil), now)
	var probs Problems
	if !errors.As(err, &probs) {
		t.Fatalf("err = %v, want Problems", err)
	}
	if len(probs) != 1 || !strings.Contains(probs[0], "MISSING is not set") {
		t.Fatalf("problems = %q, want only the unset variable", probs)
	}
}

// YAML errors say what is wrong where, in the user's terms, and where to look (spec section 7).
func TestLoadExplainsYAMLErrors(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"unknown field":          {"version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\npolicy:\n  maxDatabaseAgeHours: 48\ncolour: red\n", `line 8: unknown field "colour" (check its spelling and indentation)`},
		"text for a number":      {"version: 1\npolicy:\n  maxExceptionDays: abc\n", `line 3: expected a whole number, found "abc"`},
		"text for the version":   {"version: abc\nproject: p\n", `line 1: version must be a whole number, found "abc"`},
		"list for text":          {"version: 1\nproject: [a, b]\n", "line 2: expected a text value, found a list"},
		"mapping for text":       {"version: 1\nproject: {a: b}\n", "line 2: expected a text value, found a mapping"},
		"text for a list":        {"version: 1\ntargets: oops\n", `line 2: expected a list, found "oops"`},
		"number for a mapping":   {"version: 1\npolicy: 3\n", `line 2: expected a mapping, found "3"`},
		"a list as the document": {"- a\n- b\n", "line 1: expected a mapping, found a list"},
		"text for true or false": {"version: 1\npolicy:\n  blockKnownExploited: maybe\n", `line 3: expected true or false, found "maybe"`},
		"text for a fraction":    {"version: 1\npolicy:\n  blockEpssAtLeast: lots\n", `line 3: expected a number, found "lots"`},
		"duplicate key":          {"version: 1\nversion: 2\n", `line 2: mapping key "version" already defined at line 1`},
		"bad indentation":        {"version: 1\nproject: p\n  bad: indent\n", "line 3: not valid YAML (mapping values are not allowed in this context)"},
		"unterminated string":    {"version: 1\nproject: \"oops\n", "line 2: not valid YAML (found unexpected end of stream)"},
		"tab":                    {"\tversion: 1\n", "not valid YAML (found character that cannot start any token)"},
		"empty":                  {"", "the file is empty"},
		"two documents":          {"version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\n---\nversion: 1\n", "it must contain a single YAML document"},
	} {
		t.Run(name, func(t *testing.T) {
			path := write(t, tc.content)
			_, err := Load(path, env(nil), now)
			if err == nil {
				t.Fatal("no error")
			}
			msg := err.Error()
			for _, want := range []string{"invalid configuration in " + path + ":", tc.want, "https://github.com/klar-web-services/shearbolt#readme"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error lacks %q:\n%s", want, msg)
				}
			}
			for _, leak := range []string{"config.", "[]string", "float64", " into ", "in type", "yaml: ", "unmarshal"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error leaks %q:\n%s", leak, msg)
				}
			}
		})
	}
}

// Several type errors are all listed, one per line.
func TestLoadListsEveryYAMLError(t *testing.T) {
	path := write(t, "version: 1\npolicy:\n  blockKnownExploited: maybe\n  blockEpssAtLeast: lots\n")
	_, err := Load(path, env(nil), now)
	if err == nil || !strings.Contains(err.Error(), "\n  - line 3: expected true or false") || !strings.Contains(err.Error(), "\n  - line 4: expected a number") {
		t.Fatalf("err = %v", err)
	}
}

// Validation problems name the file and point to the docs, like YAML errors.
func TestLoadValidationProblemsNameTheFile(t *testing.T) {
	path := write(t, "version: 2\nproject: p\n")
	_, err := Load(path, env(nil), now)
	if err == nil {
		t.Fatal("no error")
	}
	want := "invalid configuration in " + path + ":\n  - version must be 1, got 2\n  - at least one target is required\nSee https://github.com/klar-web-services/shearbolt#readme for the format of shearbolt.yaml."
	if err.Error() != want {
		t.Fatalf("error:\n%s\nwant:\n%s", err, want)
	}
}

// yaml.v3 alone would truncate 1.5 to 1 and accept it.
func TestLoadRejectsANonIntegerVersion(t *testing.T) {
	for _, v := range []string{"1.5", "0.9", "1.0", "'1'", "one", "[1]"} {
		t.Run(v, func(t *testing.T) {
			path := write(t, "version: "+v+"\nproject: p\ntargets:\n  - name: repo\n    source: .\n")
			_, err := Load(path, env(nil), now)
			if err == nil || !strings.Contains(err.Error(), "line 1: version must be a whole number") || !strings.Contains(err.Error(), "invalid configuration in "+path) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	path := write(t, "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\n")
	if cfg, err := Load(path, env(nil), now); err != nil || cfg.Version != 1 {
		t.Fatalf("version 1: cfg = %+v, err = %v", cfg, err)
	}
}

// yaml.v3 alone would truncate maxDatabaseAgeHours: 47.9 to 47 and accept it; both whole-number
// policy fields reject a non-integer like version does, also through an alias.
func TestLoadRejectsNonIntegerPolicyNumbers(t *testing.T) {
	const head = "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\npolicy:\n"
	for field, values := range map[string][]string{
		"maxDatabaseAgeHours": {"47.9", "48.0", "1e2"},
		"maxExceptionDays":    {"30.5", "1.0"},
	} {
		for _, v := range values {
			path := write(t, head+"  "+field+": "+v+"\n")
			_, err := Load(path, env(nil), now)
			want := "line 7: policy." + field + " must be a whole number, found " + strconv.Quote(v)
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "invalid configuration in "+path) {
				t.Errorf("%s: %s: err = %v", field, v, err)
			}
		}
	}
	// .inf does not fit an int at all: the decode reports it once.
	path := write(t, head+"  maxExceptionDays: .inf\n")
	if _, err := Load(path, env(nil), now); err == nil || !strings.Contains(err.Error(), `line 7: expected a whole number, found ".inf"`) || strings.Count(err.Error(), "line 7") != 1 {
		t.Errorf(".inf: err = %v", err)
	}
	// Reported with the other problems of the file, in one pass.
	path = write(t, "version: 1.5\nproject: p\ntargets:\n  - name: repo\n    source: .\npolicy:\n  maxExceptionDays: 30.5\n")
	if _, err := Load(path, env(nil), now); err == nil || !strings.Contains(err.Error(), "version must be a whole number") || !strings.Contains(err.Error(), "policy.maxExceptionDays must be a whole number") {
		t.Errorf("err = %v", err)
	}
	path = write(t, "version: 1\nproject: p\ntargets:\n  - name: repo\n    source: .\nhours: &h 47.9\npolicy:\n  maxDatabaseAgeHours: *h\n")
	if _, err := Load(path, env(nil), now); err == nil || !strings.Contains(err.Error(), "policy.maxDatabaseAgeHours must be a whole number") {
		t.Errorf("alias: err = %v", err)
	}
	path = write(t, head+"  maxDatabaseAgeHours: 24\n  maxExceptionDays: 30\n")
	if cfg, err := Load(path, env(nil), now); err != nil || cfg.Policy.MaxDatabaseAgeHours != 24 || cfg.Policy.MaxExceptionDays != 30 {
		t.Fatalf("whole numbers: cfg = %+v, err = %v", cfg, err)
	}
}
