package selfmod

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxDiffLines bounds the line count of one side of a file diff. The diff is
// for a human and a model to read; a larger file is reported by name only.
const maxDiffLines = 4000

// maxDiffBytes bounds the whole diff handed back to the model.
const maxDiffBytes = 24 << 10

// renderDiff returns a unified-style diff of changes, from the files under
// before to the files under after. It is for review, not for patch(1).
func renderDiff(before, after string, changes []change) string {
	var b strings.Builder
	for _, c := range changes {
		var old, cur []string
		if c.Op != opAdd {
			old = readLines(filepath.Join(before, filepath.FromSlash(c.Path)))
		}
		if c.Op != opDelete {
			cur = readLines(filepath.Join(after, filepath.FromSlash(c.Path)))
		}
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", c.Path, c.Path)
		if len(old) > maxDiffLines || len(cur) > maxDiffLines {
			fmt.Fprintf(&b, "@@ %s: too large to show @@\n", c.Op)
			continue
		}
		writeHunks(&b, old, cur)
		if b.Len() > maxDiffBytes {
			break
		}
	}
	out := b.String()
	if len(out) > maxDiffBytes {
		out = out[:maxDiffBytes] + "\n[diff truncated]\n"
	}
	return out
}

func readLines(p string) []string {
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
}

// diffLine is one line of an edit script: ' ' kept, '-' removed, '+' added.
type diffLine struct {
	op   byte
	text string
}

// editScript is a longest-common-subsequence diff. The inputs are bounded by
// maxDiffLines, so the quadratic table stays small enough.
func editScript(a, b []string) []diffLine {
	n, m := len(a), len(b)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []diffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffLine{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, diffLine{'-', a[i]})
			i++
		default:
			out = append(out, diffLine{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{'+', b[j]})
	}
	return out
}

// writeHunks writes the changed lines of an edit script with two lines of
// context around each change.
func writeHunks(b *strings.Builder, old, cur []string) {
	const context = 2
	script := editScript(old, cur)
	show := make([]bool, len(script))
	for i, l := range script {
		if l.op == ' ' {
			continue
		}
		for k := max(0, i-context); k <= min(len(script)-1, i+context); k++ {
			show[k] = true
		}
	}
	gap := true
	for i, l := range script {
		if !show[i] {
			gap = true
			continue
		}
		if gap {
			b.WriteString("@@\n")
			gap = false
		}
		b.WriteByte(l.op)
		b.WriteString(l.text)
		if !strings.HasSuffix(l.text, "\n") {
			b.WriteString("\n")
		}
	}
}
