package plan

import (
	"fmt"
	"strings"
	"testing"
)

func numbered(n int, change map[int]string) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if s, ok := change[i]; ok {
			b.WriteString(s + "\n")
		} else {
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	return []byte(b.String())
}

func hunkHeaders(diff string) []string {
	var out []string
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "@@") {
			out = append(out, l)
		}
	}
	return out
}

func TestUnifiedDiffCreate(t *testing.T) {
	got := UnifiedDiff("x", nil, []byte("a\nb\nc\n"))
	want := "--- /dev/null\n+++ b/x\n@@ -0,0 +1,3 @@\n+a\n+b\n+c\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffModifyExistingEmptyFile(t *testing.T) {
	got := UnifiedDiff(".dockerignore", []byte{}, []byte(".shearbolt/\n"))
	want := "--- a/.dockerignore\n+++ b/.dockerignore\n@@ -0,0 +1 @@\n+.shearbolt/\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffOneChange(t *testing.T) {
	got := UnifiedDiff("f.txt", numbered(100, nil), numbered(100, map[int]string{50: "changed"}))
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -47,7 +47,7 @@\n line 47\n line 48\n line 49\n-line 50\n+changed\n line 51\n line 52\n line 53\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffSeparateAndMergedHunks(t *testing.T) {
	got := hunkHeaders(UnifiedDiff("f", numbered(100, nil), numbered(100, map[int]string{30: "x", 50: "y"})))
	if len(got) != 2 || got[0] != "@@ -27,7 +27,7 @@" || got[1] != "@@ -47,7 +47,7 @@" {
		t.Fatalf("hunks = %q", got)
	}
	got = hunkHeaders(UnifiedDiff("f", numbered(100, nil), numbered(100, map[int]string{30: "x", 36: "y"})))
	if len(got) != 1 || got[0] != "@@ -27,13 +27,13 @@" {
		t.Fatalf("hunks = %q", got)
	}
}

func TestUnifiedDiffInsertAndDelete(t *testing.T) {
	old := []byte("a\nb\nc\n")
	got := UnifiedDiff("f", old, []byte("a\nb\nnew\nc\n"))
	want := "--- a/f\n+++ b/f\n@@ -1,3 +1,4 @@\n a\n b\n+new\n c\n"
	if got != want {
		t.Fatalf("insert:\n%s", got)
	}
	got = UnifiedDiff("f", old, []byte("a\nc\n"))
	want = "--- a/f\n+++ b/f\n@@ -1,3 +1,2 @@\n a\n-b\n c\n"
	if got != want {
		t.Fatalf("delete:\n%s", got)
	}
}

func TestUnifiedDiffNoFinalNewline(t *testing.T) {
	got := UnifiedDiff("f", []byte("a\nb"), []byte("a\nb\nc\n"))
	want := "--- a/f\n+++ b/f\n@@ -1,2 +1,3 @@\n a\n-b\n\\ No newline at end of file\n+b\n+c\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	got = UnifiedDiff("f", []byte("a\n"), []byte("a\nb"))
	want = "--- a/f\n+++ b/f\n@@ -1 +1,2 @@\n a\n+b\n\\ No newline at end of file\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffIdentical(t *testing.T) {
	if got := UnifiedDiff("f", []byte("a\n"), []byte("a\n")); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestUnifiedDiffEmptyFileCreateAndDelete(t *testing.T) {
	if got, want := UnifiedDiff("x", nil, []byte{}), "--- /dev/null\n+++ b/x\n"; got != want {
		t.Fatalf("create empty: got %q, want %q", got, want)
	}
	if got, want := UnifiedDiff("x", []byte{}, nil), "--- a/x\n+++ /dev/null\n"; got != want {
		t.Fatalf("delete empty: got %q, want %q", got, want)
	}
	if got := UnifiedDiff("x", nil, nil); got != "" {
		t.Fatalf("nothing to nothing: got %q", got)
	}
}

func TestUnifiedDiffMergeBoundary(t *testing.T) {
	got := hunkHeaders(UnifiedDiff("f", numbered(100, nil), numbered(100, map[int]string{30: "x", 37: "y"})))
	if len(got) != 1 || got[0] != "@@ -27,14 +27,14 @@" {
		t.Fatalf("gap of 6: hunks = %q", got)
	}
	got = hunkHeaders(UnifiedDiff("f", numbered(100, nil), numbered(100, map[int]string{30: "x", 38: "y"})))
	if len(got) != 2 || got[0] != "@@ -27,7 +27,7 @@" || got[1] != "@@ -35,7 +35,7 @@" {
		t.Fatalf("gap of 7: hunks = %q", got)
	}
}
