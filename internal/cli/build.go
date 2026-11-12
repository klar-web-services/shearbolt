package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/scan"
)

// buildTarget returns the image target `scan --build` builds from all of a config's targets
// (before --target) and the config's folder dir. It checks the rules that keep a local build from
// standing in for any other image: exactly one image target, kept by --target (only) when given,
// with a reference tagged :local, and a Dockerfile in dir.
func buildTarget(all []config.Target, dir string, only []string) (config.Target, error) {
	var images []config.Target
	for _, t := range all {
		if t.Kind() == "image" {
			images = append(images, t)
		}
	}
	switch len(images) {
	case 0:
		return config.Target{}, errors.New("--build builds the config's image target, and it has no image target")
	case 1:
	default:
		names := make([]string, len(images))
		for i, t := range images {
			names[i] = t.Name
		}
		return config.Target{}, fmt.Errorf("--build needs exactly one image target, so that it knows which image the Dockerfile builds; the config has %d (%s). Build them with `docker build` and scan without --build", len(images), strings.Join(names, ", "))
	}
	t := images[0]
	if len(only) > 0 && !slices.Contains(only, t.Name) {
		return config.Target{}, fmt.Errorf("--build builds image target %q, which --target leaves out", t.Name)
	}
	if !scan.LocalTag(t.Image) {
		return config.Target{}, fmt.Errorf("--build only builds an image tagged :local, so a local build never takes the place of a registry image; target %q scans %s", t.Name, t.Image)
	}
	dockerfile := filepath.Join(dir, "Dockerfile")
	if info, err := os.Stat(dockerfile); err != nil || !info.Mode().IsRegular() {
		return config.Target{}, fmt.Errorf("--build builds from the Dockerfile next to shearbolt.yaml, and %s does not exist", dockerfile)
	}
	return t, nil
}

// dockerBuild builds dir/Dockerfile with dir as the context and tags the image ref. Docker's
// output goes to out; an *os.File is handed to docker as is, so it shows its terminal progress.
func dockerBuild(ctx context.Context, ref, dir string, out io.Writer) error {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("docker is not installed or not on PATH")
	}
	cmd := exec.CommandContext(ctx, docker, "build", "--tag="+ref, "--file="+filepath.Join(dir, "Dockerfile"), "--", dir)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("interrupted")
		}
		return err
	}
	return nil
}
