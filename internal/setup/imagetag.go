package setup

import (
	"regexp"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// This file derives image names and tags (R5): the local scan tag setup adds to each gated
// image build, its repository, and the variable and repository names of image targets.

// envName turns a target name into the suffix of its image variable: upper case, with every
// character outside [A-Z0-9] replaced by "_".
func envName(s string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(s) {
		if c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// dockerRepo is the form of a valid one-component Docker repository name.
var dockerRepo = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)

// dockerRepoName returns name as a valid Docker repository name for local tags: as it is when it
// is one, otherwise lower-cased with every run of other characters replaced by "-"; "image" when
// nothing is left.
func dockerRepoName(name string) string {
	if dockerRepo.MatchString(name) {
		return name
	}
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "image"
}

// scanTag returns the local tag setup adds to an image build and the gate scans (R5):
// <repository>:shearbolt-<commit>. For a shell command it is written like the build's first
// tag: double quotes are copied, single quotes become double quotes when that keeps the
// repository's meaning. For build-push-action it is a plain tags entry. A tag already
// containing ":shearbolt-" is reused as written. slotRepo stands in for a repository that
// cannot be derived (buildRepository).
func scanTag(img plan.Image, commit, slotRepo string) string {
	shellWords := img.Tool != "build-push-action"
	for _, t := range img.Tags {
		v := t
		if shellWords {
			v = wordValue(t)
		}
		if strings.Contains(v, ":shearbolt-") {
			return t
		}
	}
	repo, ok := buildRepository(img)
	if !ok {
		repo = slotRepo
	}
	tag := repo + ":shearbolt-" + commit
	if !shellWords || len(img.Tags) == 0 || len(img.Tags[0]) < 2 {
		return tag
	}
	raw := img.Tags[0]
	switch q := raw[0]; {
	case q == '"' && raw[len(raw)-1] == '"':
		return `"` + tag + `"`
	case q == '\'' && raw[len(raw)-1] == '\'':
		if !strings.ContainsAny(repo, "$`\\\"") {
			return `"` + tag + `"`
		}
		return "'" + repo + ":shearbolt-'" + commit
	}
	return tag
}

// tagValue returns a scan tag's value: unquoted for a shell build, as written for an action.
func tagValue(img plan.Image, tag string) string {
	if img.Tool == "build-push-action" {
		return tag
	}
	return wordValue(tag)
}

// buildRepository derives the repository of an image build's scan tag (R5): the first tag
// without its tag part, which starts at the last ":" after the last "/" outside ${...},
// ${{ ... }} and $(...). A variable repository followed by a tag part is kept ($IMAGE:latest
// gives $IMAGE). It fails (false) when there is no tag, when the first tag has no tag part and
// is a single variable or expression ($TAG, ${{ steps.x.outputs.tag }}), since that may hold a
// tag, and for metadata-action tags whose images entry is unknown or holds ${{, since that can
// produce upper case. A literal images entry is lower-cased, as metadata-action does.
func buildRepository(img plan.Image) (string, bool) {
	if img.Computed != "" {
		if img.ComputedRepo == "" || strings.Contains(img.ComputedRepo, "${{") {
			return "", false
		}
		// metadata-action lower-cases image names (sanitizeImageName), so its tags use this.
		return strings.ToLower(img.ComputedRepo), true
	}
	if len(img.Tags) == 0 {
		return "", false
	}
	v := img.Tags[0]
	if img.Tool != "build-push-action" {
		v = wordValue(v)
	}
	slash, colon := -1, -1
	for i := 0; i < len(v); i++ {
		if e := expansionEnd(v, i); e > i {
			i = e - 1
			continue
		}
		switch v[i] {
		case '/':
			slash = i
		case ':':
			colon = i
		}
	}
	if colon > slash {
		if colon == 0 {
			return "", false
		}
		return v[:colon], true
	}
	if expansionEnd(v, 0) == len(v) || v == "" {
		return "", false
	}
	return v, true
}

// wordValue returns a shell word's value: quotes and escapes removed, variables kept.
func wordValue(raw string) string {
	cmds, _ := shell.Parse(raw, shell.State{})
	if len(cmds) == 1 && len(cmds[0].Words) == 1 {
		return cmds[0].Words[0].Value
	}
	return raw
}

// expansionEnd returns the end of a shell or GitHub expansion that starts at v[i] ($NAME, $1,
// ${...}, ${{ ... }}, $(...) or a backtick string), or i when none starts there; an expansion
// that never closes runs to the end.
func expansionEnd(v string, i int) int {
	switch {
	case strings.HasPrefix(v[i:], "${{"):
		if e := strings.Index(v[i:], "}}"); e >= 0 {
			return i + e + 2
		}
		return len(v)
	case strings.HasPrefix(v[i:], "${"), strings.HasPrefix(v[i:], "$("):
		open, close := v[i+1], byte('}')
		if open == '(' {
			close = ')'
		}
		depth := 0
		for k := i + 1; k < len(v); k++ {
			switch v[k] {
			case open:
				depth++
			case close:
				if depth--; depth == 0 {
					return k + 1
				}
			}
		}
		return len(v)
	case v[i] == '`':
		if e := strings.IndexByte(v[i+1:], '`'); e >= 0 {
			return i + e + 2
		}
		return len(v)
	case v[i] == '$' && i+1 < len(v):
		c := v[i+1]
		if strings.IndexByte("0123456789@*#?$!-", c) >= 0 {
			return i + 2
		}
		k := i + 1
		for k < len(v) && (v[k] == '_' || v[k] >= 'a' && v[k] <= 'z' || v[k] >= 'A' && v[k] <= 'Z' || k > i+1 && v[k] >= '0' && v[k] <= '9') {
			k++
		}
		if k > i+1 {
			return k
		}
	}
	return i
}
