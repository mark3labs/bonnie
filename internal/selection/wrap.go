package selection

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Wrapped is text broken into screen rows, with what [Selection.Text] needs
// to copy the rows back as the text they came from.
type Wrapped struct {
	// Rows are the styled rows to draw.
	Rows []string
	// Plain are the rows without escape sequences. A row that a wrap broke
	// at a space keeps that space at its end: lipgloss.Wrap drops it from
	// Rows, and without it two words would be copied as one.
	Plain []string
	// Joined[i] reports that row i continues row i-1 of the same line.
	Joined []bool
}

// Wrap breaks each line of text to width with lipgloss.Wrap, as a single
// lipgloss.Wrap of the whole text would, and records which breaks it made.
func Wrap(text string, width int) Wrapped {
	var w Wrapped
	for line := range strings.SplitSeq(text, "\n") {
		rows := strings.Split(lipgloss.Wrap(line, width, ""), "\n")
		src := ansi.Strip(line)
		pos, ok := 0, true
		for i, row := range rows {
			plain := ansi.Strip(row)
			// Walk the source to find what the wrap removed after this row.
			// When the rows stop matching the source, keep the rows as they
			// are: a missing space is better than a wrong character.
			if ok && strings.HasPrefix(src[pos:], plain) {
				pos += len(plain)
			} else {
				ok = false
			}
			if ok && i+1 < len(rows) {
				next := ansi.Strip(rows[i+1])
				start := pos
				for pos < len(src) && src[pos] == ' ' && !strings.HasPrefix(src[pos:], next) {
					pos++
				}
				plain += src[start:pos]
			}
			w.Rows = append(w.Rows, row)
			w.Plain = append(w.Plain, plain)
			w.Joined = append(w.Joined, i > 0)
		}
	}
	return w
}
