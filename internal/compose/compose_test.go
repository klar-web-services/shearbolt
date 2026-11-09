package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testEnv(name string) (string, bool) {
	if name == "TAG" {
		return "1.2", true
	}
	return "", false
}

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func parse(t *testing.T, yml string, exclude ...string) *File {
	t.Helper()
	return Parse([]byte(yml), testEnv, exclude)
}

func wantImages(t *testing.T, f *File, want []Image) {
	t.Helper()
	if f.Fatal != nil {
		t.Fatalf("unexpected fatal problem: %+v", *f.Fatal)
	}
	if !reflect.DeepEqual(f.Images, want) {
		t.Fatalf("images = %+v, want %+v", f.Images, want)
	}
}

func TestParsePlain(t *testing.T) {
	f := parse(t, "services:\n  web:\n    image: nginx:1.27@"+digest+"\n  db:\n    image: \"postgres:16\"\n")
	wantImages(t, f, []Image{
		{Ref: "nginx:1.27@" + digest, Services: []string{"web"}, Line: 3},
		{Ref: "postgres:16", Services: []string{"db"}, Line: 5},
	})
	if !f.Pulls() {
		t.Error("Pulls() = false, want true")
	}
}

func TestParseDuplicates(t *testing.T) {
	f := parse(t, `services:
  proxy:
    image: minio/minio:1
  s3-down:
    image: minio/minio:1
`)
	wantImages(t, f, []Image{{Ref: "minio/minio:1", Services: []string{"proxy", "s3-down"}, Line: 3}})

	f = parse(t, `services:
  proxy:
    image: minio/minio:1
  s3-down:
    image: minio/minio:1
    platform: linux/arm64
`)
	wantImages(t, f, []Image{
		{Ref: "minio/minio:1", Services: []string{"proxy"}, Line: 3},
		{Ref: "minio/minio:1", Platform: "linux/arm64", Services: []string{"s3-down"}, Line: 5},
	})
}

func TestParseAnchorsAndMergeKeys(t *testing.T) {
	f := parse(t, `x-base: &base {image: nginx:1.27, restart: always}
services:
  web: {<<: *base}
  api:
    <<: *base
    image: "api:${TAG}"
`)
	wantImages(t, f, []Image{
		{Ref: "nginx:1.27", Services: []string{"web"}, Line: 3},
		{Ref: "api:1.2", Services: []string{"api"}, Line: 6},
	})

	t.Run("sequence merge takes a before b", func(t *testing.T) {
		f := parse(t, `x-a: &a {image: a:1}
x-b: &b {image: b:1, platform: linux/arm64}
services:
  web: {<<: [*a, *b]}
`)
		wantImages(t, f, []Image{{Ref: "a:1", Platform: "linux/arm64", Services: []string{"web"}, Line: 4}})
	})

	t.Run("alias as the image value", func(t *testing.T) {
		f := parse(t, `x-img: &img nginx:1.27
services:
  web:
    image: *img
`)
		wantImages(t, f, []Image{{Ref: "nginx:1.27", Services: []string{"web"}, Line: 3}})
	})
}

func TestParseServiceAlias(t *testing.T) {
	f := parse(t, `x-t: &t {image: nginx:1}
services:
  web: *t
`)
	wantImages(t, f, []Image{{Ref: "nginx:1", Services: []string{"web"}, Line: 3}})
}

func TestParseBuildServices(t *testing.T) {
	f := parse(t, `services:
  app: {build: ., image: ghcr.io/acme/app}
  cache: {build: {context: ./cache}}
`)
	wantImages(t, f, nil)
	if want := []string{"app", "cache"}; !reflect.DeepEqual(f.Builds, want) {
		t.Fatalf("Builds = %v, want %v", f.Builds, want)
	}
	if f.Pulls() {
		t.Error("Pulls() = true, want false")
	}
}

func TestParseNoImage(t *testing.T) {
	f := parse(t, "services:\n  web:\n    ports: [\"8080:80\"]\n")
	wantImages(t, f, nil)
	if want := []string{"web"}; !reflect.DeepEqual(f.NoImage, want) {
		t.Fatalf("NoImage = %v, want %v", f.NoImage, want)
	}
	if f.Pulls() {
		t.Error("Pulls() = true, want false")
	}
}

func TestParseProfilesIgnored(t *testing.T) {
	f := parse(t, "services:\n  debug:\n    image: busybox:1.36\n    profiles: [debug]\n")
	wantImages(t, f, []Image{{Ref: "busybox:1.36", Services: []string{"debug"}, Line: 3}})
}

func TestParseInclude(t *testing.T) {
	f := parse(t, "services:\n  web:\n    image: nginx:1\ninclude:\n  - other.yml\n")
	if f.Fatal == nil || f.Fatal.Code != "COMPOSE_UNSUPPORTED" || f.Fatal.Line != 4 {
		t.Fatalf("Fatal = %+v, want COMPOSE_UNSUPPORTED at line 4", f.Fatal)
	}
	if len(f.Images) != 0 {
		t.Errorf("images = %v, want none", f.Images)
	}
}

func TestParseExtends(t *testing.T) {
	yml := `services:
  base:
    image: nginx:1
  web:
    extends:
      service: base
  db:
    image: postgres:16
`
	f := parse(t, yml)
	if f.Fatal == nil || f.Fatal.Code != "COMPOSE_UNSUPPORTED" || f.Fatal.Line != 5 || !strings.Contains(f.Fatal.Message, "exclude: [web]") {
		t.Fatalf("Fatal = %+v, want COMPOSE_UNSUPPORTED at line 5 naming exclude: [web]", f.Fatal)
	}
	f = parse(t, yml, "web")
	wantImages(t, f, []Image{
		{Ref: "nginx:1", Services: []string{"base"}, Line: 3},
		{Ref: "postgres:16", Services: []string{"db"}, Line: 8},
	})
}

func TestParseExclude(t *testing.T) {
	yml := "services:\n  web:\n    image: nginx:1\n  db:\n    image: postgres:16\n"
	wantImages(t, parse(t, yml, "db"), []Image{{Ref: "nginx:1", Services: []string{"web"}, Line: 3}})
	f := parse(t, yml, "nope")
	if f.Fatal == nil || f.Fatal.Code != "COMPOSE_INVALID" || !strings.Contains(f.Fatal.Message, "nope") {
		t.Fatalf("Fatal = %+v, want COMPOSE_INVALID naming nope", f.Fatal)
	}
}

func wantVariableProblem(t *testing.T, f *File, service string, line int, vars ...string) Problem {
	t.Helper()
	if len(f.Missing) != 1 {
		t.Fatalf("Missing = %+v, want one problem", f.Missing)
	}
	p := f.Missing[0]
	if p.Code != "COMPOSE_VARIABLE" || p.Service != service || p.Line != line ||
		!strings.Contains(p.Message, "service "+service+" needs "+strings.Join(vars, ", ")) ||
		!strings.Contains(p.Message, "exclude: ["+service+"]") {
		t.Errorf("problem = %+v", p)
	}
	return p
}

func TestParseVariables(t *testing.T) {
	t.Run("missing in image", func(t *testing.T) {
		f := parse(t, "services:\n  app:\n    image: \"ghcr.io/acme/app:${GIT_SHA}\"\n  web:\n    image: nginx:1\n")
		p := wantVariableProblem(t, f, "app", 3, "GIT_SHA")
		if !strings.Contains(p.Message, "GIT_SHA, which is not set or empty. Set it where") {
			t.Errorf("message = %q", p.Message)
		}
		wantImages(t, f, []Image{{Ref: "nginx:1", Services: []string{"web"}, Line: 5}})
		if !f.Pulls() {
			t.Error("Pulls() = false, want true")
		}
	})
	t.Run("only missing", func(t *testing.T) {
		f := parse(t, "services:\n  app:\n    image: \"app:${GIT_SHA}\"\n")
		if len(f.Images) != 0 || !f.Pulls() {
			t.Fatalf("images = %v, Pulls() = %v, want none and true", f.Images, f.Pulls())
		}
	})
	t.Run("two variables", func(t *testing.T) {
		f := parse(t, "services:\n  app:\n    image: \"app:${A}-${B}\"\n")
		p := wantVariableProblem(t, f, "app", 3, "A", "B")
		if !strings.Contains(p.Message, "which are not set or empty. Set them where") {
			t.Errorf("message = %q", p.Message)
		}
	})
	t.Run("question mark message", func(t *testing.T) {
		f := parse(t, "services:\n  app:\n    image: \"app:${X:?set X}\"\n")
		p := wantVariableProblem(t, f, "app", 3, "X")
		if !strings.Contains(p.Message, "set X") {
			t.Errorf("message = %q, want it to hold %q", p.Message, "set X")
		}
	})
	t.Run("missing in platform", func(t *testing.T) {
		f := parse(t, "services:\n  app:\n    image: app:1\n    platform: ${PLAT}\n")
		wantImages(t, f, nil)
		wantVariableProblem(t, f, "app", 3, "PLAT")
	})
}

func TestParseInvalid(t *testing.T) {
	tests := map[string]string{
		"not yaml":            "services: [\n  a: }",
		"top-level sequence":  "- a\n- b\n",
		"services sequence":   "services: [a]\n",
		"service is a string": "services:\n  web: nginx\n",
		"image not a string":  "services:\n  web:\n    image: 1.5\n",
		"image with space":    "services:\n  web:\n    image: \"bad image\"\n",
		"image starts dash":   "services:\n  web:\n    image: \"-x\"\n",
		"platform uppercase":  "services:\n  web:\n    image: a:1\n    platform: Linux/AMD64\n",
		"nested variable":     "services:\n  web:\n    image: \"app:${A:-${B}}\"\n",
	}
	for name, yml := range tests {
		t.Run(name, func(t *testing.T) {
			f := parse(t, yml)
			if f.Fatal == nil || f.Fatal.Code != "COMPOSE_INVALID" {
				t.Fatalf("Fatal = %+v, want COMPOSE_INVALID", f.Fatal)
			}
			if len(f.Images) != 0 || f.Pulls() {
				t.Errorf("a fatal file must be empty: %+v", f)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	for name, yml := range map[string]string{"empty file": "", "services without value": "services:\n"} {
		t.Run(name, func(t *testing.T) {
			f := parse(t, yml)
			if !reflect.DeepEqual(f, &File{}) {
				t.Fatalf("file = %+v, want empty", f)
			}
			if f.Pulls() {
				t.Error("Pulls() = true, want false")
			}
		})
	}
}

func TestFileName(t *testing.T) {
	for _, n := range []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml", "docker-compose.prod.yml", "compose.override.yaml", "docker-compose-prod.yml", "compose-dev.yaml"} {
		if !FileName(n) {
			t.Errorf("FileName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"compose.yml.bak", "my-compose.yml", "docker-compose.json", "stack.yml", "docker-composer.yml"} {
		if FileName(n) {
			t.Errorf("FileName(%q) = true, want false", n)
		}
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	if f, err := Read(filepath.Join(dir, "missing.yml"), testEnv, nil); err == nil || f != nil {
		t.Fatalf("Read(missing) = %v, %v, want nil and an error", f, err)
	}
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, []byte("services:\n  web:\n    image: nginx:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Read(path, testEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantImages(t, f, []Image{{Ref: "nginx:1", Services: []string{"web"}, Line: 3}})
}

func TestParseMultipleDocuments(t *testing.T) {
	f := parse(t, "services:\n  web:\n    image: nginx:1\n---\nservices:\n  db:\n    image: postgres:16\n")
	if f.Fatal == nil || f.Fatal.Code != "COMPOSE_UNSUPPORTED" || !strings.Contains(f.Fatal.Message, "more than one YAML document") {
		t.Fatalf("Fatal = %+v, want COMPOSE_UNSUPPORTED about several documents", f.Fatal)
	}
	if len(f.Images) != 0 {
		t.Errorf("images = %v, want none", f.Images)
	}
	for name, yml := range map[string]string{"comment only": "# nothing here\n", "document marker": "---\nservices:\n  web:\n    image: nginx:1\n"} {
		f := parse(t, yml)
		if f.Fatal != nil {
			t.Errorf("%s: unexpected fatal problem: %+v", name, *f.Fatal)
		}
	}
	if f := parse(t, "# nothing here\n"); !reflect.DeepEqual(f, &File{}) {
		t.Errorf("comment-only file = %+v, want empty", f)
	}
}

func TestParseProblemMessagesCarryNoLine(t *testing.T) {
	t.Run("yaml syntax", func(t *testing.T) {
		f := parse(t, "services:\n  web: [\n    a: }")
		if f.Fatal == nil || f.Fatal.Code != "COMPOSE_INVALID" || f.Fatal.Line == 0 {
			t.Fatalf("Fatal = %+v, want COMPOSE_INVALID with a line", f.Fatal)
		}
		if strings.Contains(f.Fatal.Message, "line ") || strings.Contains(f.Fatal.Message, "yaml: ") {
			t.Errorf("message = %q, want no line prefix", f.Fatal.Message)
		}
	})
	t.Run("merge key", func(t *testing.T) {
		f := parse(t, "services:\n  web:\n    <<: 5\n    image: a:1\n")
		if f.Fatal == nil || f.Fatal.Code != "COMPOSE_INVALID" || f.Fatal.Line != 3 {
			t.Fatalf("Fatal = %+v, want COMPOSE_INVALID at line 3", f.Fatal)
		}
		if strings.Contains(f.Fatal.Message, "line ") || !strings.HasPrefix(f.Fatal.Message, "service web: ") {
			t.Errorf("message = %q", f.Fatal.Message)
		}
	})
}

func TestParsePlatformLines(t *testing.T) {
	tests := map[string]string{
		"invalid":    "services:\n  web:\n    image: a:1\n    platform: Linux/AMD64\n",
		"not string": "services:\n  web:\n    image: a:1\n    platform: [x]\n",
		"nested":     "services:\n  web:\n    image: a:1\n    platform: \"${A:-${B}}\"\n",
	}
	for name, yml := range tests {
		t.Run(name, func(t *testing.T) {
			f := parse(t, yml)
			if f.Fatal == nil || f.Fatal.Code != "COMPOSE_INVALID" || f.Fatal.Line != 4 {
				t.Fatalf("Fatal = %+v, want COMPOSE_INVALID at line 4", f.Fatal)
			}
		})
	}
}
