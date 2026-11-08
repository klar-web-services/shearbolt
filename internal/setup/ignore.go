package setup

import (
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

// This file writes R7: .shearbolt/ (the scan's work and cache directory) in the root .gitignore
// and in the .dockerignore files of the directories Docker builds from.

// ignoreLines are the entries that already ignore .shearbolt/ at every depth below the file's
// directory. An anchored /.shearbolt covers only the top one, not nested projects' directories.
var ignoreLines = []string{".shearbolt", ".shearbolt/"}

// ignoreChanges returns the R7 changes, sorted by path: the root .gitignore, and the
// .dockerignore of every selected project dir that holds a Dockerfile or Containerfile and of
// the context dir of every gated docker, buildx or build-push-action build (railpack reads no
// .dockerignore). A context that is unknown or not a directory in the repository is skipped, so
// setup never creates directories.
func ignoreChanges(r plan.Repo, selected []plan.Project, gated []plan.Build) []plan.FileChange {
	files := []string{".gitignore"}
	for _, p := range selected {
		if r.Exists(path.Join(p.Dir, "Dockerfile")) || r.Exists(path.Join(p.Dir, "Containerfile")) {
			files = append(files, path.Join(p.Dir, ".dockerignore"))
		}
	}
	for _, b := range gated {
		if b.Kind != plan.BuildImage || b.Image == nil || b.Image.Tool == "railpack" || b.Image.Context == "" {
			continue
		}
		if _, err := r.ReadDir(b.Image.Context); err == nil {
			files = append(files, path.Join(b.Image.Context, ".dockerignore"))
		}
	}
	slices.Sort(files)
	var out []plan.FileChange
	for _, f := range slices.Compact(files) {
		if ch := ignoreChange(r, f); ch != nil {
			out = append(out, *ch)
		}
	}
	return out
}

// ignoreChange appends .shearbolt/ to the ignore file at file unless an unanchored line already
// ignores it, creating the file when it is missing. It adds a line break first when the file
// does not end with one, and uses the file's line endings. A file that exists but cannot be read
// is left alone (nil).
func ignoreChange(r plan.Repo, file string) *plan.FileChange {
	src, err := r.Read(file)
	if errors.Is(err, fs.ErrNotExist) {
		content := ".shearbolt/\n"
		return &plan.FileChange{Path: file, Action: "create", Content: content, Diff: plan.UnifiedDiff(file, nil, []byte(content)), Mode: 0o644}
	}
	if err != nil {
		return nil
	}
	text := string(src)
	for _, l := range strings.Split(text, "\n") {
		if slices.Contains(ignoreLines, strings.TrimRight(l, " \t\r")) {
			return nil
		}
	}
	eol := "\n"
	if i := strings.IndexByte(text, '\n'); i > 0 && text[i-1] == '\r' {
		eol = "\r\n"
	}
	content := text
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += eol
	}
	content += ".shearbolt/" + eol
	return &plan.FileChange{Path: file, Action: "modify", Content: content, Diff: plan.UnifiedDiff(file, src, []byte(content)), Base: src}
}
