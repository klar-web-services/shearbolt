package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/testutil"
)

const buildConfig = "version: 1\nproject: web\ntargets:\n  - name: web\n    image: ${SHEARBOLT_IMAGE:-web:local}\n  - name: source\n    source: .\n"

// buildProject writes a project with the given shearbolt.yaml, a lockfile and, when dockerfile
// is set, a Dockerfile. It returns the config's path.
func buildProject(t *testing.T, yaml string, dockerfile bool) string {
	t.Helper()
	dir := projectDir(t)
	if dockerfile {
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, "shearbolt.yaml")
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

type buildCall struct{ ref, dir string }

// recordBuilds makes d's builds succeed and record their arguments, and fails the test when a
// build starts after Grype has already run.
func recordBuilds(t *testing.T, d *deps, grypeCalls string) *[]buildCall {
	t.Helper()
	var calls []buildCall
	d.buildImage = func(_ context.Context, ref, dir string, out io.Writer) error {
		if _, err := os.Stat(grypeCalls); err == nil {
			t.Errorf("build of %s started after Grype ran", ref)
		}
		calls = append(calls, buildCall{ref, dir})
		return nil
	}
	return &calls
}

func TestScanBuildBuildsTheImageTargetBeforeScanning(t *testing.T) {
	cfg := buildProject(t, buildConfig, true)
	grypeCalls := filepath.Join(t.TempDir(), "calls")
	d := testDeps(t, map[string]string{"FAKE_GRYPE_CALLS": grypeCalls, "FAKE_GRYPE_JSON": testutil.GrypeJSON(time.Now().UTC())})
	builds := recordBuilds(t, &d, grypeCalls)
	code, out, errb := scanCmd(t, d, "--config", cfg, "--build", "--json", "--output-dir", evidence(t))
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	want := buildCall{"web:local", filepath.Dir(cfg)}
	if len(*builds) != 1 || (*builds)[0] != want {
		t.Fatalf("builds = %+v, want [%+v]", *builds, want)
	}
	if !strings.Contains(errb, "docker build --tag=web:local") {
		t.Fatalf("stderr does not say what is built:\n%s", errb)
	}
	data, err := os.ReadFile(grypeCalls)
	if err != nil || !strings.Contains(string(data), "web:local") {
		t.Fatalf("the built image was not scanned (err %v):\n%s", err, data)
	}
}

func TestScanWithoutBuildNeverBuilds(t *testing.T) {
	cfg := buildProject(t, buildConfig, true)
	d := testDeps(t, map[string]string{"FAKE_GRYPE_JSON": testutil.GrypeJSON(time.Now().UTC())})
	builds := recordBuilds(t, &d, filepath.Join(t.TempDir(), "calls"))
	scanCmd(t, d, "--config", cfg, "--output-dir", evidence(t))
	if len(*builds) != 0 {
		t.Fatalf("builds = %+v without --build", *builds)
	}
}

// Every rule --build checks stops the run before anything is built or scanned.
func TestScanBuildRefusals(t *testing.T) {
	two := "version: 1\nproject: web\ntargets:\n  - name: web\n    image: web:local\n  - name: worker\n    image: worker:local\n"
	none := "version: 1\nproject: web\ntargets:\n  - name: source\n    source: .\n"
	for _, tc := range []struct {
		name       string
		yaml       string
		dockerfile bool
		vars       map[string]string
		args       []string
		want       string
	}{
		{name: "CI mode", yaml: buildConfig, dockerfile: true, vars: map[string]string{"CI": "true"}, want: "In CI"},
		{name: "--ci", yaml: buildConfig, dockerfile: true, args: []string{"--ci"}, want: "In CI"},
		{name: "ad hoc", args: []string{"--image", "web:local"}, want: "ad-hoc"},
		{name: "no Dockerfile", yaml: buildConfig, want: "Dockerfile"},
		{name: "two image targets", yaml: two, dockerfile: true, want: "exactly one image target"},
		{name: "no image target", yaml: none, dockerfile: true, want: "no image target"},
		{name: "registry tag", yaml: buildConfig, dockerfile: true, vars: map[string]string{"SHEARBOLT_IMAGE": "ghcr.io/acme/web:1"}, want: ":local"},
		{name: "--target leaves it out", yaml: buildConfig, dockerfile: true, args: []string{"--target", "source"}, want: "--target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grypeCalls := filepath.Join(t.TempDir(), "calls")
			vars := map[string]string{"FAKE_GRYPE_CALLS": grypeCalls}
			for k, v := range tc.vars {
				vars[k] = v
			}
			d := testDeps(t, vars)
			builds := recordBuilds(t, &d, grypeCalls)
			args := append([]string{"--build", "--output-dir", evidence(t)}, tc.args...)
			if tc.yaml != "" {
				args = append(args, "--config", buildProject(t, tc.yaml, tc.dockerfile))
			}
			code, out, errb := scanCmd(t, d, args...)
			if code != 2 || !strings.Contains(errb, tc.want) {
				t.Fatalf("exit = %d, want 2 with %q\nstdout: %s\nstderr: %s", code, tc.want, out, errb)
			}
			if len(*builds) != 0 {
				t.Fatalf("built %+v", *builds)
			}
			if _, err := os.Stat(grypeCalls); err == nil {
				t.Fatal("Grype ran")
			}
		})
	}
}

func TestScanBuildFailureScansNothing(t *testing.T) {
	cfg := buildProject(t, buildConfig, true)
	grypeCalls := filepath.Join(t.TempDir(), "calls")
	d := testDeps(t, map[string]string{"FAKE_GRYPE_CALLS": grypeCalls})
	d.buildImage = func(context.Context, string, string, io.Writer) error { return errors.New("exit status 1") }
	ev := evidence(t)
	code, out, errb := scanCmd(t, d, "--config", cfg, "--build", "--output-dir", ev)
	if code != 2 || !strings.Contains(errb, "docker build failed") || !strings.Contains(errb, "nothing was scanned") {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if _, err := os.Stat(grypeCalls); err == nil {
		t.Fatal("Grype ran after a failed build")
	}
}

// dockerBuild runs the docker on PATH with the tag, the Dockerfile next to the config and the
// config's folder as the context, and passes on its exit status.
func TestDockerBuildCommand(t *testing.T) {
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\nexit \"${FAKE_DOCKER_STATUS:-0}\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	dir := t.TempDir()
	if err := dockerBuild(context.Background(), "web:local", dir, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "build\n--tag=web:local\n--file=" + filepath.Join(dir, "Dockerfile") + "\n--\n" + dir + "\n"
	if string(data) != want {
		t.Fatalf("docker args:\n%s\nwant:\n%s", data, want)
	}
	t.Setenv("FAKE_DOCKER_STATUS", "3")
	if err := dockerBuild(context.Background(), "web:local", dir, nil); err == nil {
		t.Fatal("a failed docker build returned no error")
	}
	t.Setenv("PATH", t.TempDir())
	if err := dockerBuild(context.Background(), "web:local", dir, nil); err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("err = %v without docker on PATH", err)
	}
}
