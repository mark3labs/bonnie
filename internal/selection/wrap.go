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
	// Margin[i] is the width of a decoration at the start of row i, which a
	// selection does not copy; see [Selection.Margin]. It is nil when no
	// row has one.
	Margin []int
}

// Append adds the rows of v after the rows of w. Once w has margins, it has
// one for each row, so only the new rows' margins are added: a transcript
// built from many blocks does not copy its margins again for each block.
func (w *Wrapped) Append(v Wrapped) {
	if v.Margin != nil || w.Margin != nil {
		if w.Margin == nil {
			w.Margin = make([]int, len(w.Rows), len(w.Rows)+len(v.Rows))
		}
		w.Margin = append(w.Margin, v.margins()...)
	}
	w.Rows = append(w.Rows, v.Rows...)
	w.Plain = append(w.Plain, v.Plain...)
	w.Joined = append(w.Joined, v.Joined...)
}

// Indent puts prefix, a decoration of one row, before each row. Rows that
// were joined stay joined: a selection skips the prefix and copies the text
// as it was before the prefix was added.
func (w *Wrapped) Indent(prefix string) {
	n := ansi.StringWidth(prefix)
	plain := ansi.Strip(prefix)
	w.Margin = w.margins()
	for i := range w.Rows {
		w.Rows[i] = prefix + w.Rows[i]
		w.Plain[i] = plain + w.Plain[i]
		w.Margin[i] += n
	}
}

// margins returns Margin with one entry for each row.
func (w Wrapped) margins() []int {
	m := make([]int, len(w.Rows))
	copy(m, w.Margin)
	return m
}

// Lines returns text as rows that are not wrapped: each line of text is one
// row.
func Lines(text string) Wrapped {
	var w Wrapped
	for line := range strings.SplitSeq(text, "\n") {
		w.Rows = append(w.Rows, line)
		w.Plain = append(w.Plain, ansi.Strip(line))
		w.Joined = append(w.Joined, false)
	}
	return w
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
