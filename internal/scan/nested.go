package scan

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/grype"
	"github.com/klar-web-services/shearbolt/internal/model"
)

// Syft decides which yarn.lock and bun.lock packages are devDependencies from the package.json
// at the scan root, not the one next to the lockfile. Two cases follow, and mergeNested rescans
// each affected lockfile and replaces the root scan's view of it:
//   - A yarn workspace root (package.json with a workspaces field) loses every package reachable
//     from its devDependencies, even those a workspace member ships. Grype cannot tell which
//     workspace packages ship, so the lockfile is scanned on its own as a file source, which
//     checks every package, dev tooling included.
//   - An independent non-workspace yarn or bun project in a subfolder is rescanned as a dir
//     source, so its own package.json decides what is a devDependency.
//
// Any other lockfile, including a root-level non-workspace one, gets no extra scan.
func (s *state) mergeNested(ctx context.Context, i int, name, ref string, exclude, lockfiles []string, res *grype.Result, inv *grype.Inventory) bool {
	nested, workspaces := 0, 0
	for _, lf := range lockfiles {
		base := path.Base(lf)
		if base != "yarn.lock" && base != "bun.lock" {
			continue
		}
		dir := path.Dir(lf)
		abs, err := filepath.Abs(filepath.Join(ref, filepath.FromSlash(lf)))
		if err != nil {
			s.targetError(i, "SCAN_FAILED", fmt.Sprintf("%s: %v", lf, err))
			return false
		}
		var req grype.Request
		workspace := yarnWorkspaceLockfile(abs)
		switch {
		case workspace:
			workspaces++
			req = s.request(fmt.Sprintf("%s+workspace%d", name, workspaces))
			req.Source = "file:" + abs
		case dir != ".":
			nested++
			req = s.request(fmt.Sprintf("%s+nested%d", name, nested))
			req.Source = "dir:" + filepath.Dir(abs)
			req.Exclude = GrypeExcludes(underFolder(exclude, dir))
		default:
			continue
		}
		if err := s.opts.Scanner.Scan(ctx, req); err != nil {
			s.targetError(i, "SCAN_FAILED", fmt.Sprintf("%s: %v", lf, err))
			return false
		}
		sub, subInv, err := s.readOutputs(i, req)
		if err != nil {
			s.targetError(i, "BAD_OUTPUT", fmt.Sprintf("%s: %v", lf, err))
			return false
		}
		res.Findings = replaceLockfile(res.Findings, sub.Findings, dir, base)
		if inv.Locations == nil {
			inv.Locations = map[string]int{}
		}
		// Report what the extra scan checked instead of what the root scan saw for this lockfile.
		inv.Packages += subInv.Locations["/"+base] - inv.Locations["/"+lf]
		inv.Locations["/"+lf] = subInv.Locations["/"+base]
		if workspace {
			s.warn("YARN_WORKSPACE_DEV_INCLUDED", name, fmt.Sprintf("%s is a yarn workspace root; Grype cannot tell which workspace packages ship, so every package in the lockfile was checked, including dev tooling", lf))
		}
	}
	return true
}

// underFolder returns the exclude entries that lie under dir, relative to dir.
func underFolder(exclude []string, dir string) []string {
	var out []string
	for _, e := range exclude {
		if c := cleanExclude(e); strings.HasPrefix(c, dir+"/") {
			out = append(out, strings.TrimPrefix(c, dir+"/"))
		}
	}
	return out
}

// replaceLockfile removes /dir/base from the root findings, adds the sub-scan's findings for
// /base (relocated under /dir, or kept as is for dir "."), and merges duplicates.
func replaceLockfile(root, sub []model.Finding, dir, base string) []model.Finding {
	prefix := ""
	if dir != "." {
		prefix = "/" + dir
	}
	target := prefix + "/" + base
	var out []model.Finding
	for _, f := range root {
		had := slices.Contains(f.Locations, target)
		f.Locations = slices.DeleteFunc(slices.Clone(f.Locations), func(l string) bool { return l == target })
		if had && len(f.Locations) == 0 {
			continue
		}
		out = append(out, f)
	}
	for _, f := range sub {
		if !slices.Contains(f.Locations, "/"+base) {
			continue
		}
		locs := make([]string, len(f.Locations))
		for k, l := range f.Locations {
			locs[k] = prefix + l
		}
		f.Locations = locs
		out = append(out, f)
	}
	var merged []model.Finding
	index := map[string]int{}
	for _, f := range out {
		key := strings.Join([]string{f.ID, f.Package, f.Version, f.Type}, "\x00")
		if k, ok := index[key]; ok {
			for _, l := range f.Locations {
				if !slices.Contains(merged[k].Locations, l) {
					merged[k].Locations = append(merged[k].Locations, l)
				}
			}
			continue
		}
		index[key] = len(merged)
		merged = append(merged, f)
	}
	return merged
}
