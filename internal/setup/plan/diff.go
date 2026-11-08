package plan

import (
	"bytes"
	"fmt"
	"strings"
)

const (
	diffContext = 3
	// maxLCSCells bounds the LCS table for the part of the files that differs. Larger inputs
	// get a correct but not minimal diff (all old lines removed, all new lines added).
	maxLCSCells = 4_000_000
)

// UnifiedDiff returns a unified diff from old to new with a/ and b/ prefixes and 3 lines of
// context, or "" when they are equal. old is nil when the file does not exist (the diff then
// reads from /dev/null); new is nil when it is removed. Creating or removing a file always gives
// a diff, even an empty file (headers only).
func UnifiedDiff(path string, old, new []byte) string {
	if (old == nil) == (new == nil) && bytes.Equal(old, new) {
		return ""
	}
	a, b := splitLines(old), splitLines(new)
	ops := diffLines(a, b)
	var out strings.Builder
	from, to := "a/"+path, "b/"+path
	if old == nil {
		from = "/dev/null"
	}
	if new == nil {
		to = "/dev/null"
	}
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	for _, h := range hunks(ops) {
		hunk := ops[h[0]:h[1]]
		oldN, newN := 0, 0
		for _, o := range hunk {
			if o.kind != '+' {
				oldN++
			}
			if o.kind != '-' {
				newN++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(hunk[0].a, oldN), hunkRange(hunk[0].b, newN))
		for _, o := range hunk {
			line := o.text
			out.WriteByte(o.kind)
			out.WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
	}
	return out.String()
}

// diffOp is one line of the edit script. a and b are the 0-based positions in the old and new
// lines where the op happens (for an insert, a is the number of old lines before it).
type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
	a, b int
}

// splitLines splits into lines that keep their "\n"; a last line without one stays as is.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines computes a line LCS edit script, with deletions before insertions in each change.
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []diffOp
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{' ', a[i], i, i})
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(am), len(bm)
	i, j := 0, 0
	if n > 0 && m > 0 && n*m <= maxLCSCells {
		w := m + 1
		lcs := make([]int32, (n+1)*w)
		for x := n - 1; x >= 0; x-- {
			for y := m - 1; y >= 0; y-- {
				if am[x] == bm[y] {
					lcs[x*w+y] = lcs[(x+1)*w+y+1] + 1
				} else {
					lcs[x*w+y] = max(lcs[(x+1)*w+y], lcs[x*w+y+1])
				}
			}
		}
		for i < n && j < m {
			switch {
			case am[i] == bm[j]:
				ops = append(ops, diffOp{' ', am[i], pre + i, pre + j})
				i, j = i+1, j+1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				ops = append(ops, diffOp{'-', am[i], pre + i, pre + j})
				i++
			default:
				ops = append(ops, diffOp{'+', bm[j], pre + i, pre + j})
				j++
			}
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', am[i], pre + i, pre + j})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', bm[j], pre + n, pre + j})
	}
	for k := 0; k < suf; k++ {
		ops = append(ops, diffOp{' ', a[len(a)-suf+k], len(a) - suf + k, len(b) - suf + k})
	}
	return ops
}

// hunks groups the changes into [start, end) ranges of ops with context, merging changes whose
// context would overlap.
func hunks(ops []diffOp) [][2]int {
	var out [][2]int
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-diffContext, 0)
		j := i
		for {
			for j < len(ops) && ops[j].kind != ' ' {
				j++
			}
			k := j
			for k < len(ops) && ops[k].kind == ' ' {
				k++
			}
			if k < len(ops) && k-j <= 2*diffContext {
				j = k
				continue
			}
			break
		}
		end := min(j+diffContext, len(ops))
		out = append(out, [2]int{start, end})
		i = end
	}
	return out
}

// hunkRange formats one side of a hunk header. pos is the 0-based index of its first line; an
// empty range names the line before it.
func hunkRange(pos, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", pos)
	case 1:
		return fmt.Sprintf("%d", pos+1)
	}
	return fmt.Sprintf("%d,%d", pos+1, n)
}
