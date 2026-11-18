package setup

import (
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
)

// ignoreLines are the entries that already ignore .shearbolt/ at every depth below the file's
// directory. An anchored /.shearbolt covers only the top one, not nested projects' directories.
var ignoreLines = []string{".shearbolt", ".shearbolt/"}

// railpack reads no .dockerignore. A context that is unknown or not a directory in the repository
// is skipped, so setup never creates directories.
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
