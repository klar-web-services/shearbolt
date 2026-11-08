package github_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/ci/github"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"go.yaml.in/yaml/v3"
)

const wf = ".github/workflows/ci.yml"

// tree writes files under a new temp dir. A path ending in "*" is made executable.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, "*") {
			p, mode = strings.TrimSuffix(p, "*"), 0o755
		}
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// npmRepo is an npm project named name with a Dockerfile, plus the workflow and extra files.
func npmRepo(name, workflow string, extra map[string]string) map[string]string {
	files := map[string]string{
		"package.json":      `{"name":"` + name + `"}`,
		"package-lock.json": "{}",
		"Dockerfile":        "FROM node:22\n",
		wf:                  workflow,
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

// result runs setup on files and returns the plan and the workflow afterwards ("" when it is
// unchanged). It also checks that the result is valid YAML, and that a second run on the written
// result changes nothing.
func result(t *testing.T, files map[string]string) (*plan.Plan, string) {
	t.Helper()
	root := tree(t, files)
	p := setup.Compute(setup.Options{Root: root, Yes: true, Version: "1.2.3"})
	if p.Error != nil {
		t.Fatalf("plan error: %+v", p.Error)
	}
	out := ""
	for _, c := range p.Changes {
		if c.Path == wf {
			out = c.Content
			var v any
			if err := yaml.Unmarshal([]byte(c.Content), &v); err != nil {
				t.Fatalf("the edited workflow is not valid YAML: %v\n%s", err, c.Content)
			}
		}
	}
	if err := setup.Write(root, p); err != nil {
		t.Fatal(err)
	}
	again := setup.Compute(setup.Options{Root: root, Yes: true, Version: "1.2.3"})
	if again.Error != nil {
		t.Fatalf("second plan error: %+v", again.Error)
	}
	for _, c := range again.Changes {
		t.Errorf("a second run changes %s:\n%s", c.Path, c.Diff)
	}
	return p, out
}

func hasNote(p *plan.Plan, code string) bool {
	return slices.ContainsFunc(p.Notes, func(n plan.Note) bool { return n.Code == code })
}

func noteCodes(p *plan.Plan) []string {
	var out []string
	for _, n := range p.Notes {
		out = append(out, n.Code)
	}
	return out
}

func lines(s ...string) string { return strings.Join(s, "\n") + "\n" }

func wantWorkflow(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("workflow:\n%s\nwant:\n%s", got, want)
	}
}

const head = "name: ci\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v5\n"

func TestGitHubFiles(t *testing.T) {
	root := tree(t, map[string]string{
		".github/workflows/b.yaml":      "on: push\n",
		".github/workflows/a.yml":       "on: push\n",
		".github/workflows/notes.md":    "x\n",
		".github/workflows/dir.yml/x":   "x\n",
		".github/workflows/sub/c.yml":   "on: push\n",
		".github/dependabot.yml":        "version: 2\n",
		".github/workflows/z.yml.orig":  "x\n",
		".github/workflows/upper.YML":   "on: push\n",
		".github/workflows/.hidden.yml": "on: push\n",
	})
	got := github.Plugin{}.Files(plan.Repo{Root: root})
	want := []string{".github/workflows/.hidden.yml", ".github/workflows/a.yml", ".github/workflows/b.yaml"}
	if !slices.Equal(got, want) {
		t.Fatalf("Files = %q, want %q", got, want)
	}
}

func TestGitHubGatePlacementAndID(t *testing.T) {
	p, got := result(t, npmRepo("web", head+lines(
		"      - name: Build",
		"        run: docker build -t ghcr.io/acme/web:latest .",
		"      - name: Push",
		"        run: docker push ghcr.io/acme/web:latest",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Build",
		"        id: shearbolt-build",
		"        run: |",
		"          docker build -t ghcr.io/acme/web:latest -t ghcr.io/acme/web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=ghcr.io/acme/web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Push",
		"        run: docker push ghcr.io/acme/web:latest",
	))
	if !hasNote(p, "TAG_ADDED") {
		t.Errorf("notes = %v, want TAG_ADDED", noteCodes(p))
	}
}

func TestGitHubExistingIDAndFirstKey(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - name: Build",
		"        id: img",
		"        run: docker build -t web:1 .",
		"      - run: docker build -f Dockerfile -t web-2:1 .  # second image",
		"      - run: docker push web:1 && docker push web-2:1",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Build",
		"        id: img",
		"        run: |",
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - id: shearbolt-build",
		"        run: |  # second image",
		"          docker build -f Dockerfile -t web-2:1 -t web-2:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web-2:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.img.outputs.shearbolt-image }}",
		"          SHEARBOLT_IMAGE_WEB_2: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - run: docker push web:1 && docker push web-2:1",
	))
}

func TestGitHubBuildxGetsLoad(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: |",
		"          docker buildx build -t web:1 .",
		"          docker buildx build --load -t worker:2 -f Dockerfile .",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - id: shearbolt-build",
		"        run: |",
		"          docker buildx build --load -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"          docker buildx build --load -t worker:2 -t worker:shearbolt-${{ github.sha }} -f Dockerfile .",
		`          echo shearbolt-image-2=worker:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"          SHEARBOLT_IMAGE_WORKER: ${{ steps.shearbolt-build.outputs.shearbolt-image-2 }}",
	))
}

func TestGitHubSplitCombinedStep(t *testing.T) {
	p, got := result(t, npmRepo("web", head+lines(
		"      - name: Ship",
		"        env:",
		"          REGISTRY: ghcr.io",
		"        run: |",
		"          set -eu",
		"          cd .",
		"          IMAGE=$REGISTRY/acme/web",
		`          docker build -t "$IMAGE:1" . && docker push "$IMAGE:1"`,
		"          ./notify.sh",
	), map[string]string{"notify.sh*": "#!/bin/sh\necho done\n"}))
	wantWorkflow(t, got, head+lines(
		"      - name: Ship",
		"        id: shearbolt-build",
		"        env:",
		"          REGISTRY: ghcr.io",
		"        run: |",
		"          set -eu",
		"          cd .",
		"          IMAGE=$REGISTRY/acme/web",
		`          docker build -t "$IMAGE:1" -t "$IMAGE:shearbolt-${{ github.sha }}" . && echo shearbolt-image="$IMAGE:shearbolt-${{ github.sha }}" >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Ship (continued)",
		"        env:",
		"          REGISTRY: ghcr.io",
		"        run: |",
		"          set -eu",
		"          cd .",
		"          IMAGE=$REGISTRY/acme/web",
		`          docker push "$IMAGE:1"`,
		"          ./notify.sh",
	))
	if hasNote(p, "UNSUPPORTED_EDIT") {
		t.Errorf("notes = %v", noteCodes(p))
	}
}

// P1: any command after the last gated build, recognized push or not, moves after the gate.
func TestGitHubSplitUnrecognizedDeploy(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: npm ci && npm run build && npx vercel deploy --prebuilt",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - run: npm ci && npm run build",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"      - name: Continue after shearbolt",
		"        run: npx vercel deploy --prebuilt",
	))
}

func TestGitHubPushBuild(t *testing.T) {
	p, got := result(t, npmRepo("api", head+lines(
		"      - name: Image",
		"        run: docker buildx build --push -t ghcr.io/acme/api:latest .",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Image",
		"        id: shearbolt-build",
		"        run: |",
		"          docker buildx build --load -t ghcr.io/acme/api:latest -t ghcr.io/acme/api:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=ghcr.io/acme/api:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Image (continued)",
		"        run: docker push ghcr.io/acme/api:latest",
	))
	if !hasNote(p, "ATTESTATIONS_DROPPED") {
		t.Errorf("notes = %v, want ATTESTATIONS_DROPPED", noteCodes(p))
	}
}

// The remainder of every --push step goes after the gate, even when the gate follows a later build.
func TestGitHubPushBuildBeforeLaterBuild(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: docker buildx build --push -t web:1 -t web:latest .",
		"      - run: docker build -f Dockerfile -t worker:1 .",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - id: shearbolt-build",
		"        run: |",
		"          docker buildx build --load -t web:1 -t web:shearbolt-${{ github.sha }} -t web:latest .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - id: shearbolt-build-2",
		"        run: |",
		"          docker build -f Dockerfile -t worker:1 -t worker:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=worker:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"          SHEARBOLT_IMAGE_WORKER: ${{ steps.shearbolt-build-2.outputs.shearbolt-image }}",
		"      - name: Continue after shearbolt",
		"        run: |",
		"          docker push web:1",
		"          docker push web:latest",
	))
}

func unsupported(t *testing.T, files map[string]string, why string) *plan.Plan {
	t.Helper()
	p, got := result(t, files)
	if got != "" {
		t.Errorf("%s: the workflow changed:\n%s", why, got)
	}
	i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" && n.File == wf })
	if i < 0 {
		t.Fatalf("%s: notes = %v, want UNSUPPORTED_EDIT", why, noteCodes(p))
	}
	if !strings.Contains(p.Notes[i].Message, "uses: klar-web-services/shearbolt@v1") {
		t.Errorf("%s: UNSUPPORTED_EDIT has no snippet: %s", why, p.Notes[i].Message)
	}
	if hasNote(p, "TAG_ADDED") || hasNote(p, "ATTESTATIONS_DROPPED") {
		t.Errorf("%s: notes about edits that did not land: %v", why, noteCodes(p))
	}
	return p
}

func TestGitHubUnsplittableSteps(t *testing.T) {
	cases := map[string]string{
		"piped --push": "docker buildx build --push -t web:1 . | tee build.log",
		"(a) id and GITHUB_OUTPUT in the remainder": "", // below
		"(b) conditional build":                     "if [ -n \"$X\" ]; then\n  docker build -t web:1 .\nfi\ndocker push web:1",
		"(b) continuation":                          "docker build -t web:1 . && \\\n  docker push web:1",
		"(c) ||":                                    "docker build -t web:1 . || exit 1\ndocker push web:1",
		"(d) source":                                "source ./env.sh\ndocker build -t web:1 .\ndocker push web:1",
		"(d) function":                              "push() { docker push web:1; }\ndocker build -t web:1 .\npush",
		"(d) command substitution":                  "TAG=$(git rev-parse HEAD)\ndocker build -t web:1 .\ndocker push web:$TAG",
		"heredoc":                                   "docker build -t web:1 - <<EOF\nFROM node:22\nEOF\ndocker push web:1",
	}
	for why, script := range cases {
		step := "      - name: Build\n        run: |\n" + indent(script, "          ")
		if why == "(a) id and GITHUB_OUTPUT in the remainder" {
			step = "      - name: Build\n        id: build\n        run: |\n" + indent("docker build -t web:1 .\necho \"digest=abc\" >> \"$GITHUB_OUTPUT\"", "          ")
		}
		p := unsupported(t, npmRepo("web", head+step, map[string]string{"env.sh": "export X=1\n"}), why)
		i := slices.IndexFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" })
		if why == "piped --push" && !strings.Contains(p.Notes[i].Message, "|") {
			t.Errorf("%s: the note does not say why: %s", why, p.Notes[i].Message)
		}
		if why == "piped --push" && !strings.Contains(p.Notes[i].Message, "docker buildx build --push") {
			t.Errorf("%s: the note does not name the command", why)
		}
	}
}

func indent(s, pad string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		b.WriteString(pad + l + "\n")
	}
	return b.String()
}

// Only edits that need a POSIX shell are refused in other shells; the Rust rewrite works anywhere.
func TestGitHubNonPOSIXShell(t *testing.T) {
	unsupported(t, npmRepo("web", head+lines(
		"      - shell: pwsh",
		"        run: docker build -t web:1 .",
	), nil), "pwsh step")
	unsupported(t, npmRepo("web", strings.Replace(head, "ubuntu-latest", "windows-latest", 1)+lines(
		"      - run: docker build -t web:1 .",
	), nil), "windows runner")
	unsupported(t, npmRepo("web", strings.Replace(head, "ubuntu-latest", "${{ matrix.os }}\n    strategy:\n      matrix:\n        os: [ubuntu-latest, windows-latest]", 1)+lines(
		"      - run: docker build -t web:1 .",
	), nil), "matrix with windows")
	unsupported(t, npmRepo("web", strings.Replace(head, "ubuntu-latest", "${{ matrix.os }}\n    strategy:\n      matrix:\n        os: [ubuntu-latest]\n        include:\n          - os: windows-latest", 1)+lines(
		"      - run: docker build -t web:1 .",
	), nil), "matrix include with windows")
	unsupported(t, npmRepo("web", strings.Replace(head, "ubuntu-latest", "${{ vars.RUNNER }}", 1)+lines(
		"      - run: docker build -t web:1 .",
	), nil), "unknown runner")
	_, got := result(t, npmRepo("web", strings.Replace(head, "ubuntu-latest", "${{ matrix.os }}\n    strategy:\n      matrix:\n        os: [ubuntu-latest, macos-latest]", 1)+lines(
		"      - run: docker build -t web:1 .",
	), nil))
	if !strings.Contains(got, "echo shearbolt-image=") {
		t.Errorf("a matrix of POSIX runners is edited:\n%s", got)
	}
}

func TestGitHubIfWrapping(t *testing.T) {
	p, got := result(t, npmRepo("web", head+lines(
		"      - name: Build",
		"        if: always()",
		"        run: docker build -t web:1 . && docker push web:1",
		"      - name: Release",
		"        if: ${{ !cancelled() }}",
		"        run: gh release create v1",
		"      - name: Report",
		"        if: always()",
		"        run: echo done",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Build",
		"        id: shearbolt-build",
		"        if: always()",
		`        run: docker build -t web:1 -t web:shearbolt-${{ github.sha }} . && echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        if: always()",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Build (continued)",
		"        if: ${{ success() && (always()) }}",
		"        run: docker push web:1",
		"      - name: Release",
		"        if: ${{ success() && (!cancelled()) }}",
		"        run: gh release create v1",
		"      - name: Report",
		"        if: always()",
		"        run: echo done",
	))
	if !hasNote(p, "PUSH_IF_WRAPPED") {
		t.Errorf("notes = %v, want PUSH_IF_WRAPPED for the rewritten push step", noteCodes(p))
	}
}

func TestGitHubRust(t *testing.T) {
	files := map[string]string{
		"Cargo.toml":  "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"Cargo.lock":  "version = 3\n",
		"src/main.rs": "fn main() {}\n",
		wf: head + lines(
			"      # Build the release binary",
			"      - run: cargo build --release --locked && ./upload.sh",
		),
		"upload.sh*": "#!/bin/sh\necho upload\n",
	}
	_, got := result(t, files)
	wantWorkflow(t, got, head+lines(
		"      - name: Install cargo-auditable",
		"        uses: taiki-e/install-action@v2",
		"        with:",
		"          tool: cargo-auditable",
		"      # Build the release binary",
		"      - run: cargo auditable build --release --locked",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"      - name: Continue after shearbolt",
		"        run: ./upload.sh",
	))

	// A release build after a push is rewritten and gets the install, though it is not gated.
	files[wf] = head + lines(
		"      - run: cargo build --release",
		"      - run: gh release upload v1 target/release/demo",
		"      - run: cargo build --release --target x86_64-unknown-linux-musl",
	)
	p, got := result(t, files)
	wantWorkflow(t, got, head+lines(
		"      - name: Install cargo-auditable",
		"        uses: taiki-e/install-action@v2",
		"        with:",
		"          tool: cargo-auditable",
		"      - run: cargo auditable build --release",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"      - run: gh release upload v1 target/release/demo",
		"      - run: cargo auditable build --release --target x86_64-unknown-linux-musl",
	))
	if !hasNote(p, "BUILD_AFTER_PUSH") {
		t.Errorf("notes = %v", noteCodes(p))
	}

	// An install already in the job is not added again.
	files[wf] = head + lines(
		"      - run: cargo install cargo-auditable --locked",
		"      - run: cargo build --release",
	)
	_, got = result(t, files)
	if strings.Count(got, "cargo-auditable") != 1 || !strings.Contains(got, "cargo auditable build --release") {
		t.Errorf("workflow:\n%s", got)
	}
}

func TestGitHubConditionalRustBuild(t *testing.T) {
	files := map[string]string{
		"Cargo.toml":  "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"Cargo.lock":  "version = 3\n",
		"src/main.rs": "fn main() {}\n",
		wf: head + lines(
			"      - run: |",
			`          if [ "$RELEASE" = 1 ]; then`,
			"            cargo build --release",
			"          fi",
		),
	}
	p, got := result(t, files)
	// A closer after the build is not "a command that follows", so no split is needed.
	wantWorkflow(t, got, head+lines(
		"      - name: Install cargo-auditable",
		"        uses: taiki-e/install-action@v2",
		"        with:",
		"          tool: cargo-auditable",
		"      - run: |",
		`          if [ "$RELEASE" = 1 ]; then`,
		"            cargo auditable build --release",
		"          fi",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
	))
	if !hasNote(p, "RUST_CONDITIONAL_BUILD") {
		t.Errorf("notes = %v", noteCodes(p))
	}
}

func TestGitHubMultiPlatformGateBefore(t *testing.T) {
	p, got := result(t, npmRepo("web", head+lines(
		"      # both platforms",
		"      - run: docker buildx build --platform linux/amd64,linux/arm64 --push -t web:1 .",
		"        if: always()",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"      # both platforms",
		"      - run: docker buildx build --platform linux/amd64,linux/arm64 --push -t web:1 .",
		"        if: ${{ success() && (always()) }}",
	))
	if !hasNote(p, "MULTI_PLATFORM") {
		t.Errorf("notes = %v", noteCodes(p))
	}
}

const gated = "      - name: Build\n" +
	"        id: shearbolt-build\n" +
	"        run: |\n" +
	"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .\n" +
	"          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> \"$GITHUB_OUTPUT\"\n"

func TestGitHubExistingGates(t *testing.T) {
	// A stale image variable is replaced; nothing else changes.
	_, got := result(t, npmRepo("web", head+gated+lines(
		"      - name: shearbolt  # keep",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: web:stale",
		"          OTHER: x",
		"          SHEARBOLT_IMAGE_OLD: old",
		"      - run: docker push web:1",
	), nil))
	wantWorkflow(t, got, head+gated+lines(
		"      - name: shearbolt  # keep",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"          OTHER: x",
		"      - run: docker push web:1",
	))

	// A stale targets input is removed, and the image variable is added.
	_, got = result(t, npmRepo("web", head+gated+lines(
		"      - uses: klar-web-services/shearbolt@v1",
		"        with:",
		"          targets: web",
		"      - run: docker push web:1",
	), nil))
	wantWorkflow(t, got, head+gated+lines(
		"      - uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - run: docker push web:1",
	))
}

func TestGitHubExistingGateWithConfig(t *testing.T) {
	files := map[string]string{
		"apps/web/package.json":      `{"name":"web"}`,
		"apps/web/package-lock.json": "{}",
		"apps/web/Dockerfile":        "FROM node:22\n",
		wf: head + lines(
			"      - run: docker build -t web:1 apps/web",
			"      - uses: klar-web-services/shearbolt@v1",
			"        with:",
			"          working-directory: apps",
			"          config: web/shearbolt.yaml",
			"      - run: docker push web:1",
		),
	}
	_, got := result(t, files)
	wantWorkflow(t, got, head+lines(
		"      - id: shearbolt-build",
		"        run: |",
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} apps/web",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"        with:",
		"          working-directory: apps",
		"          config: web/shearbolt.yaml",
		"      - run: docker push web:1",
	))
}

func TestGitHubJobsLeftOut(t *testing.T) {
	src := lines(
		"on: push",
		"jobs:",
		"  call:",
		"    uses: ./.github/workflows/reusable.yml",
		"  build: &build",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: npm run build",
		"  again: *build",
		"  merged:",
		"    <<: *build",
		"  flow:",
		"    runs-on: ubuntu-latest",
		"    steps: [{run: npm run build}]",
		"  ok:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: npm run build",
		"        working-directory: ${{ matrix.dir }}",
	)
	classify := func(cmd shell.Command, ctx plan.Context) plan.Classified {
		if ctx.Dir != "" {
			t.Errorf("an expression working directory gives an unknown directory, not %q", ctx.Dir)
		}
		return plan.Classified{}
	}
	jobs, notes, err := github.Plugin{}.Jobs(plan.Repo{Root: t.TempDir()}, wf, []byte(src), classify)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != wf+"#ok" || jobs[0].Label != "ci.yml: ok" {
		t.Fatalf("jobs = %+v, want only ok", jobs)
	}
	if len(notes) != 1 || notes[0].Code != "UNSUPPORTED_EDIT" {
		t.Fatalf("notes = %+v, want one UNSUPPORTED_EDIT", notes)
	}
	for _, k := range []string{"build", "again", "merged", "flow", "klar-web-services/shearbolt@v1"} {
		if !strings.Contains(notes[0].Message, k) {
			t.Errorf("the note does not mention %s: %s", k, notes[0].Message)
		}
	}
	if strings.Contains(notes[0].Message, "call (") {
		t.Errorf("a reusable workflow call is not listed: %s", notes[0].Message)
	}

	if _, _, err := (github.Plugin{}).Jobs(plan.Repo{Root: t.TempDir()}, wf, []byte("jobs:\n  a: [\n"), classify); err == nil {
		t.Error("invalid YAML is an error")
	}
}

func change(p *plan.Plan, path string) *plan.FileChange {
	for i := range p.Changes {
		if p.Changes[i].Path == path {
			return &p.Changes[i]
		}
	}
	return nil
}

const demoMake = "release:\n\tcargo build --release --locked\n"

func rustFiles(workflow string) map[string]string {
	return map[string]string{
		"Cargo.toml":  "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"Cargo.lock":  "version = 3\n",
		"src/main.rs": "fn main() {}\n",
		"Makefile":    demoMake,
		"env.sh":      "export X=1\n",
		"deploy.sh*":  "#!/bin/sh\necho deploy\n",
		wf:            workflow,
	}
}

// A job that cannot be edited is left completely unchanged: no gate, no install, and the
// Makefile its build reaches keeps its plain cargo build. Another job is still edited.
func TestGitHubFailedJobIsAtomic(t *testing.T) {
	src := lines(
		"on: push",
		"jobs:",
		"  bad:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: |",
		"          source ./env.sh",
		"          make release",
		"          ./deploy.sh",
		"  good:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: cargo build --release",
	)
	p, got := result(t, rustFiles(src))
	if change(p, "Makefile") != nil {
		t.Errorf("the Makefile is rewritten although the job that runs it failed")
	}
	if !slices.Contains(noteCodes(p), "UNSUPPORTED_EDIT") {
		t.Fatalf("notes = %v", noteCodes(p))
	}
	want := lines(
		"on: push",
		"jobs:",
		"  bad:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: |",
		"          source ./env.sh",
		"          make release",
		"          ./deploy.sh",
		"  good:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - name: Install cargo-auditable",
		"        uses: taiki-e/install-action@v2",
		"        with:",
		"          tool: cargo-auditable",
		"      - run: cargo auditable build --release",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
	)
	wantWorkflow(t, got, want)
}

// Rule 3 can point at a command in the middle of a step; the gate then goes before the step.
func TestGitHubBeforeGateMidStep(t *testing.T) {
	files := rustFiles(head + lines(
		"      - run: echo start && make ship",
	))
	files["Makefile"] = "ship:\n\tcargo build --release --locked\n\tgh release upload v1 target/release/demo\n"
	p, got := result(t, files)
	wantWorkflow(t, got, head+lines(
		"      - name: Install cargo-auditable",
		"        uses: taiki-e/install-action@v2",
		"        with:",
		"          tool: cargo-auditable",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"      - run: echo start && make ship",
	))
	if !hasNote(p, "BUILD_AFTER_PUSH") || change(p, "Makefile") == nil {
		t.Errorf("notes = %v, Makefile changed = %v", noteCodes(p), change(p, "Makefile") != nil)
	}
}

// An existing gate is not moved; the push of a --push build goes after it.
func TestGitHubExistingGateAfterPushBuild(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - name: Image",
		"        run: docker buildx build --push -t web:1 .",
		"      - uses: klar-web-services/shearbolt@v1",
		"      - run: echo after",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: Image",
		"        id: shearbolt-build",
		"        run: |",
		"          docker buildx build --load -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Image (continued)",
		"        run: docker push web:1",
		"      - run: echo after",
	))
}

// A shearbolt scan in a run step is an existing gate; the image variable goes in its step env.
func TestGitHubExistingRunStepGate(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 .",
		"      - run: |",
		"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
		`          "$HOME/.local/bin/shearbolt" scan --ci`,
		"      - run: docker push web:1",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - id: shearbolt-build",
		"        run: |",
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - run: |",
		"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
		`          "$HOME/.local/bin/shearbolt" scan --ci`,
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - run: docker push web:1",
	))
}

// A project in a subdirectory gets with.working-directory; a project without a build is checked
// at the start of the gate-job default (the first job).
func TestGitHubMonorepoGate(t *testing.T) {
	files := map[string]string{
		"apps/web/package.json":      `{"name":"web"}`,
		"apps/web/package-lock.json": "{}",
		"apps/web/Dockerfile":        "FROM node:22\n",
		"crates/cli/Cargo.toml":      "[package]\nname = \"cli\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"crates/cli/Cargo.lock":      "version = 3\n",
		"crates/cli/src/main.rs":     "fn main() {}\n",
		wf: lines(
			"on: push",
			"jobs:",
			"  web:",
			"    runs-on: ubuntu-latest",
			"    defaults:",
			"      run:",
			"        working-directory: apps/web",
			"    steps:",
			"      - uses: actions/checkout@v5",
			"      - run: docker build -t ghcr.io/acme/web:1 .",
			"      - run: docker push ghcr.io/acme/web:1",
		),
	}
	_, got := result(t, files)
	wantWorkflow(t, got, lines(
		"on: push",
		"jobs:",
		"  web:",
		"    runs-on: ubuntu-latest",
		"    defaults:",
		"      run:",
		"        working-directory: apps/web",
		"    steps:",
		"      - uses: actions/checkout@v5",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        with:",
		"          working-directory: crates/cli",
		"      - id: shearbolt-build",
		"        run: |",
		"          docker build -t ghcr.io/acme/web:1 -t ghcr.io/acme/web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=ghcr.io/acme/web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"        with:",
		"          working-directory: apps/web",
		"      - run: docker push ghcr.io/acme/web:1",
	))
}

func TestGitHubCRLFAndCopiedKeys(t *testing.T) {
	src := lines(
		"on: push",
		"jobs:",
		"  build:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"    - run: |  # build and ship",
		"        docker build -t web:1 .",
		"        docker push web:1",
		"      shell: bash",
		"      working-directory: .",
		"      timeout-minutes: 10",
		"      continue-on-error: false",
		"      env:",
		"        A: 1  # keep",
	)
	_, got := result(t, npmRepo("web", strings.ReplaceAll(src, "\n", "\r\n"), nil))
	want := lines(
		"on: push",
		"jobs:",
		"  build:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"    - id: shearbolt-build",
		"      run: |  # build and ship",
		"        docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`        echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      shell: bash",
		"      working-directory: .",
		"      timeout-minutes: 10",
		"      continue-on-error: false",
		"      env:",
		"        A: 1  # keep",
		"    - name: shearbolt",
		"      uses: klar-web-services/shearbolt@v1",
		"      env:",
		"        SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"    - name: Continue after shearbolt",
		"      shell: bash",
		"      working-directory: .",
		"      timeout-minutes: 10",
		"      continue-on-error: false",
		"      env:",
		"        A: 1  # keep",
		"      run: |",
		"        docker push web:1",
	)
	wantWorkflow(t, got, strings.ReplaceAll(want, "\n", "\r\n"))
}

// Two projects building in one step would need two splits: the job is left alone.
func TestGitHubTwoProjectsInOneStep(t *testing.T) {
	files := map[string]string{
		"a/package.json": `{"name":"a"}`, "a/package-lock.json": "{}", "a/Dockerfile": "FROM x\n",
		"b/package.json": `{"name":"b"}`, "b/package-lock.json": "{}", "b/Dockerfile": "FROM x\n",
		wf: head + lines("      - run: docker build -t a:1 a && docker build -t b:1 b && docker push a:1 b:1"),
	}
	p, got := result(t, files)
	if got != "" || !hasNote(p, "UNSUPPORTED_EDIT") {
		t.Errorf("notes = %v, workflow:\n%s", noteCodes(p), got)
	}
}

// An existing gate that must now list its targets gets them.
func TestGitHubExistingGateGetsTargets(t *testing.T) {
	src := lines(
		"on: push",
		"jobs:",
		"  image:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: docker build -t web:1 .",
		"      - run: docker push web:1",
		"  publish:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: npm run build",
		"      - uses: klar-web-services/shearbolt@v1",
		"      - run: npm publish",
	)
	_, got := result(t, npmRepo("web", src, nil))
	wantWorkflow(t, got, lines(
		"on: push",
		"jobs:",
		"  image:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - id: shearbolt-build",
		"        run: |",
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - run: docker push web:1",
		"  publish:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: npm run build",
		"      - uses: klar-web-services/shearbolt@v1",
		"        with:",
		"          targets: source",
		"      - run: npm publish",
	))
}

func TestGitHubActionSteps(t *testing.T) {
	src := lines(
		"on: push",
		"jobs:",
		"  a:",
		"    runs-on: [self-hosted, linux]",
		"    strategy:",
		"      matrix:",
		"        include:",
		"          - os: windows-2022",
		"    steps:",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: ${{ github.ref == 'refs/heads/main' }}",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: false",
		"      - uses: SoftProps/Action-GH-Release@v2",
		"      - uses: klar-web-services/shearbolt@v1",
		"        with:",
		"          working-directory: ./apps/",
		"      - uses: klar-web-services/shearbolt@v1",
		"        with:",
		"          working-directory: ${{ matrix.dir }}",
		"      - shell: bash",
		"        run: echo hi",
	)
	jobs, _, err := github.Plugin{}.Jobs(plan.Repo{Root: t.TempDir()}, wf, []byte(src), func(shell.Command, plan.Context) plan.Classified { return plan.Classified{} })
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	st := jobs[0].Steps
	// build-push-action is an image build whose push comes from Image.Push, never Step.Push.
	imagePush := func(i int) bool {
		return len(st[i].Builds) == 1 && st[i].Builds[0].Image != nil && st[i].Builds[0].Image.Push
	}
	if !imagePush(0) || imagePush(1) || len(st[0].Push)+len(st[1].Push) != 0 || !slices.Equal(st[2].Push, []int{-1}) {
		t.Errorf("pushes: %+v %+v %v", st[0], st[1], st[2].Push)
	}
	if len(st[3].Gates) != 1 || st[3].Gates[0].Project != "apps" || st[3].Gates[0].Pos.Command != -1 || st[3].Gates[0].Pos.Line != 17 {
		t.Errorf("gate: %+v", st[3].Gates)
	}
	if len(st[4].Gates) != 1 || st[4].Gates[0].Project != "" {
		t.Errorf("an expression working directory gives an unknown project: %+v", st[4].Gates)
	}
}

// A user's step after the output line is split off without taking the output line along.
func TestGitHubSplitKeepsOutputLine(t *testing.T) {
	_, got := result(t, npmRepo("web", head+gated+lines(
		"          ./deploy.sh",
		"      - uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
	), map[string]string{"deploy.sh*": "#!/bin/sh\necho deploy\n"}))
	if got != "" {
		t.Errorf("an up-to-date job changed:\n%s", got)
	}
	// The existing gate is after the step, so no split is made (COMMANDS_BEFORE_GATE covers it);
	// without the existing gate, the split keeps the output line in part 1.
	_, got = result(t, npmRepo("web", head+gated+lines(
		"          ./deploy.sh",
	), map[string]string{"deploy.sh*": "#!/bin/sh\necho deploy\n"}))
	wantWorkflow(t, got, head+gated+lines(
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - name: Build (continued)",
		"        run: |",
		"          ./deploy.sh",
	))
}

// Odd but valid workflows parse without errors or panics, and nothing breaks in them.
func TestGitHubOddWorkflows(t *testing.T) {
	for name, src := range map[string]string{
		"empty":        "",
		"no jobs":      "on: push\n",
		"null jobs":    "on: push\njobs:\n",
		"null step":    head + "      -\n      - run: npm run build\n",
		"scalar step":  head + "      - just a string\n      - run: npm run build\n",
		"mapping run":  head + "      - run: {a: 1}\n      - run: npm run build\n",
		"empty run":    head + "      - run: ''\n      - run: npm run build\n",
		"steps scalar": "on: push\njobs:\n  a:\n    steps: x\n",
		"comment only": "# nothing here\n",
	} {
		root := tree(t, npmRepo("web", src, nil))
		p := setup.Compute(setup.Options{Root: root, Yes: true, Version: "1.2.3"})
		if p.Error != nil {
			t.Errorf("%s: %+v", name, p.Error)
			continue
		}
		for _, c := range p.Changes {
			if c.Path == wf {
				var v any
				if err := yaml.Unmarshal([]byte(c.Content), &v); err != nil {
					t.Errorf("%s: invalid result: %v\n%s", name, err, c.Content)
				}
			}
		}
	}
}

// Per-platform builds of one repository get distinct scan tags, and a re-run keeps them.
func TestGitHubSameRepositoryTwice(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: |",
		"          docker build --platform linux/amd64 -t ghcr.io/acme/web:amd64 .",
		"          docker build --platform linux/arm64 -t ghcr.io/acme/web:arm64 -f Dockerfile .",
		"      - run: docker push ghcr.io/acme/web:amd64 && docker push ghcr.io/acme/web:arm64",
	), nil))
	for _, want := range []string{
		"-t ghcr.io/acme/web:amd64 -t ghcr.io/acme/web:shearbolt-${{ github.sha }} .",
		"-t ghcr.io/acme/web:arm64 -t ghcr.io/acme/web:shearbolt-2-${{ github.sha }} -f Dockerfile .",
		"echo shearbolt-image-2=ghcr.io/acme/web:shearbolt-2-${{ github.sha }}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("workflow lacks %q:\n%s", want, got)
		}
	}
}

// wantUnsupported checks that the job is left unchanged with UNSUPPORTED_EDIT giving reason.
func wantUnsupported(t *testing.T, files map[string]string, why, reason string) *plan.Plan {
	t.Helper()
	p := unsupported(t, files, why)
	if !slices.ContainsFunc(p.Notes, func(n plan.Note) bool { return n.Code == "UNSUPPORTED_EDIT" && strings.Contains(n.Message, reason) }) {
		t.Errorf("%s: no UNSUPPORTED_EDIT says %q: %v", why, reason, noteMessages(p))
	}
	return p
}

func noteMessages(p *plan.Plan) []string {
	var out []string
	for _, n := range p.Notes {
		out = append(out, n.Code+": "+n.Message)
	}
	return out
}

const differentIfs = "the gated builds run under different if: conditions, so one gate cannot follow them all"

// Review 1: the gate copies an if: only when every step holding one of its builds has that same
// condition. Copying the last build step's condition let an earlier build be pushed unscanned
// where the gate is skipped; the reverse scanned an image that was never built.
func TestGitHubGateIfMustMatchAllBuildSteps(t *testing.T) {
	main := "        if: github.ref == 'refs/heads/main'\n"
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 .",
		"      - run: docker build -f Dockerfile -t worker:1 .",
	)+main+lines(
		"      - run: docker push web:1",
	), nil), "unconditional, then conditional build", differentIfs)
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker buildx build --push -t web:1 .",
		"      - run: docker build -f Dockerfile -t worker:1 .",
	)+main, nil), "unconditional --push, then conditional build", differentIfs)
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 .",
	)+main+lines(
		"      - run: docker build -f Dockerfile -t worker:1 .",
		"      - run: docker push web:1 && docker push worker:1",
	), nil), "conditional, then unconditional build", differentIfs)

	// The same condition on every build step is copied.
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 .",
	)+main+lines(
		"      - run: docker build -f Dockerfile -t worker:1 .",
	)+main+lines(
		"      - run: docker push web:1 && docker push worker:1",
	), nil))
	if !strings.Contains(got, "      - name: shearbolt\n"+main) {
		t.Errorf("the shared condition is not copied:\n%s", got)
	}
}

const staleOutputs = "remove setup's output lines from this step and run setup again"

// Review 2: an output line counts only when setup's own line for that build follows it. After a
// build is inserted before an already set-up one, matching on the output name alone pointed both
// gate variables at the same image.
func TestGitHubOutputLineBelongsToItsBuild(t *testing.T) {
	gate := lines(
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"      - run: docker push web:1 && docker push api:1",
	)
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - name: Build",
		"        id: shearbolt-build",
		"        run: |",
		"          docker build -f Dockerfile -t api:1 .",
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
	)+gate, nil), "a build inserted before a set-up one", staleOutputs)

	// An output line that names another image than its build's scan tag.
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - name: Build",
		"        id: shearbolt-build",
		"        run: |",
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		`          echo shearbolt-image=api:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
	)+gate, nil), "an output line for another image", staleOutputs)

	// An output line that is not right after its build.
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - name: Build",
		"        id: shearbolt-build",
		"        run: |",
		`          echo shearbolt-image=web:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"          docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
	)+gate, nil), "an output line before its build", staleOutputs)
}

func twoProjects(workflow string, a, b string) map[string]string {
	return map[string]string{
		a + "/package.json": `{"name":"` + a + `"}`, a + "/package-lock.json": "{}", a + "/Dockerfile": "FROM x\n",
		b + "/package.json": `{"name":"` + b + `"}`, b + "/package-lock.json": "{}", b + "/Dockerfile": "FROM x\n",
		wf: workflow,
	}
}

// Review 3: builds of two projects in one step are each edited with their own gate.
func TestGitHubTwoGatesOneStepNoSplit(t *testing.T) {
	_, got := result(t, twoProjects(head+lines(
		"      - run: |",
		"          docker build -t a:1 a",
		"          docker build -t b:1 b",
		"      - run: docker push a:1 && docker push b:1",
	), "a", "b"))
	wantWorkflow(t, got, head+lines(
		"      - id: shearbolt-build",
		"        run: |",
		"          docker build -t a:1 -t a:shearbolt-${{ github.sha }} a",
		`          echo shearbolt-image=a:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"          docker build -t b:1 -t b:shearbolt-${{ github.sha }} b",
		`          echo shearbolt-image-2=b:shearbolt-${{ github.sha }} >> "$GITHUB_OUTPUT"`,
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}",
		"        with:",
		"          working-directory: a",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image-2 }}",
		"        with:",
		"          working-directory: b",
		"      - run: docker push a:1 && docker push b:1",
	))

	// A command between the two builds follows a's gated build, so a's gate would need a split
	// that b's build would end up after: the job is left alone.
	wantUnsupported(t, twoProjects(head+lines(
		"      - run: |",
		"          docker build -t a:1 a",
		"          ./notify.sh",
		"          docker build -t b:1 b",
		"      - run: docker push a:1 && docker push b:1",
	), "a", "b"), "a command between two projects' builds", "two projects")
}

// Review 3: scan tags are unique per job, so two projects pushing tags of one repository do not
// share a local scan tag.
func TestGitHubScanTagsUniquePerJob(t *testing.T) {
	_, got := result(t, twoProjects(head+lines(
		"      - run: docker build -t ghcr.io/acme/mono:api api",
		"      - run: docker build -t ghcr.io/acme/mono:web web",
		"      - run: docker push ghcr.io/acme/mono:api && docker push ghcr.io/acme/mono:web",
	), "api", "web"))
	for _, want := range []string{
		"docker build -t ghcr.io/acme/mono:api -t ghcr.io/acme/mono:shearbolt-${{ github.sha }} api",
		"docker build -t ghcr.io/acme/mono:web -t ghcr.io/acme/mono:shearbolt-2-${{ github.sha }} web",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("workflow lacks %q:\n%s", want, got)
		}
	}
}

// Two builds of a job that already carry the same shearbolt- tag cannot both be scanned: the job
// is left alone instead of scanning one image twice.
func TestGitHubDuplicateExistingScanTags(t *testing.T) {
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 -t web:shearbolt-${{ github.sha }} .",
		"      - run: docker build -f Dockerfile -t web:2 -t web:shearbolt-${{ github.sha }} .",
		"      - run: docker push web:1 && docker push web:2",
	), nil), "duplicate existing scan tags", "share the scan tag")
}

// Review 4: a --push build whose tags setup cannot read exactly would lose its push when it is
// converted to --load, so the job is left alone.
func TestGitHubPushBuildWithUnreadableTags(t *testing.T) {
	for _, script := range []string{
		"docker buildx build --push $TAG_ARGS .",
		"docker buildx build --push -t web:1 $CTX",
		`docker buildx build --push -t web:1 "${TAGS[@]}" .`,
		`docker buildx build --push -t web:1 "$@" .`,
		"docker buildx build --push -t web:1 $* .",
		`docker buildx build --push -t "$@" .`,
		"docker buildx build --push -t web:1 `cat extra-args` .",
		"docker buildx build --push .",
	} {
		wantUnsupported(t, npmRepo("web", head+lines("      - run: "+script), nil), script, "--push")
	}
	// A quoted variable is one word, and a GitHub expression is filled in before the shell runs.
	_, got := result(t, npmRepo("web", head+lines(`      - run: docker buildx build --push -t "$IMAGE:${{ github.sha }}" .`), nil))
	if !strings.Contains(got, `docker push "$IMAGE:${{ github.sha }}"`) {
		t.Errorf("workflow:\n%s", got)
	}
}

// Batch 4 decision: an unquoted expansion that is the value of a flag taking a value is accepted,
// and the push line repeats the tag word as written, so it expands the same way.
func TestGitHubPushBuildWithExpandedFlagValues(t *testing.T) {
	for script, push := range map[string]string{
		"docker buildx build --push -t $IMAGE:$TAG .":                        "docker push $IMAGE:$TAG",
		"docker buildx build --push --tag=$IMAGE:$TAG .":                     "docker push $IMAGE:$TAG",
		"docker buildx build --push --build-arg V=$(cat VERSION) -t web:1 .": "docker push web:1",
		"docker buildx build --push -f $DOCKERFILE --label v=$V -t web:1 .":  "docker push web:1",
	} {
		_, got := result(t, npmRepo("web", head+lines("      - name: Image", "        run: "+script), nil))
		if !strings.Contains(got, "        run: "+push+"\n") || !strings.Contains(got, "--load") {
			t.Errorf("%s:\n%s", script, got)
		}
	}
}

// Review 5: a job that fails after its edits are queued is rolled back completely. Here the gate,
// the --push conversion, the split and the id are queued before the aliased push step, whose
// if: always() must be wrapped but cannot be changed in place, fails the job.
func TestGitHubRollbackAfterQueuedEdits(t *testing.T) {
	src := lines(
		"x-push: &p {if: always(), uses: softprops/action-gh-release@v2}",
		"on: push",
		"jobs:",
		"  build:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - uses: actions/checkout@v5",
		"      - run: docker buildx build --push -t web:1 . && echo hi",
		"      - *p",
	)
	wantUnsupported(t, npmRepo("web", src, nil), "late failure", "runs even when the gate fails")
}

// Review 5: the per-job check that the edited file still parses also rolls a job back. GitHub
// rejects a mapping that repeats a key, so a job that has one is not edited.
func TestGitHubRollbackOnInvalidResult(t *testing.T) {
	src := strings.Replace(head, "    runs-on: ubuntu-latest\n", "    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    timeout-minutes: 10\n", 1)
	wantUnsupported(t, npmRepo("web", src+lines(
		"      - run: docker build -t web:1 .",
		"      - run: docker push web:1",
	), nil), "repeated key", "would not be valid YAML")
}

// An empty id: is filled in, not repeated.
func TestGitHubEmptyIDIsFilled(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - name: Build",
		"        id:",
		"        run: docker build -t web:1 .",
		"      - run: docker push web:1",
	), nil))
	if strings.Count(got, "id:") != 1 || !strings.Contains(got, "        id: shearbolt-build\n") {
		t.Errorf("workflow:\n%s", got)
	}
}

// Minor review fixes.

func TestGitHubPartialExpressionConditionRefused(t *testing.T) {
	// Wrapping a condition that holds ${{ }} next to other text would nest expressions and break
	// the workflow.
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 .",
		"      - if: ${{ always() }} && github.ref == 'refs/heads/main'",
		"        run: docker push web:1",
	), nil), "push step", "cannot be wrapped")
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - if: ${{ always() }} && github.ref == 'refs/heads/main'",
		"        run: docker build -t web:1 . && docker push web:1",
	), nil), "remainder", "cannot be wrapped")
}

func TestGitHubConditionalStateRefused(t *testing.T) {
	for _, script := range []string{
		"test -d sub && cd sub\ndocker build -t web:1 .\ndocker push web:1",
		"[ -n \"$X\" ] || export A=1\ndocker build -t web:1 .\ndocker push web:1",
		"if [ -n \"$X\" ]; then\n  export A=1\nfi\ndocker build -t web:1 .\ndocker push web:1",
		"cd sub\ncd -\ndocker build -t web:1 .\ndocker push web:1",
		"f() { :; }\nexport -f f\ndocker build -t web:1 .\ndocker push web:1",
	} {
		unsupported(t, npmRepo("web", head+"      - run: |\n"+indent(script, "          "), map[string]string{"sub/x": "x\n"}), script)
	}
}

func TestGitHubInstallMarkerIgnoresComments(t *testing.T) {
	files := rustFiles(head + lines(
		"      # cargo-auditable comes later",
		"      - run: |",
		"          # TODO cargo-auditable",
		"          echo hi",
		"      - run: cargo build --release",
	))
	_, got := result(t, files)
	if !strings.Contains(got, "uses: taiki-e/install-action@v2") {
		t.Errorf("a comment suppressed the install:\n%s", got)
	}
	// The marker in the same script, before the build, does count.
	files[wf] = head + lines("      - run: cargo install cargo-auditable --locked && cargo build --release")
	_, got = result(t, files)
	if strings.Contains(got, "taiki-e/install-action") || !strings.Contains(got, "cargo auditable build --release") {
		t.Errorf("workflow:\n%s", got)
	}
}

func TestGitHubWinRunnerLabels(t *testing.T) {
	for _, label := range []string{"[self-hosted, Win64]", "WIN-2022", "my-WINDOWS-box"} {
		unsupported(t, npmRepo("web", strings.Replace(head, "ubuntu-latest", label, 1)+lines(
			"      - run: docker build -t web:1 .",
		), nil), label)
	}
}

func TestGitHubBuildPushActionPushCase(t *testing.T) {
	src := head + lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: TRUE",
	)
	jobs, _, err := github.Plugin{}.Jobs(plan.Repo{Root: t.TempDir()}, wf, []byte(src), func(shell.Command, plan.Context) plan.Classified { return plan.Classified{} })
	if err != nil || len(jobs[0].Steps[1].Builds) != 1 || !jobs[0].Steps[1].Builds[0].Image.Push || len(jobs[0].Steps[1].Push) != 0 {
		t.Fatalf("push: TRUE is a push: %+v, %v", jobs, err)
	}
}

// Guards the review found untested.

func TestGitHubIfCopiedOnlyFromBuildSteps(t *testing.T) {
	_, got := result(t, npmRepo("web", lines(
		"on: push",
		"jobs:",
		"  test:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - uses: actions/checkout@v5",
		"        if: github.event_name == 'push'",
		"      - run: npm test",
	), nil))
	if !strings.Contains(got, "      - name: shearbolt\n        uses: klar-web-services/shearbolt@v1\n") {
		t.Errorf("the gate after checkout copies no if:\n%s", got)
	}
}

func TestGitHubExistingGateInPushStep(t *testing.T) {
	wantUnsupported(t, npmRepo("web", head+lines(
		`      - run: docker buildx build --push -t web:1 . && "$HOME/.local/bin/shearbolt" scan --ci`,
	), nil), "existing gate in the --push step", "same step as the --push build")
}

func TestGitHubPushLinesSkipScanTag(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - name: Image",
		"        run: docker buildx build --push -t web:1 -t web:shearbolt-${{ github.sha }} .",
	), nil))
	if !strings.Contains(got, "        run: docker push web:1\n") || strings.Contains(got, "docker push web:shearbolt") {
		t.Errorf("workflow:\n%s", got)
	}
}

// x and ${{ x }} are the same condition for the gate's if: copy.
func TestGitHubGateIfComparesBareExpressions(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 .",
		"        if: github.ref == 'refs/heads/main'",
		"      - run: docker build -f Dockerfile -t worker:1 .",
		"        if: ${{ github.ref == 'refs/heads/main' }}",
		"      - run: docker push web:1 && docker push worker:1",
	), nil))
	if !strings.Contains(got, "      - name: shearbolt\n        if: ${{ github.ref == 'refs/heads/main' }}\n") {
		t.Errorf("workflow:\n%s", got)
	}
}

// A step's name does not count as mentioning the install marker.
func TestGitHubInstallMarkerIgnoresNames(t *testing.T) {
	_, got := result(t, rustFiles(head+lines(
		"      - name: cargo-auditable comes later",
		"        run: echo hi",
		"      - run: cargo build --release",
	)))
	if !strings.Contains(got, "uses: taiki-e/install-action@v2") {
		t.Errorf("a step name suppressed the install:\n%s", got)
	}
}

// A run-step scan whose failure would not stop the job is not a gate setup can keep (Batch 5
// review): the job fails.
func TestGitHubExistingRunStepGateMustStop(t *testing.T) {
	for name, line := range map[string]string{
		"or true":  `          "$HOME/.local/bin/shearbolt" scan --ci || true`,
		"piped":    `          "$HOME/.local/bin/shearbolt" scan --ci | tee scan.log`,
		"and echo": `          "$HOME/.local/bin/shearbolt" scan --ci && echo scanned`,
		"negated":  `          ! "$HOME/.local/bin/shearbolt" scan --ci`,
		"if":       `          if [ -n "$X" ]; then "$HOME/.local/bin/shearbolt" scan --ci; fi`,
	} {
		wantUnsupported(t, npmRepo("web", head+lines(
			"      - run: docker build -t web:1 .",
			"      - run: |",
			"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
			line,
			"      - run: docker push web:1",
		), nil), name, "would not stop the job")
	}
}

// docker push --all-tags of a scanned repository would push the local scan tag: the job fails.
func TestGitHubAllTagsPushRefused(t *testing.T) {
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker build -t ghcr.io/acme/web:1 .",
		"      - run: docker push --all-tags ghcr.io/acme/web",
	), nil), "all-tags push", "--all-tags")
}

// An existing gate step that may continue on error cannot stop the job: the job fails (Batch 5
// re-review). continue-on-error: false is fine.
func TestGitHubExistingGateContinueOnError(t *testing.T) {
	runGate := lines(
		"        run: |",
		"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
		`          "$HOME/.local/bin/shearbolt" scan --ci`,
	)
	for name, gate := range map[string]string{
		"action":     "      - uses: klar-web-services/shearbolt@v1\n        continue-on-error: true\n",
		"action var": "      - uses: klar-web-services/shearbolt@v1\n        continue-on-error: ${{ matrix.experimental }}\n",
		"run step":   "      - continue-on-error: true\n" + runGate,
	} {
		wantUnsupported(t, npmRepo("web", head+"      - run: docker build -t web:1 .\n"+gate+"      - run: docker push web:1\n", nil), name, "continue-on-error")
	}
	_, got := result(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 .",
		"      - uses: klar-web-services/shearbolt@v1",
		"        continue-on-error: false",
		"      - run: docker push web:1",
	), nil))
	if !strings.Contains(got, "SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}") {
		t.Errorf("workflow:\n%s", got)
	}
}

// A run-step scan after set +e in its step would not fail the step: the job fails.
func TestGitHubRunStepScanAfterSetE(t *testing.T) {
	for _, off := range []string{"set +e", "set +o errexit"} {
		wantUnsupported(t, npmRepo("web", head+lines(
			"      - run: docker build -t web:1 .",
			"      - run: |",
			"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
			"          "+off,
			`          "$HOME/.local/bin/shearbolt" scan --ci`,
			"          echo done",
			"      - run: docker push web:1",
		), nil), off, "errexit")
	}
}

// The sh a user's own install pipes into belongs to the install: a run step that installs and
// scans gives no COMMANDS_BEFORE_GATE.
func TestGitHubUserInstallIsPartOfTheGate(t *testing.T) {
	for _, install := range []string{
		"curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
		"wget -qO- https://tools.klar.ws/shearbolt/install.sh | SHEARBOLT_VERSION=1.2.3 sh",
	} {
		p, _ := result(t, npmRepo("web", head+lines(
			"      - run: docker build -t web:1 .",
			"      - run: |",
			"          "+install,
			`          "$HOME/.local/bin/shearbolt" scan --ci`,
			"      - run: docker push web:1",
		), nil))
		if hasNote(p, "COMMANDS_BEFORE_GATE") {
			t.Errorf("%s: notes %v", install, noteMessages(p))
		}
	}
}

// A run-step gate under a custom shell without errexit does not stop the step when a command
// follows the scan: the job fails. bash, sh and shells with -e are fine (Batch 5 final review).
func TestGitHubCustomShellWithoutErrexit(t *testing.T) {
	gate := func(shellLine string, after ...string) string {
		return head + lines(append([]string{
			"      - run: docker build -t web:1 .",
			shellLine,
			"        run: |",
			"          curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh",
			`          "$HOME/.local/bin/shearbolt" scan --ci`,
		}, append(after, "      - run: docker push web:1")...)...)
	}
	wantUnsupported(t, npmRepo("web", gate("      - shell: bash {0}", "          echo done"), nil), "bash {0}", "errexit")
	// Through defaults.run.shell too.
	wf := strings.Replace(gate("      - name: Gate", "          echo done"), "    runs-on: ubuntu-latest\n", "    runs-on: ubuntu-latest\n    defaults:\n      run:\n        shell: sh {0}\n", 1)
	wantUnsupported(t, npmRepo("web", wf, nil), "defaults sh {0}", "errexit")

	for _, ok := range []string{
		gate("      - shell: bash {0}"), // the scan is the last command
		gate("      - shell: bash --noprofile --norc -eo pipefail {0}", "          echo done"),
		gate("      - shell: sh -e {0}", "          echo done"),
		gate("      - shell: bash", "          echo done"),
	} {
		_, got := result(t, npmRepo("web", ok, nil))
		if !strings.Contains(got, "SHEARBOLT_IMAGE: ${{ steps.shearbolt-build.outputs.shearbolt-image }}") {
			t.Errorf("workflow:\n%s", got)
		}
	}
}
