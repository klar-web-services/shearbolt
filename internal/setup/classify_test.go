package setup

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// rustMake is the Makefile of the rust-make fixture.
const rustMake = `CARGO ?= cargo

.PHONY: dev test release

# Debug build for local work
dev:
	$(CARGO) build

test:
	$(CARGO) test --locked

# Release binary used by CI
release: test
	$(CARGO) build --release --locked
	@ls -lh target/release/demo
`

const demoToml = "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"

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

// demoRepo is a Rust project at the root, plus files.
func demoRepo(t *testing.T, files map[string]string) plan.Repo {
	t.Helper()
	all := map[string]string{"Cargo.toml": demoToml, "Cargo.lock": "version = 3\n", "src/main.rs": "fn main() {}\n"}
	for k, v := range files {
		all[k] = v
	}
	return repo(t, all)
}

// classifyAll classifies every command of script, run from the repository root.
func classifyAll(t *testing.T, r plan.Repo, script string) ([]shell.Command, []plan.Classified) {
	t.Helper()
	classify := newClassifier(r, ecosystems)
	cmds, _ := shell.Parse(script, shell.State{Dir: "."})
	var out []plan.Classified
	for _, c := range cmds {
		env := map[string]bool{}
		for _, n := range append(slices.Clone(c.Before.Env), c.Prefix()...) {
			env[n] = true
		}
		out = append(out, classify(c, plan.Context{Dir: c.Before.Dir, Env: env}))
	}
	return cmds, out
}

// one classifies a script that is a single command.
func one(t *testing.T, r plan.Repo, script string) plan.Classified {
	t.Helper()
	cmds, cl := classifyAll(t, r, script)
	if len(cmds) != 1 {
		t.Fatalf("%q parsed into %d commands", script, len(cmds))
	}
	return cl[0]
}

// image classifies a single image build command.
func image(t *testing.T, script string) plan.Build {
	t.Helper()
	cl := one(t, repo(t, nil), script)
	if len(cl.Builds) != 1 || cl.Push || cl.Gate != nil {
		t.Fatalf("%q: %+v", script, cl)
	}
	return cl.Builds[0]
}

func TestImageBuilds(t *testing.T) {
	b := image(t, "docker build -t a:1 -t a:2 -f docker/web.Dockerfile web")
	want := plan.Image{Tool: "docker", Tags: []string{"a:1", "a:2"}, Dockerfile: "docker/web.Dockerfile", Context: "web", SubEnd: 12, TagEnd: 19}
	if b.Kind != plan.BuildImage || b.Image == nil || !reflect.DeepEqual(*b.Image, want) || b.Dir != "docker" {
		t.Fatalf("build = %+v, image %+v", b, b.Image)
	}
	if b.Cmd.Words[0].Value != "docker" || b.Via != "" {
		t.Fatalf("Cmd = %+v", b.Cmd)
	}

	b = image(t, "docker build . --target prod -t x --build-arg A=1")
	if b.Image.Context != "." || !slices.Equal(b.Image.Tags, []string{"x"}) || b.Image.Dockerfile != "Dockerfile" || b.Dir != "." {
		t.Fatalf("image = %+v", b.Image)
	}

	b = image(t, "docker buildx build --platform linux/amd64,linux/arm64 --push -t x .")
	if b.Kind != plan.BuildImage || b.Image.Tool != "buildx" || b.Image.Platforms != 2 || !b.Image.Push || b.Image.Gateable() {
		t.Fatalf("multi-platform push: %+v", b.Image)
	}
	b = image(t, "docker buildx build --push -t x .")
	if !b.Image.Push || !b.Image.Gateable() || b.Image.Platforms > 1 {
		t.Fatalf("--push: %+v", b.Image)
	}

	b = image(t, `docker image build --tag="$IMAGE:latest" -tghcr.io/x/y:1 --file=Containerfile ctx`)
	if !slices.Equal(b.Image.Tags, []string{`"$IMAGE:latest"`, "ghcr.io/x/y:1"}) || b.Image.Dockerfile != "Containerfile" || b.Dir != "." || b.Image.Context != "ctx" {
		t.Fatalf("image build forms: %+v, dir %q", b.Image, b.Dir)
	}
	b = image(t, "docker --context ci build -t x:${{ github.sha }} apps/web")
	if b.Image.Context != "apps/web" || b.Image.Dockerfile != "apps/web/Dockerfile" || b.Dir != "apps/web" || b.Image.Tags[0] != "x:${{ github.sha }}" {
		t.Fatalf("global options: %+v", b.Image)
	}
	b = image(t, "docker buildx build --platform $PLATFORMS -t x .")
	if b.Image.Gateable() {
		t.Fatalf("a variable platform list is treated as multi-platform: %+v", b.Image)
	}
	for s, dir := range map[string]string{
		"docker build -t x $CTX":                       "",
		"docker build -t x https://github.com/a/b.git": "",
		"docker build -f $DF -t x .":                   "",
		"docker build -f - -t x .":                     "",
		"docker build -t x -":                          "",
	} {
		if b := image(t, s); b.Kind != plan.BuildImage || b.Dir != dir {
			t.Errorf("%q: build %+v, image %+v", s, b, b.Image)
		}
	}

	_, cl := classifyAll(t, repo(t, nil), "cd $X && docker build -t x .")
	if b := cl[1].Builds[0]; b.Dir != "" || b.Image.Context != "" {
		t.Fatalf("unknown directory: %+v", b.Image)
	}
}

// TestImageEditOffsets checks the offsets CI plugins edit at: after the build subcommand (where
// --load goes), after the first tag's word (where the scan tag goes), and the --push words.
func TestImageEditOffsets(t *testing.T) {
	for _, c := range []struct {
		script, sub, tag string
		push             []string
	}{
		{script: "docker build -t a:1 -t a:2 .", sub: "docker build", tag: "docker build -t a:1"},
		{script: "docker buildx build --push -t x .", sub: "docker buildx build", tag: "docker buildx build --push -t x", push: []string{"--push"}},
		{script: `docker build --tag="$I:1" .`, sub: "docker build", tag: `docker build --tag="$I:1"`},
		{script: "docker build -tx:1 --push=true .", sub: "docker build", tag: "docker build -tx:1", push: []string{"--push=true"}},
		{script: "sudo docker --context ci image build .", sub: "sudo docker --context ci image build"},
		{script: "docker buildx --builder b build -t y .", sub: "docker buildx --builder b build", tag: "docker buildx --builder b build -t y"},
	} {
		b := image(t, c.script)
		img := b.Image
		if got := c.script[:img.SubEnd]; got != c.sub {
			t.Errorf("%q: text before SubEnd = %q, want %q", c.script, got, c.sub)
		}
		if c.tag == "" && img.TagEnd != 0 || c.tag != "" && c.script[:img.TagEnd] != c.tag {
			t.Errorf("%q: TagEnd %d, want after %q", c.script, img.TagEnd, c.tag)
		}
		var push []string
		for _, sp := range img.PushArgs {
			push = append(push, c.script[sp[0]:sp[1]])
		}
		if !slices.Equal(push, c.push) {
			t.Errorf("%q: push words %q, want %q", c.script, push, c.push)
		}
	}
}

func TestImageOutputs(t *testing.T) {
	for _, s := range []string{
		"docker buildx build -o type=registry -t x .",
		"docker buildx build --output type=image,push=true -t x .",
		"docker buildx build --output=type=image,name=x,push=true .",
		"docker buildx build -o $OUT -t x .",
	} {
		b := image(t, s)
		if b.Kind != plan.BuildImage || b.Image == nil || !b.Image.OutputPush || b.Image.Gateable() {
			t.Errorf("%q: %+v, image %+v", s, b, b.Image)
		}
	}
	for _, s := range []string{
		"docker buildx build --output type=docker -t x .",
		"docker buildx build --load -t x .",
		"docker buildx build -o type=docker,name=x .",
	} {
		b := image(t, s)
		if b.Kind != plan.BuildImage || b.Image == nil || !b.Image.Load || !b.Image.Gateable() {
			t.Errorf("%q: %+v, image %+v", s, b, b.Image)
		}
	}
	for _, s := range []string{
		"docker buildx build -o type=local,dest=out .",
		"docker buildx build --output type=tar,dest=x.tar .",
		"docker buildx build -o out .",
		"docker buildx build --output type=docker,dest=img.tar .",
		"docker buildx build -o type=oci,dest=x .",
		"docker buildx build -o type=cacheonly .",
		"docker buildx build -o type=image,name=x .",
	} {
		b := image(t, s)
		if b.Kind != plan.BuildOther || b.Image != nil || b.Dir != "." {
			t.Errorf("%q: %+v", s, b)
		}
	}
}

func TestRailpack(t *testing.T) {
	b := image(t, "railpack build apps/blog --name x:1")
	want := plan.Image{Tool: "railpack", Tags: []string{"x:1"}, Context: "apps/blog"}
	if b.Kind != plan.BuildImage || b.Image == nil || !reflect.DeepEqual(*b.Image, want) || b.Dir != "apps/blog" {
		t.Fatalf("build = %+v, image %+v", b, b.Image)
	}
	b = image(t, "railpack build . --name ghcr.io/acme/blog:latest --platform linux/arm64")
	if b.Image.Context != "." || b.Image.Platforms != 1 || b.Image.Tags[0] != "ghcr.io/acme/blog:latest" {
		t.Fatalf("image = %+v", b.Image)
	}
	b = image(t, "railpack build .")
	if b.Kind != plan.BuildOther || b.Image != nil || len(b.Notes) != 1 || b.Notes[0].Code != "RAILPACK_NO_NAME" || !strings.Contains(b.Notes[0].Message, "--name") {
		t.Fatalf("no name: %+v", b)
	}
	if b := image(t, "railpack build"); b.Dir != "." || b.Kind != plan.BuildOther {
		t.Fatalf("default directory: %+v", b)
	}
}

func TestPushes(t *testing.T) {
	r := repo(t, nil)
	for _, s := range []string{
		"docker push x", "docker image push x", "docker manifest push x", "docker buildx imagetools create -t x y",
		"podman push x", "buildah push x", "crane push x y", "crane copy x y", "skopeo copy a b",
		"npm publish", "pnpm publish", "bun publish", "yarn publish", "yarn npm publish", "cargo publish",
		"gh release create v1", "gh release upload v1 x", "fly deploy", "flyctl deploy", "vercel deploy",
		"vercel --prod", "railway up", "kubectl apply -f x", "kubectl set image d x=y", "kubectl rollout restart d",
		"kubectl create -f x", "helm upgrade x y", "helm install x y",
		"kubectl -n prod apply -f x", "docker --context ci push x", "gh -R o/r release upload v1 x",
		"sudo docker push x", "cargo +nightly publish", "/usr/bin/docker push x", "npm --prefix web publish",
	} {
		if cl := one(t, r, s); !cl.Push || len(cl.Builds) != 0 || cl.Gate != nil {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	for _, s := range []string{
		"docker pull x", "docker login", "npm ci", "gh pr create", "kubectl get pods", "helm repo add x y",
		"echo docker push", "vercel build", "cargo build", "docker tag a b", "gh release view v1",
	} {
		if cl := one(t, r, s); cl.Push {
			t.Errorf("%q is a push", s)
		}
	}
}

func TestComposePushesAndStackDeploys(t *testing.T) {
	r := repo(t, nil)
	for _, s := range []string{
		"docker compose push", "docker compose -f prod.yml -p app push web",
		"docker compose --profile ci --env-file .env.ci push", "docker-compose -f a.yml push",
		"docker --context prod stack deploy -c compose.yml app", "docker stack deploy --compose-file compose.yml app",
	} {
		if cl := one(t, r, s); !cl.Push || len(cl.Builds) != 0 || cl.Gate != nil {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	for _, s := range []string{
		"docker compose up -d", "docker compose -f prod.yml pull", "docker compose build", "docker-compose up",
	} {
		if cl := one(t, r, s); cl.Push {
			t.Errorf("%q is a push", s)
		}
	}
}

func TestGateCommands(t *testing.T) {
	r := repo(t, nil)
	install := "(curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | SHEARBOLT_VERSION=1.2.3 sh"
	cmds, cl := classifyAll(t, r, install)
	found := false
	for i, c := range cl {
		if c.Push || len(c.Builds) != 0 {
			t.Fatalf("install line command %q: %+v", cmds[i].Argv(), c)
		}
		if c.Gate != nil {
			if !c.Gate.Install {
				t.Fatalf("gate = %+v", c.Gate)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("the install line gives no install gate")
	}

	for s, project := range map[string]string{
		`SHEARBOLT_IMAGE=x "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/apps/web/shearbolt.yaml"`: "apps/web",
		`"$HOME/.local/bin/shearbolt" scan --ci`:                                ".",
		`shearbolt scan --config=apps/api/shearbolt.yaml`:                       "apps/api",
		`./shearbolt scan -config ${BITBUCKET_CLONE_DIR}/x/shearbolt.yaml --ci`: "x",
		`shearbolt scan --config shearbolt.yaml`:                                ".",
		`shearbolt scan --config $OTHER/x.yaml`:                                 "",
		`shearbolt scan --config /etc/shearbolt.yaml`:                           "",
		`shearbolt scan --config "${{ github.workspace }}/web/shearbolt.yaml"`:  "web",
	} {
		cl := one(t, r, s)
		if cl.Gate == nil || cl.Gate.Install || cl.Gate.Project != project || cl.Push || len(cl.Builds) != 0 {
			t.Errorf("%q: %+v (gate %+v), want project %q", s, cl, cl.Gate, project)
		}
	}
	_, cl = classifyAll(t, r, "cd apps/web && shearbolt scan --ci")
	if cl[1].Gate == nil || cl[1].Gate.Project != "apps/web" {
		t.Fatalf("scan in a directory: %+v", cl[1].Gate)
	}
	if cl := one(t, r, "shearbolt version"); cl.Gate != nil {
		t.Fatalf("shearbolt version is a gate: %+v", cl.Gate)
	}
}

func TestEcosystemBuilds(t *testing.T) {
	r := demoRepo(t, nil)
	cl := one(t, r, "cargo build --release --locked")
	if len(cl.Builds) != 1 || cl.Builds[0].Kind != plan.BuildBinary || !slices.Equal(cl.Builds[0].Paths, []string{"target/release/demo"}) {
		t.Fatalf("cargo: %+v", cl)
	}
	cl = one(t, r, "npm run build")
	if len(cl.Builds) != 1 || cl.Builds[0].Kind != plan.BuildOther {
		t.Fatalf("npm: %+v", cl)
	}
}

// viaBuild returns the only build of cl that is not RewriteOnly.
func gateable(cl plan.Classified) []plan.Build {
	var out []plan.Build
	for _, b := range cl.Builds {
		if !b.RewriteOnly {
			out = append(out, b)
		}
	}
	return out
}

func rewriteOnly(cl plan.Classified) []plan.Build {
	var out []plan.Build
	for _, b := range cl.Builds {
		if b.RewriteOnly {
			out = append(out, b)
		}
	}
	return out
}

func TestMakefile(t *testing.T) {
	r := demoRepo(t, map[string]string{"Makefile": rustMake})
	cl := one(t, r, "make release")
	g := gateable(cl)
	if len(g) != 1 || len(cl.Builds) != 1 || cl.Push {
		t.Fatalf("make release: %+v", cl)
	}
	b := g[0]
	if b.Kind != plan.BuildBinary || b.Via != "Makefile" || !slices.Equal(b.Paths, []string{"target/release/demo"}) || b.Pos != (plan.Pos{}) {
		t.Fatalf("build = %+v", b)
	}
	if got := rustMake[b.Cmd.Start:b.Cmd.End]; got != "$(CARGO) build --release --locked" {
		t.Fatalf("Cmd offsets give %q", got)
	}
	if w := b.Cmd.Words[1]; rustMake[w.Start:w.Start+len(w.Raw)] != "build" {
		t.Fatalf("word offsets are not in the Makefile: %+v", w)
	}

	cl = one(t, r, "make test")
	if len(gateable(cl)) != 0 || len(rewriteOnly(cl)) != 1 || cl.Push {
		t.Fatalf("make test: %+v", cl)
	}
	if ro := rewriteOnly(cl)[0]; ro.Via != "Makefile" || rustMake[ro.Cmd.Start:ro.Cmd.End] != "$(CARGO) build --release --locked" || len(ro.Notes) != 0 {
		t.Fatalf("RewriteOnly build = %+v", ro)
	}

	// No target: the first one that does not start with "." (dev, a debug build).
	if cl := one(t, r, "make"); len(gateable(cl)) != 0 || len(rewriteOnly(cl)) != 1 {
		t.Fatalf("make: %+v", cl)
	}
	r = demoRepo(t, map[string]string{"Makefile": ".DEFAULT_GOAL := release\n" + rustMake})
	if cl := one(t, r, "$(MAKE)"); len(gateable(cl)) != 1 {
		t.Fatalf(".DEFAULT_GOAL: %+v", cl)
	}

	// release reaches the build through a prerequisite (order-only ones are ignored).
	mk := "release: build | dirs\n\techo done\nbuild: ; cargo build --release\ndirs:\n\tcargo build --release --bin never\n"
	r = demoRepo(t, map[string]string{"Makefile": mk})
	cl = one(t, r, "make release")
	if g := gateable(cl); len(g) != 1 || mk[g[0].Cmd.Start:g[0].Cmd.End] != "cargo build --release" {
		t.Fatalf("prerequisite: %+v", cl)
	}
	if ro := rewriteOnly(cl); len(ro) != 1 || !strings.Contains(mk[ro[0].Cmd.Start:ro[0].Cmd.End], "never") {
		t.Fatalf("order-only prerequisite: %+v", ro)
	}

	// make -C reads that directory's Makefile, and its recipes run there.
	r = demoRepo(t, map[string]string{
		"crates/cli/Makefile": "all:\n\tcargo build --release \\\n\t  --locked\n",
	})
	cl = one(t, r, "make -C crates/cli")
	if g := gateable(cl); len(g) != 1 || g[0].Via != "crates/cli/Makefile" || g[0].Dir != "crates/cli" {
		t.Fatalf("make -C: %+v", cl)
	} else if src := "all:\n\tcargo build --release \\\n\t  --locked\n"; src[g[0].Cmd.Start:g[0].Cmd.End] != "cargo build --release \\\n\t  --locked" {
		t.Fatalf("continued recipe: %q", src[g[0].Cmd.Start:g[0].Cmd.End])
	}
	if cl := one(t, r, "make --directory=crates/cli -f Makefile all"); len(gateable(cl)) != 1 {
		t.Fatalf("make --directory -f: %+v", cl)
	}

	// Each recipe line runs in its own shell from the Makefile's directory.
	r = demoRepo(t, map[string]string{
		"Makefile":        "all:\n\tcd sub && cargo build --release\n\tcargo build --release\n",
		"sub/Cargo.toml":  "[package]\nname = \"sub\"\nedition = \"2021\"\n",
		"sub/Cargo.lock":  "",
		"sub/src/main.rs": "",
	})
	cl = one(t, r, "make")
	if g := gateable(cl); len(g) != 2 || g[0].Dir != "sub" || g[1].Dir != "." || g[0].Paths[0] != "target/release/sub" {
		t.Fatalf("recipe line state: %+v", cl)
	}

	// Exported Make variables and command-line variables reach the recipes.
	for _, s := range []string{"make", "make CARGO_TARGET_DIR=/tmp/t"} {
		mk := "all:\n\tcargo build --release\n"
		if s == "make" {
			mk = "export CARGO_TARGET_DIR := /tmp/t\n" + mk
		}
		r = demoRepo(t, map[string]string{"Makefile": mk})
		cl = one(t, r, s)
		g := gateable(cl)
		if len(g) != 1 || g[0].Paths != nil || len(g[0].Notes) != 1 || g[0].Notes[0].Code != "RUST_PATH_UNKNOWN" {
			t.Fatalf("%q: %+v", s, cl)
		}
		if n := g[0].Notes[0]; n.File != "Makefile" || n.Line != strings.Count(mk[:g[0].Cmd.Start], "\n")+1 {
			t.Fatalf("note location = %+v", n)
		}
	}
}

func TestJustfile(t *testing.T) {
	just := "set shell := [\"bash\", \"-c\"]\n\nalias r := release\n\n# default\ncheck:\n    cargo test\n\nrelease target='x86_64': build\n    @echo released {{target}}\n\n[private]\nbuild: (lint \"strict\")\n    cargo build --release --locked\n\nlint mode:\n    cargo clippy\n\nother:\n    cargo build --release --bin other\n"
	r := demoRepo(t, map[string]string{"justfile": just})
	cl := one(t, r, "just release")
	g := gateable(cl)
	if len(g) != 1 || g[0].Via != "justfile" || just[g[0].Cmd.Start:g[0].Cmd.End] != "cargo build --release --locked" {
		t.Fatalf("just release: %+v", cl)
	}
	if ro := rewriteOnly(cl); len(ro) != 1 || !strings.Contains(just[ro[0].Cmd.Start:ro[0].Cmd.End], "--bin other") {
		t.Fatalf("RewriteOnly: %+v", ro)
	}
	if cl := one(t, r, "just r aarch64"); len(gateable(cl)) != 1 {
		t.Fatalf("alias with an argument: %+v", cl)
	}
	if cl := one(t, r, "just"); len(gateable(cl)) != 0 {
		t.Fatalf("default recipe (check): %+v", cl)
	}
	if cl := one(t, r, "just --list"); len(cl.Builds) != 0 {
		t.Fatalf("just --list: %+v", cl)
	}

	// A shebang recipe is one script; a justfile is found in a parent directory.
	just = "build:\n    #!/usr/bin/env bash\n    set -euo pipefail\n    cd crates\n    cargo build --release\n\npy:\n    #!/usr/bin/env python3\n    print('cargo build --release')\n"
	r = demoRepo(t, map[string]string{"Justfile": just, "crates/x.txt": ""})
	_, cls := classifyAll(t, r, "cd crates && just build py")
	if g := gateable(cls[1]); len(g) != 1 || g[0].Dir != "crates" || g[0].Via != "Justfile" {
		t.Fatalf("shebang recipe: %+v", cls[1])
	}
}

func TestScripts(t *testing.T) {
	script := "#!/bin/sh\nset -eu\n# macOS runners build here; Linux uses `make release`.\ncargo build --release --locked\nstrip target/release/demo || true\n"
	r := demoRepo(t, map[string]string{"scripts/build-release.sh": script, "Makefile": rustMake, "scripts/make.sh": "make release\n"})
	for _, s := range []string{"./scripts/build-release.sh", "bash scripts/build-release.sh", "sh -eu scripts/build-release.sh", "scripts/build-release.sh"} {
		cl := one(t, r, s)
		g := gateable(cl)
		if len(g) != 1 || g[0].Via != "scripts/build-release.sh" || script[g[0].Cmd.Start:g[0].Cmd.End] != "cargo build --release --locked" || cl.Push {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	cl := one(t, r, "bash ./scripts/make.sh")
	if g := gateable(cl); len(g) != 1 || g[0].Via != "Makefile" {
		t.Fatalf("a script calling make: %+v", cl)
	}
	if cl := one(t, r, "bash -c ./scripts/build-release.sh"); len(cl.Builds) != 0 {
		t.Fatalf("bash -c is not a file call: %+v", cl)
	}

	// A script that builds and then pushes makes the calling command a push.
	release := "#!/usr/bin/env bash\ncargo build --release\ngh release upload \"$TAG\" target/release/demo\n"
	r = demoRepo(t, map[string]string{"release.sh": release})
	cl = one(t, r, "./release.sh")
	g := gateable(cl)
	if !cl.Push || len(g) != 1 {
		t.Fatalf("build then push: %+v", cl)
	}
	notes := g[0].Notes
	if len(notes) != 1 || notes[0].Code != "BUILD_AFTER_PUSH" || !strings.Contains(notes[0].Message, "release.sh") || notes[0].File != "release.sh" || notes[0].Line != 2 {
		t.Fatalf("notes = %+v", notes)
	}

	// The script runs from the caller's directory and state.
	r = demoRepo(t, map[string]string{"tools/b.sh": "cargo build --release\n"})
	_, cls := classifyAll(t, r, "export CARGO_TARGET_DIR=x\nbash tools/b.sh")
	if g := gateable(cls[1]); len(g) != 1 || g[0].Dir != "." || g[0].Paths != nil {
		t.Fatalf("caller state: %+v", cls[1])
	}

	// A conditional call makes its builds conditional.
	r = demoRepo(t, map[string]string{"Makefile": rustMake})
	_, cls = classifyAll(t, r, "if [ -n \"$X\" ]; then\n  make release\nfi")
	if g := gateable(cls[1]); len(g) != 1 || g[0].Kind != plan.BuildOther || g[0].Notes[0].Code != "RUST_CONDITIONAL_BUILD" || g[0].Notes[0].File != "Makefile" {
		t.Fatalf("conditional call: %+v", cls[1])
	}

	// Executables that are not shell scripts cannot be followed, so they count as pushes (M6: a
	// file without #! is a shell script).
	r = demoRepo(t, map[string]string{"bin/tool": "\x7fELF\x00cargo build --release\n", "bin/py": "#!/usr/bin/env python3\ncargo build --release\n", "bin/plain": "cargo build --release\n"})
	for _, s := range []string{"./bin/tool", "./bin/py"} {
		if cl := one(t, r, s); len(cl.Builds) != 0 || !cl.Push {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	for _, s := range []string{"sh bin/plain", "./bin/plain", "bin/plain"} {
		if cl := one(t, r, s); len(cl.Builds) != 1 || cl.Push {
			t.Errorf("%q: a script without a shebang: %+v", s, cl)
		}
	}
}

func TestCallLimits(t *testing.T) {
	// Depth: the CI calls a.sh (1), which calls b.sh (2), c.sh (3) and d.sh (4, not followed).
	r := demoRepo(t, map[string]string{
		"a.sh": "./b.sh\n",
		"b.sh": "./c.sh\ncargo build --release --bin b\n",
		"c.sh": "./d.sh\ncargo build --release --bin c\n",
		"d.sh": "cargo build --release --bin d\ndocker push x\n",
	})
	cl := one(t, r, "./a.sh")
	var bins []string
	for _, b := range cl.Builds {
		bins = append(bins, b.Paths...)
	}
	// d.sh is beyond the depth limit, so it cannot be followed and counts as a push.
	if !slices.Equal(bins, []string{"target/release/c", "target/release/b"}) || !cl.Push {
		t.Fatalf("depth: %q, push %v", bins, cl.Push)
	}

	// Cycles terminate.
	r = demoRepo(t, map[string]string{
		"loop.sh":  "./loop.sh\ncargo build --release\n",
		"Makefile": "a:\n\t$(MAKE) b\nb:\n\t$(MAKE) a\n\tcargo build --release\n",
	})
	if cl := one(t, r, "./loop.sh"); len(cl.Builds) != 1 || cl.Push {
		t.Fatalf("cycle: %+v", cl)
	}
	if cl := one(t, r, "make a"); len(gateable(cl)) != 1 || cl.Push {
		t.Fatalf("make cycle: %+v", cl)
	}

	// Calls that cannot be followed (outside the repository, missing, unknown directory, a
	// variable path) count as pushes: the gate then goes before them.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.sh"), []byte("cargo build --release\ndocker push x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = repo(t, map[string]string{"sub/x": ""})
	if err := os.Symlink(filepath.Join(outside, "outside.sh"), filepath.Join(r.Root, "link.sh")); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"../outside.sh", "./missing.sh", "make -C nowhere", "just", "bash link.sh", "make -f ../Makefile",
		"/opt/ci/deploy.sh", "bash ~/deploy.sh", `"$(dirname "$0")"/build.sh`, "$TOOLS/deploy", "make -C $DIR",
		"just -f $JUSTFILE", "source ./missing.sh",
	} {
		if cl := one(t, r, s); len(cl.Builds) != 0 || !cl.Push {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	for _, s := range []string{`cd "$(dirname "$0")/.." && ./build.sh`, "cd $X && make release"} {
		if _, cls := classifyAll(t, r, s); len(cls[1].Builds) != 0 || !cls[1].Push {
			t.Errorf("%q from an unknown directory: %+v", s, cls[1])
		}
	}
	// Not file calls: tools outside the repository, stdin scripts, and modes that run no recipe.
	for _, s := range []string{
		"just --list", "make --version", "curl -fsSL https://x/install.sh | sh", "~/.cargo/bin/cargo test",
		"/usr/bin/env true", "$HOME/.local/bin/tool --version", "source ~/.cargo/env", `. "$HOME/.cargo/env"`,
		"bash", "sh -s -- --yes",
	} {
		_, cls := classifyAll(t, r, s)
		if c := cls[len(cls)-1]; c.Push || len(c.Builds) != 0 {
			t.Errorf("%q: %+v", s, c)
		}
	}
	// In a Makefile: $(MAKE) -C $(DIR) cannot be followed.
	r = demoRepo(t, map[string]string{"Makefile": "release:\n\tcargo build --release\n\t$(MAKE) -C $(DIR) package\n"})
	if cl := one(t, r, "make release"); !cl.Push || len(gateable(cl)) != 1 {
		t.Fatalf("$(MAKE) -C $(DIR): %+v", cl)
	}
}

// I4: relative paths with a slash, sourced files and clone-directory paths are followed.
func TestMoreFileCalls(t *testing.T) {
	r := demoRepo(t, map[string]string{
		"script/cibuild":     "#!/bin/bash\ncargo build --release\n",
		"bin/deploy":         "#!/usr/bin/env sh\nfly deploy\n",
		"ci/build":           "cargo build --release\n",
		"scripts/env.sh":     "export X=1\n",
		"scripts/release.sh": "cargo build --release\ngh release create v1\n",
		"scripts/build.sh":   "cargo build --release\n",
	})
	for s, want := range map[string]struct {
		builds int
		push   bool
	}{
		"script/cibuild":                          {1, false},
		"bin/deploy":                              {0, true},
		"ci/build":                                {1, false},
		"source scripts/env.sh":                   {0, false},
		". scripts/release.sh":                    {1, true},
		"source ./scripts/build.sh":               {1, false},
		"$GITHUB_WORKSPACE/scripts/build.sh":      {1, false},
		`bash "$CI_PROJECT_DIR/scripts/build.sh"`: {1, false},
		"${BITBUCKET_CLONE_DIR}/bin/deploy":       {0, true},
	} {
		cl := one(t, r, s)
		if len(cl.Builds) != want.builds || cl.Push != want.push {
			t.Errorf("%q: %+v, want %d builds, push %v", s, cl, want.builds, want.push)
		}
	}
	_, cls := classifyAll(t, r, "cd $X && $GITHUB_WORKSPACE/scripts/build.sh")
	if len(cls[1].Builds) != 1 || cls[1].Builds[0].Via != "scripts/build.sh" {
		t.Fatalf("a clone-directory path works from an unknown directory: %+v", cls[1])
	}
}

// M4, M5: what make or just runs is not always visible; it fails closed.
func TestMakeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, mk, cmd string
		gateable      int
		push          bool
	}{
		{"a variable target resolves to its name",
			"BIN = target/release/demo\nrelease: $(BIN)\n$(BIN):\n\tcargo build --release\n", "make release", 1, false},
		{"a variable prerequisite that resolves is followed",
			"TARGETS = build\nrelease: $(TARGETS)\nbuild:\n\tcargo build --release\ndeploy:\n\tdocker push x\n", "make release", 1, false},
		{"a variable prerequisite that does not resolve",
			"release: $(EXTRA)\n\tcargo build --release\n", "make release", 1, true},
		{"a named goal without a rule (an include may define it)",
			"include deploy.mk\nbuild:\n\tcargo build --release\n", "make build deploy", 1, true},
		{"a named goal without a rule",
			"build:\n\tcargo build --release\n", "make release", 0, true},
		{"no default goal, with an include",
			"include rules.mk\n", "make", 0, true},
		{"endef with a comment closes define",
			"define HELP # usage\nrelease:\n\tdocker push x\nendef # end\nrelease:\n\tcargo build --release\n", "make release", 1, false},
	} {
		r := demoRepo(t, map[string]string{"Makefile": tc.mk})
		cl := one(t, r, tc.cmd)
		if len(gateable(cl)) != tc.gateable || cl.Push != tc.push {
			t.Errorf("%s: %+v", tc.name, cl)
		}
	}
	for _, tc := range []struct{ just, cmd string }{
		{"import 'deploy.just'\nbuild:\n    cargo build --release\n", "just deploy"},
		{"build: deploy\n    cargo build --release\n", "just build"},
		{"deploy:\n    #!/usr/bin/env python3\n    print('x')\n", "just deploy"},
	} {
		r := demoRepo(t, map[string]string{"justfile": tc.just})
		if cl := one(t, r, tc.cmd); !cl.Push {
			t.Errorf("%q with %q: %+v", tc.cmd, tc.just, cl)
		}
	}
}

func TestDockerBuildsInCalledFilesAreNotRecognized(t *testing.T) {
	r := repo(t, map[string]string{"build.sh": "docker build -t x .\n"})
	if cl := one(t, r, "./build.sh"); len(cl.Builds) != 0 {
		t.Fatalf("%+v", cl)
	}
}

// I1: wrappers such as time and nohup do not hide pushes or builds.
func TestWrappedCommands(t *testing.T) {
	r := demoRepo(t, nil)
	for _, s := range []string{"time docker push x", "nohup kubectl apply -f k8s", "timeout 10m helm upgrade x y", "command docker push x"} {
		if cl := one(t, r, s); !cl.Push {
			t.Errorf("%q is not a push", s)
		}
	}
	script := "time cargo build --release"
	cl := one(t, r, script)
	if len(cl.Builds) != 1 || cl.Builds[0].Kind != plan.BuildBinary || !slices.Equal(cl.Builds[0].Paths, []string{"target/release/demo"}) {
		t.Fatalf("%q: %+v", script, cl)
	}
	prep, _ := ecosystems[1].Prepare(r, plan.Project{Ecosystem: "rust", Dir: "."}, cl.Builds[0])
	if len(prep.Inserts) != 1 {
		t.Fatalf("Prepare = %+v", prep)
	}
	in := prep.Inserts[0]
	if got := script[:in.Offset] + in.Text + script[in.Offset:]; got != "time cargo auditable build --release" {
		t.Fatalf("rewritten: %q", got)
	}
}

// within fails the test when f does not return in time, instead of hanging it.
func within(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not return")
	}
}

// I5: a directory that is absolute or outside the repository is unknown, and nothing loops on it.
func TestAbsoluteDirectories(t *testing.T) {
	r := demoRepo(t, map[string]string{"justfile": "release:\n    cargo build --release\n"})
	classify := newClassifier(r, ecosystems)
	for _, s := range []string{"just release", "cargo build --release", "npm run build", "make", "./build.sh", "docker build -t x ."} {
		cmds, _ := shell.Parse(s, shell.State{Dir: "."})
		for _, dir := range []string{"/srv/app", "/", "../x"} {
			var cl plan.Classified
			within(t, func() { cl = classify(cmds[0], plan.Context{Dir: dir}) })
			for _, b := range cl.Builds {
				if b.Dir != "" {
					t.Errorf("%q from %q: build dir %q", s, dir, b.Dir)
				}
			}
		}
	}
	cmds, _ := shell.Parse("cargo build --release", shell.State{Dir: "."})
	var b *plan.Build
	within(t, func() { b = ecosystems[1].Recognize(r, cmds[0], plan.Context{Dir: "/srv/app"}) })
	if b == nil || b.Dir != "" || b.Paths != nil {
		t.Fatalf("rust from /srv/app: %+v", b)
	}
	cl := newClassifier(r, ecosystems)(cmds[0], plan.Context{Dir: "./"})
	if len(cl.Builds) != 1 || cl.Builds[0].Dir != "." || cl.Builds[0].Paths == nil {
		t.Fatalf("a clean relative directory still works: %+v", cl)
	}
}

// C1: an image build that pushes inside a called file makes the calling command a push.
func TestImagePushInCalledFile(t *testing.T) {
	mk := "IMAGE ?= ghcr.io/acme/demo\n\nrelease:\n\tcargo build --release\n\tdocker buildx build --platform linux/amd64,linux/arm64 --push -t $(IMAGE) .\n"
	r := demoRepo(t, map[string]string{"Makefile": mk})
	cl := one(t, r, "make release")
	g := gateable(cl)
	if !cl.Push || len(g) != 1 || g[0].Kind != plan.BuildBinary || len(g[0].Notes) != 1 || g[0].Notes[0].Code != "BUILD_AFTER_PUSH" {
		t.Fatalf("make release: %+v", cl)
	}
	for _, line := range []string{
		"docker build --push -t x .", "docker buildx build -o type=registry -t x .",
		"docker buildx build --output type=image,push=true -t x .", "$(DOCKER) buildx build --push -t x .",
	} {
		r := demoRepo(t, map[string]string{"Makefile": "all:\n\t" + line + "\n"})
		if cl := one(t, r, "make"); !cl.Push || len(cl.Builds) != 0 {
			t.Errorf("%q in a Makefile: %+v", line, cl)
		}
	}
	r = demoRepo(t, map[string]string{"build.sh": "docker build -t x .\ndocker buildx build --load -t y .\n"})
	if cl := one(t, r, "./build.sh"); cl.Push || len(cl.Builds) != 0 {
		t.Fatalf("image builds that do not push: %+v", cl)
	}
}

// I2: a push inside bash|sh|zsh -c '...' is seen; builds inside stay unrecognized.
func TestInlineShellPush(t *testing.T) {
	r := demoRepo(t, map[string]string{"Makefile": "deploy:\n\tkubectl apply -f k8s\n"})
	for _, s := range []string{
		`bash -c "docker push x"`, `sh -ec 'cd web && npm publish'`, `zsh -c 'time docker push "$IMAGE"'`,
		`bash -c "bash -c 'docker push x'"`, `sh -c 'make deploy'`, `bash -c 'docker buildx build --push -t x .'`,
	} {
		if cl := one(t, r, s); !cl.Push || len(cl.Builds) != 0 {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	for _, s := range []string{`bash -c "cargo build --release"`, `sh -c 'echo docker push'`, `bash -c`} {
		if cl := one(t, r, s); cl.Push || len(cl.Builds) != 0 {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	r = demoRepo(t, map[string]string{"Makefile": "release:\n\tcargo build --release\n\tbash -c 'docker push $(IMAGE)'\n"})
	if cl := one(t, r, "make release"); !cl.Push || len(gateable(cl)) != 1 {
		t.Fatalf("inline push in a Makefile: %+v", cl)
	}
}

// I3: tools named through a variable still push.
func TestVariablePrograms(t *testing.T) {
	r := demoRepo(t, nil)
	for _, s := range []string{"$(DOCKER) push x", "${KUBECTL} apply -f k8s", "$HELM upgrade a b", `"$DOCKER" push x`} {
		if cl := one(t, r, s); !cl.Push {
			t.Errorf("%q is not a push", s)
		}
	}
	if cl := one(t, r, "$(DOCKER) build -t x ."); len(cl.Builds) != 1 || cl.Builds[0].Kind != plan.BuildImage {
		t.Fatalf("$(DOCKER) build: %+v", cl)
	}
	if cl := one(t, r, "$(LINT) apply"); cl.Push {
		t.Fatalf("an unknown variable program is a push: %+v", cl)
	}
	mk := "KUBE ?= kubectl\nCONTAINER := podman\n\ndeploy:\n\t$(KUBE) apply -f k8s\npush:\n\t${CONTAINER} push x\n"
	r = demoRepo(t, map[string]string{"Makefile": mk})
	for _, s := range []string{"make deploy", "make push"} {
		if cl := one(t, r, s); !cl.Push {
			t.Errorf("%q: a Makefile variable naming a pushing tool is not resolved: %+v", s, cl)
		}
	}
}

// I6: a push two calls down still reaches the CI command.
func TestNestedCallPush(t *testing.T) {
	r := demoRepo(t, map[string]string{
		"scripts/release.sh": "#!/bin/sh\nmake release\n",
		"Makefile":           "release:\n\tcargo build --release\n\tgh release upload v1 target/release/demo\n",
	})
	cl := one(t, r, "./scripts/release.sh")
	g := gateable(cl)
	if !cl.Push || len(g) != 1 || g[0].Via != "Makefile" || len(g[0].Notes) != 1 || !strings.Contains(g[0].Notes[0].Message, "scripts/release.sh") {
		t.Fatalf("%+v", cl)
	}
}

// M2, M3, I6: buildx --check/--call, push= values, and unknown output types.
func TestBuildxModesAndOutputs(t *testing.T) {
	for _, s := range []string{
		"docker build --check .", "docker buildx build --call=outline .", "docker buildx build --call targets -t x .",
		"docker buildx build --call=check,error=true .",
	} {
		if b := image(t, s); b.Kind != plan.BuildOther || b.Image != nil {
			t.Errorf("%q: %+v", s, b)
		}
	}
	for _, s := range []string{"docker buildx build --call=build -t x .", "docker buildx build --call build -t x ."} {
		if b := image(t, s); b.Kind != plan.BuildImage || !b.Image.Gateable() {
			t.Errorf("%q: %+v", s, b)
		}
	}
	for _, s := range []string{
		"docker buildx build -o type=image,push=1 .", "docker buildx build -o type=image,push=TRUE .",
		"docker buildx build -o type=image,push=yes .", "docker buildx build -o type=foo .",
		"docker buildx build --push=maybe -t x .",
	} {
		b := image(t, s)
		if b.Kind != plan.BuildImage || b.Image == nil || b.Image.Gateable() && !b.Image.Push {
			t.Errorf("%q: %+v, image %+v", s, b, b.Image)
		}
	}
	if b := image(t, "docker buildx build -o type=foo ."); !b.Image.OutputPush {
		t.Fatalf("unknown output type: %+v", b.Image)
	}
	for _, s := range []string{"docker buildx build -o type=image,push=false .", "docker buildx build -o type=image,push=0 ."} {
		if b := image(t, s); b.Kind != plan.BuildOther {
			t.Errorf("%q: %+v", s, b)
		}
	}
}

func TestJustImportsAndRecipeNames(t *testing.T) {
	r := demoRepo(t, map[string]string{"justfile": "importer:\n    cargo build --release\nmod? extra\n"})
	if cl := one(t, r, "just importer"); len(gateable(cl)) != 1 || cl.Push {
		t.Fatalf("a recipe whose name starts with import: %+v", cl)
	}
	if cl := one(t, r, "just extra::deploy"); !cl.Push {
		t.Fatalf("a recipe from a module: %+v", cl)
	}
}

// M1: a `\` continuation in a CRLF file joins the lines, and offsets stay those of the file.
func TestCRLFContinuations(t *testing.T) {
	mk := "all:\r\n\tcargo build \\\r\n\t  --release\r\n"
	r := demoRepo(t, map[string]string{"Makefile": mk, "build.sh": "cargo build \\\r\n  --release\r\ndocker \\\r\n  push x\r\n"})
	cl := one(t, r, "make")
	g := gateable(cl)
	if len(g) != 1 || mk[g[0].Cmd.Start:g[0].Cmd.End] != "cargo build \\\r\n\t  --release" {
		t.Fatalf("make: %+v", cl)
	}
	for _, w := range g[0].Cmd.Words {
		if mk[w.Start:w.Start+len(w.Raw)] != w.Raw {
			t.Fatalf("word %+v does not match the file", w)
		}
	}
	if cl := one(t, r, "./build.sh"); len(gateable(cl)) != 1 || !cl.Push {
		t.Fatalf("script: %+v", cl)
	}
}

// I6: make's @, - and + recipe prefixes are removed.
func TestMakeRecipePrefixes(t *testing.T) {
	r := demoRepo(t, map[string]string{"Makefile": "build:\n\t@cargo build --release\npush:\n\t-@docker push x\nboth:\n\t+-@ cargo build --release\n"})
	if cl := one(t, r, "make build"); len(gateable(cl)) != 1 || cl.Push {
		t.Fatalf("@: %+v", cl)
	}
	if cl := one(t, r, "make push"); !cl.Push {
		t.Fatalf("-@: %+v", cl)
	}
	if cl := one(t, r, "make both"); len(gateable(cl)) != 1 {
		t.Fatalf("+-@: %+v", cl)
	}
}

// M9: RewriteOnly builds carry no notes, not even BUILD_AFTER_PUSH.
func TestRewriteOnlyBuildsCarryNoNotes(t *testing.T) {
	mk := "release:\n\tcargo build --release\n\tdocker push x\nother:\n\tcargo build --release --target $(T)\nmaybe:\n\tif true; then cargo build --release; fi\n"
	r := demoRepo(t, map[string]string{"Makefile": mk})
	cl := one(t, r, "make release")
	ro := rewriteOnly(cl)
	if !cl.Push || len(ro) != 2 {
		t.Fatalf("%+v", cl)
	}
	for _, b := range ro {
		if len(b.Notes) != 0 {
			t.Errorf("RewriteOnly build has notes: %+v", b.Notes)
		}
	}
	if g := gateable(cl); len(g) != 1 || len(g[0].Notes) != 1 || g[0].Notes[0].Code != "BUILD_AFTER_PUSH" {
		t.Fatalf("gateable: %+v", g)
	}
}

// M9: a few more just, buildx and cargo forms.
func TestMoreForms(t *testing.T) {
	just := "@release +targets: build\n    -cargo build --release --bin extra\nbuild:\n    @cargo build --release\n"
	r := demoRepo(t, map[string]string{"justfile": just})
	if cl := one(t, r, "just release a b c"); len(gateable(cl)) != 2 || cl.Push {
		t.Fatalf("variadic recipe: %+v", cl)
	}
	b := image(t, "docker buildx --builder ci build -t y .")
	if b.Image.Tool != "buildx" || b.Image.Context != "." {
		t.Fatalf("buildx --builder: %+v", b.Image)
	}
	if b := image(t, "docker buildx build --push=false -t y ."); b.Image.Push {
		t.Fatalf("--push=false: %+v", b.Image)
	}
	cl := one(t, demoRepo(t, nil), "cargo +stable auditable build --release")
	prep, _ := ecosystems[1].Prepare(r, plan.Project{Ecosystem: "rust", Dir: "."}, cl.Builds[0])
	if len(cl.Builds) != 1 || len(prep.Inserts) != 0 || prep.Install == nil {
		t.Fatalf("auditable build: %+v, %+v", cl, prep)
	}
}

// Re-review 1: a Makefile variable whose value carries options or a wrapper still pushes.
func TestVariableProgramsWithOptions(t *testing.T) {
	for _, def := range []string{
		"KUBECTL := kubectl -n prod\nPUSH = $(KUBECTL) apply -f k8s\n", "DOCKER := sudo docker\nPUSH = $(DOCKER) push x\n",
		"DOCKER := time docker --context ci\nPUSH = $(DOCKER) push x\n", "DOCKER := dkr\nPUSH = $(DOCKER) push x\n",
		"DOCKER_PUSH := docker push\nPUSH = $(DOCKER_PUSH) x\n",
	} {
		lines := strings.SplitN(def, "\n", 3)
		push := strings.TrimPrefix(lines[1], "PUSH = ")
		mk := lines[0] + "\nrelease:\n\tcargo build --release\n\t" + push + "\n"
		r := demoRepo(t, map[string]string{"Makefile": mk})
		if cl := one(t, r, "make release"); !cl.Push || len(gateable(cl)) != 1 {
			t.Errorf("%q: %+v", mk, cl)
		}
	}
	r := demoRepo(t, map[string]string{"Makefile": "DOCKER := sudo docker\nrelease:\n\t$(DOCKER) buildx build --push -t x .\n"})
	if cl := one(t, r, "make release"); !cl.Push {
		t.Fatalf("image push through a wrapped variable: %+v", cl)
	}
}

// Re-review 2: inline scripts in a Makefile see its variables.
func TestInlinePushSeesMakefileVariables(t *testing.T) {
	r := demoRepo(t, map[string]string{"Makefile": "CONTAINER_TOOL ?= podman\nrelease:\n\tcargo build --release\n\tsh -c '$(CONTAINER_TOOL) push x'\n"})
	if cl := one(t, r, "make release"); !cl.Push || len(gateable(cl)) != 1 {
		t.Fatalf("%+v", cl)
	}
}

// Re-review 3: rules with a variable target are matched by their resolved name; an unresolved
// one is only maybe reachable.
func TestMakeVariableTargets(t *testing.T) {
	mk := "BIN := target/release/demo\n\ntest:\n\tcargo test\n\nrelease: $(BIN)\n\n$(BIN):\n\tcargo build --release\n"
	r := demoRepo(t, map[string]string{"Makefile": mk})
	if cl := one(t, r, "make test"); len(gateable(cl)) != 0 || len(rewriteOnly(cl)) != 1 || cl.Push {
		t.Fatalf("make test: %+v", cl)
	}
	if cl := one(t, r, "make release"); len(gateable(cl)) != 1 || cl.Push {
		t.Fatalf("make release: %+v", cl)
	}
	if cl := one(t, r, "make target/release/demo"); len(gateable(cl)) != 1 || cl.Push {
		t.Fatalf("make $(BIN) by name: %+v", cl)
	}
	r = demoRepo(t, map[string]string{"Makefile": "BIN := target/release/demo\nSRCS := $(wildcard src/*.rs)\ntest:\n\tcargo test\n$(BIN): $(SRCS)\n\tcargo build --release\n"})
	if cl := one(t, r, "make test"); cl.Push || len(gateable(cl)) != 0 {
		t.Fatalf("make test with a wildcard prerequisite elsewhere: %+v", cl)
	}

	// An unresolved variable target: its builds stay RewriteOnly, its pushes count.
	r = demoRepo(t, map[string]string{"Makefile": "test:\n\tcargo test\n$(OUT)/demo: $(SRCS)\n\tcargo build --release\n"})
	if cl := one(t, r, "make test"); cl.Push || len(gateable(cl)) != 0 || len(rewriteOnly(cl)) != 1 {
		t.Fatalf("maybe reachable build: %+v", cl)
	}
	r = demoRepo(t, map[string]string{"Makefile": "test:\n\tcargo test\n$(DEPLOY):\n\tdocker push x\n\t$(TOOLS)/upload\n"})
	if cl := one(t, r, "make test"); !cl.Push {
		t.Fatalf("maybe reachable push: %+v", cl)
	}
	// The first rule is the default goal even when its target does not resolve.
	r = demoRepo(t, map[string]string{"Makefile": "$(OUT)/demo:\n\tcargo build --release\ntest:\n\tcargo test\n"})
	if cl := one(t, r, "make"); len(gateable(cl)) != 1 || cl.Push {
		t.Fatalf("unresolved default goal: %+v", cl)
	}
}

// Re-review 4: running a program the job built is not a file call.
func TestBuiltProgramsAreNotCalls(t *testing.T) {
	r := demoRepo(t, map[string]string{
		"Makefile":            "release:\n\tcargo build --release\n\t./target/release/demo --version\n",
		"dist/committed":      "#!/bin/sh\ndocker push x\n",
		"crates/x/Cargo.toml": "",
	})
	cl := one(t, r, "make release")
	if g := gateable(cl); cl.Push || len(g) != 1 || len(g[0].Notes) != 0 {
		t.Fatalf("smoke test after the build: %+v", cl)
	}
	for _, s := range []string{"./node_modules/.bin/eslint .", "target/release/demo", "out/app", "build/tool --help"} {
		if cl := one(t, r, s); cl.Push {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	_, cls := classifyAll(t, r, "cd crates/x && ../../target/release/demo")
	if cls[1].Push {
		t.Fatalf("a built program from a subdirectory: %+v", cls[1])
	}
	if cl := one(t, r, "./dist/committed"); !cl.Push {
		t.Fatalf("a committed file in an output directory is followed: %+v", cl)
	}
	if cl := one(t, r, "./tools/missing"); !cl.Push {
		t.Fatalf("a missing file elsewhere still counts as a push: %+v", cl)
	}
}

// Re-review 5: environment scripts sourced through their home variable, and $(dirname "$0").
func TestEnvironmentScriptsAndScriptDirectory(t *testing.T) {
	r := demoRepo(t, map[string]string{
		"scripts/lib.sh":   "export X=1\n",
		"scripts/push.sh":  "docker push x\n",
		"scripts/a.sh":     "source \"$(dirname \"$0\")/lib.sh\"\ncargo build --release\n",
		"scripts/b.sh":     ". $(dirname ${BASH_SOURCE[0]})/lib.sh\ncargo build --release\n",
		"scripts/c.sh":     "cd \"$(dirname \"$0\")/..\"\ncargo build --release\n./scripts/push.sh\n",
		"scripts/d.sh":     "\"$(dirname \"$0\")\"/lib.sh\ncargo build --release\n",
		"scripts/sub/e.sh": "cd \"$(dirname -- \"${BASH_SOURCE[0]}\")\" && cargo build --release\n",
	})
	for _, s := range []string{
		`. "$NVM_DIR/nvm.sh"`, `source "$VIRTUAL_ENV/bin/activate"`, `source ${CARGO_HOME}/env`, `. $HOME/.profile`,
		`source "$CONDA_PREFIX/etc/profile.d/conda.sh"`,
	} {
		if cl := one(t, r, s); cl.Push || len(cl.Builds) != 0 {
			t.Errorf("%q: %+v", s, cl)
		}
	}
	for _, s := range []string{"./scripts/a.sh", "bash scripts/b.sh", "scripts/d.sh"} {
		if cl := one(t, r, s); cl.Push || len(gateable(cl)) != 1 {
			t.Errorf("%q does not follow $(dirname \"$0\"): %+v", s, cl)
		}
	}
	cl := one(t, r, "./scripts/c.sh")
	if g := gateable(cl); !cl.Push || len(g) != 1 || g[0].Dir != "." || g[0].Paths == nil {
		t.Fatalf("cd \"$(dirname \"$0\")/..\": %+v", cl)
	}
	if g := gateable(one(t, r, "bash scripts/sub/e.sh")); len(g) != 1 || g[0].Dir != "scripts/sub" {
		t.Fatalf("cd to the script's directory: %+v", g)
	}
}

// Re-review 7: more cases.
func TestReReviewGaps(t *testing.T) {
	r := demoRepo(t, map[string]string{"Makefile": "release:\n\tcargo build --release\n", "justfile": "build:\n    cargo build --release\n"})
	if cl := one(t, r, "bash -o pipefail -c 'docker push x'"); !cl.Push {
		t.Fatalf("bash -o pipefail -c: %+v", cl)
	}
	for _, s := range []string{"cd $X && make release", "cd $X && just"} {
		if _, cls := classifyAll(t, r, s); !cls[1].Push || len(cls[1].Builds) != 0 {
			t.Errorf("%q: %+v", s, cls[1])
		}
	}
	r = demoRepo(t, map[string]string{"justfile": "import 'build.just'\nimport? 'local.just'\n"})
	if cl := one(t, r, "just"); !cl.Push {
		t.Fatalf("a justfile with only imports: %+v", cl)
	}
}

// Batch 2 re-review follow-up: a maybe-reachable rule (its target does not resolve) also
// follows its literal prerequisites in the same push-only mode; its $ prerequisites stay ignored.
func TestMaybeReachableLiteralPrerequisites(t *testing.T) {
	mk := "BIN = $(shell echo app)\n\nrelease: app\n\tcargo build --release\n\n$(BIN): push-image\n\npush-image:\n\tdocker push x\n"
	r := demoRepo(t, map[string]string{"Makefile": mk})
	cl := one(t, r, "make release")
	if !cl.Push {
		t.Fatalf("make release: a push reached through a maybe-reachable rule's prerequisite: %+v", cl)
	}
	if g := gateable(cl); len(g) != 1 || !slices.ContainsFunc(g[0].Notes, func(n plan.Note) bool { return n.Code == "BUILD_AFTER_PUSH" }) {
		t.Fatalf("the release build counts as after the push: %+v", cl)
	}

	// Builds in rules reached that way stay RewriteOnly.
	mk = "test:\n\tcargo test\n$(OUT)/demo: compile\ncompile:\n\tcargo build --release\n"
	r = demoRepo(t, map[string]string{"Makefile": mk})
	if cl := one(t, r, "make test"); cl.Push || len(gateable(cl)) != 0 || len(rewriteOnly(cl)) != 1 {
		t.Fatalf("a build behind a maybe-reachable rule: %+v", cl)
	}
	// Transitively: the prerequisite's own literal prerequisites are followed too.
	mk = "test:\n\tcargo test\n$(OUT)/demo: a\na: b\nb:\n\tdocker push x\n"
	r = demoRepo(t, map[string]string{"Makefile": mk})
	if cl := one(t, r, "make test"); !cl.Push {
		t.Fatalf("a push two prerequisites behind a maybe-reachable rule: %+v", cl)
	}
	// $ prerequisites of a maybe-reachable rule stay ignored: no push for an unresolved one.
	mk = "test:\n\tcargo test\n$(OUT)/demo: $(SRCS) compile\ncompile:\n\tcargo build --release\n"
	r = demoRepo(t, map[string]string{"Makefile": mk})
	if cl := one(t, r, "make test"); cl.Push {
		t.Fatalf("an unresolved $ prerequisite of a maybe-reachable rule counts as a push: %+v", cl)
	}
	// A rule that is actually reached keeps its full mode, even when a maybe rule names it.
	mk = "release: compile\n$(OUT)/demo: compile\ncompile:\n\tcargo build --release\n"
	r = demoRepo(t, map[string]string{"Makefile": mk})
	if cl := one(t, r, "make release"); cl.Push || len(gateable(cl)) != 1 {
		t.Fatalf("a reached rule that a maybe rule also names: %+v", cl)
	}
}

// The built-program rule never applies to .sh paths: a missing script in an output directory is
// an unfollowable call, so it counts as a push.
func TestMissingScriptInOutputDirectoryIsPush(t *testing.T) {
	r := demoRepo(t, nil)
	for _, s := range []string{"./dist/deploy.sh", "dist/deploy.sh", "bash dist/deploy.sh", "sh ./build/release.sh"} {
		if cl := one(t, r, s); !cl.Push {
			t.Errorf("%q: %+v", s, cl)
		}
	}
}

// Review 5: a maybe-reachable rule follows its $ prerequisites that resolve; unresolved ones stay
// ignored.
func TestMaybeReachableResolvedVariablePrerequisites(t *testing.T) {
	mk := "BIN = $(shell echo app)\nDEPLOY = push-image\n\nrelease: app\n\tcargo build --release\n\n$(BIN): $(DEPLOY)\n\npush-image:\n\tdocker push x\n"
	r := demoRepo(t, map[string]string{"Makefile": mk})
	if cl := one(t, r, "make release"); !cl.Push {
		t.Fatalf("a push behind a resolved $ prerequisite of a maybe-reachable rule: %+v", cl)
	}
	mk = "BIN = $(shell echo app)\n\nrelease: app\n\tcargo build --release\n\n$(BIN): $(DEPLOY)\n\npush-image:\n\tdocker push x\n"
	r = demoRepo(t, map[string]string{"Makefile": mk})
	if cl := one(t, r, "make release"); cl.Push {
		t.Fatalf("an unresolved $ prerequisite of a maybe-reachable rule counts: %+v", cl)
	}
}
