package report

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/model"
)

type Repin struct {
	Target, Image, File string
	Line                int
	Services            []string
	Count, Unfixed      int
}

func Repins(r *model.Run, blocked []model.Finding) []Repin {
	type key struct{ target, image string }
	idx := map[key]int{}
	var out []Repin
	for _, f := range blocked {
		if f.Image == "" {
			continue
		}
		k := key{f.Target, f.Image}
		i, seen := idx[k]
		if !seen {
			i = len(out)
			idx[k] = i
			p := Repin{Target: f.Target, Image: f.Image}
			for _, t := range r.Targets {
				if t.Name != f.Target {
					continue
				}
				p.File = t.Ref
				// One ref may appear under several platforms: the line is the first entry's,
				// the services come from all of them.
				found := false
				for _, im := range t.Images {
					if im.Ref != f.Image {
						continue
					}
					if !found {
						p.Line, found = im.Line, true
					}
					for _, sv := range im.Services {
						if !slices.Contains(p.Services, sv) {
							p.Services = append(p.Services, sv)
						}
					}
				}
				break
			}
			out = append(out, p)
		}
		out[i].Count++
		if f.FixState != "fixed" || len(f.FixVersions) == 0 {
			out[i].Unfixed++
		}
	}
	return out
}

var digestRe = regexp.MustCompile(`(@sha256:[0-9a-f]{12})[0-9a-f]+`)

func shortImage(ref string) string { return digestRe.ReplaceAllString(ref, "$1") }

func repinSentence(p Repin) string {
	n := plural(p.Count, "blocking vulnerability", "blocking vulnerabilities")
	switch {
	case p.Unfixed == 0 && p.Count == 1:
		return n + ", fixed upstream"
	case p.Unfixed == 0:
		return n + ", all fixed upstream"
	case p.Unfixed == p.Count && p.Count == 1:
		return n + " with no fix yet, so a newer image may not clear it"
	case p.Unfixed == p.Count:
		return n + " with no fix yet, so a newer image may not clear them"
	default:
		return fmt.Sprintf("%s, %d of them with no fix yet, so a newer image may not clear all of them", n, p.Unfixed)
	}
}

func repinMarkdown(p Repin) string {
	services := "service " + md(strings.Join(p.Services, ", "))
	if len(p.Services) > 1 {
		services = "services " + md(strings.Join(p.Services, ", "))
	}
	return fmt.Sprintf("- Re-pin %s (%s:%d, %s): %s. Use a newer build of this tag or a newer tag, then scan again.\n",
		md(shortImage(p.Image)), md(p.File), p.Line, services, md(repinSentence(p)))
}

func repinText(p Repin) string {
	return fmt.Sprintf("  re-pin %s (%s:%d): %s\n", clean(shortImage(p.Image)), clean(p.File), p.Line, clean(repinSentence(p)))
}

type skippedImage struct{ Target, Image, Note string }

func skippedImages(r *model.Run) []skippedImage {
	var out []skippedImage
	for _, t := range r.Targets {
		for _, im := range t.Images {
			if im.Status == "skipped" {
				out = append(out, skippedImage{t.Name, shortImage(im.Ref), im.Note})
			}
		}
	}
	return out
}
