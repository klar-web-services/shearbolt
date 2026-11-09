package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/compose"
	"github.com/klar-web-services/shearbolt/internal/config"
	"github.com/klar-web-services/shearbolt/internal/model"
)

// scanCompose scans every image a compose file pulls like an image target. The results nest
// under the target as images; a problem with one image fails the target but not the others.
func (s *state) scanCompose(ctx context.Context, i int, t config.Target) {
	file, err := filepath.Rel(s.opts.Config.Dir, t.Compose)
	if err != nil {
		file = t.Compose
	}
	file = filepath.ToSlash(file)
	s.run.Targets[i].Ref = file
	prefix := func(line int) string {
		if line > 0 {
			return fmt.Sprintf("%s:%d: ", file, line)
		}
		return file + ": "
	}
	f, err := compose.Read(t.Compose, compose.Env(s.opts.Env), t.Exclude)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		s.targetError(i, "COMPOSE_UNREADABLE", fmt.Sprintf("compose file %s cannot be read: %v", file, err))
		return
	}
	if f.Fatal != nil {
		s.targetError(i, f.Fatal.Code, prefix(f.Fatal.Line)+f.Fatal.Message)
		return
	}
	for _, service := range f.Builds {
		s.warn("COMPOSE_BUILD_NOT_SCANNED", t.Name, fmt.Sprintf("service %s in %s builds its image where it is deployed, so it is not scanned; an image CI builds is gated with an image target", service, file))
	}
	if len(f.NoImage) > 0 {
		s.warn("COMPOSE_SERVICE_NO_IMAGE", t.Name, fmt.Sprintf("%s sets no image for %s; their images are scanned where they are defined", file, strings.Join(f.NoImage, ", ")))
	}
	for _, p := range f.Missing {
		s.targetError(i, p.Code, prefix(p.Line)+p.Message)
	}
	if len(f.Images) == 0 {
		if len(f.Missing) == 0 {
			s.targetError(i, "COMPOSE_NO_IMAGES", fmt.Sprintf("%s pulls no image: every service builds its image, sets none or is excluded. Remove this target if the file pulls nothing.", file))
		}
		return
	}
	scannedAny := false
	for n, img := range f.Images {
		s.run.Targets[i].Images = append(s.run.Targets[i].Images, model.ImageResult{Ref: img.Ref, Platform: img.Platform, Services: img.Services, Line: img.Line, Status: "scanned"})
		if s.scanComposeImage(ctx, i, n, t.Name, img) {
			scannedAny = true
		}
	}
	if s.run.Targets[i].Status != "error" && !scannedAny {
		s.skip(i, fmt.Sprintf("every image in %s was skipped; each image's note says how to include it", file))
	}
}

// scanComposeImage scans image n (0-based) of target i and reports whether it was scanned.
func (s *state) scanComposeImage(ctx context.Context, i, n int, name string, img compose.Image) bool {
	res := &s.run.Targets[i].Images[n]
	req := s.request(fmt.Sprintf("%s+%d", name, n+1))
	req.Platform = img.Platform
	req.Source = s.imageSource(ctx, img.Ref)
	fail := func(code, msg string) {
		res.Status, res.Note = "error", msg
		s.targetError(i, code, msg)
	}
	if err := s.opts.Scanner.Scan(ctx, req); err != nil {
		if s.maySkipImage(ctx, req) {
			res.Status = "skipped"
			res.Note = fmt.Sprintf("image %s is not in your local Docker and could not be pulled (%s). Pull it with `docker pull %s` to include it.", img.Ref, firstLine(err.Error()), img.Ref)
			return false
		}
		msg := fmt.Sprintf("image %s: %v", img.Ref, err)
		if strings.HasPrefix(req.Source, "registry:") && ctx.Err() == nil {
			msg = strings.TrimRight(msg, ".") + ". If the image is in a private registry, log in to it before the shearbolt step."
		}
		fail("SCAN_FAILED", msg)
		return false
	}
	res.Raw = append(res.Raw, s.raw(req)...)
	out, inv, err := s.readOutputs(req)
	if err != nil {
		fail("BAD_OUTPUT", fmt.Sprintf("image %s: %v", img.Ref, err))
		return false
	}
	if inv.Packages == 0 {
		fail("NO_PACKAGES", fmt.Sprintf("Grype found no packages in image %s, so nothing could be checked", img.Ref))
		return false
	}
	res.Identity, res.Packages = out.Identity, inv.Packages
	s.run.Targets[i].Packages += inv.Packages
	for k := range out.Findings {
		out.Findings[k].Image = img.Ref
	}
	s.add(name, out.Findings)
	return true
}
