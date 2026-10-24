// Package selection is the mouse text selection that BONNIE's terminal
// interfaces share.
//
// The interfaces capture the mouse so that the wheel scrolls the transcript.
// While the mouse is captured, the terminal cannot select text itself, so the
// interface must do it: it records where the left button went down, follows
// the drag, draws the selected cells in reverse video, and copies the text
// when the button comes up. The terminal's own selection is still available
// with Shift+drag (Option+drag in iTerm2), but few people know that, and it
// sees only the visible rows and copies each wrapped row as its own line.
//
// A [Selection] knows nothing about the screen. Its points are (line, cell)
// pairs in whatever space the caller chooses: content lines of a scrolled
// transcript in `bonnie chat`, screen rows of one panel in `bonnie runs`.
package selection

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Point is one cell: a line index and a column in terminal cells.
type Point struct {
	Line, Col int
}

// before reports whether p comes before q in reading order.
func (p Point) before(q Point) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Col < q.Col)
}

// Selection is a range of cells between the point where the left button went
// down and the point it was dragged to. Both end cells are selected, so a
// drag from the first letter of a word to its last copies the whole word.
// The zero value has no selection.
type Selection struct {
	// Left and Right bound the selectable columns; Right is exclusive. A
	// selection that spans lines runs from its start to Right on the first
	// line and from Left to its end on the last. A Right of zero means the
	// end of each line.
	Left, Right int

	anchor, head Point
	dragging     bool
	// moved is set by the first drag to a cell other than the anchor. A
	// click that never moves selects nothing: it must not replace the
	// clipboard with one character.
	moved bool
}

// Press starts a new selection at p and drops the old one.
func (s *Selection) Press(p Point) {
	s.anchor, s.head = p, p
	s.dragging, s.moved = true, false
}

// Drag moves the free end of the selection to p. It does nothing when no
// button is down.
func (s *Selection) Drag(p Point) {
	if !s.dragging {
		return
	}
	if p != s.anchor {
		s.moved = true
	}
	s.head = p
}

// Release ends the drag. The selection stays, so the caller can show what it
// copied.
func (s *Selection) Release() { s.dragging = false }

// Clear drops the selection.
func (s *Selection) Clear() {
	left, right := s.Left, s.Right
	*s = Selection{Left: left, Right: right}
}

// Dragging reports whether the left button is down.
func (s Selection) Dragging() bool { return s.dragging }

// Empty reports whether nothing is selected: there was no press, or the
// press never moved.
func (s Selection) Empty() bool { return !s.moved }

// bounds returns the two ends of the selection in reading order.
func (s Selection) bounds() (start, end Point) {
	if s.head.before(s.anchor) {
		return s.head, s.anchor
	}
	return s.anchor, s.head
}

// span returns the selected cells of line as [lo, hi). ok is false when the
// line is outside the selection.
func (s Selection) span(line int) (lo, hi int, ok bool) {
	if s.Empty() {
		return 0, 0, false
	}
	start, end := s.bounds()
	if line < start.Line || line > end.Line {
		return 0, 0, false
	}
	lo, hi = s.Left, s.Right
	if hi <= 0 {
		hi = int(^uint(0) >> 1)
	}
	if line == start.Line {
		lo = max(lo, start.Col)
	}
	if line == end.Line {
		hi = min(hi, end.Col+1)
	}
	return lo, hi, lo < hi
}

// Highlight returns lines with the selected cells drawn in style. lines[i] is
// line first+i in the selection's space. Each line keeps its own styling
// outside the selection; inside it, the text is drawn plain in style, so a
// reverse-video style reads the same on every color. lines is not changed: a
// line that needs a highlight is replaced in a copy.
func (s Selection) Highlight(lines []string, first int, style lipgloss.Style) []string {
	if s.Empty() {
		return lines
	}
	out := lines
	copied := false
	for i, line := range lines {
		lo, hi, ok := s.span(first + i)
		if !ok {
			continue
		}
		width := ansi.StringWidth(line)
		hi = min(hi, width)
		if lo >= hi {
			continue
		}
		if !copied {
			out = append([]string(nil), lines...)
			copied = true
		}
		// ansi.Cut closes the SGR state it opened at the end of each piece
		// and opens it again at the start of the next, so the color of the
		// text before the selection does not leak into the highlight.
		out[i] = ansi.Cut(line, 0, lo) + style.Render(ansi.Strip(ansi.Cut(line, lo, hi))) + ansi.Cut(line, hi, width)
	}
	return out
}

// Text returns the selected text from plain, which holds the lines as plain
// text without escape sequences, indexed by line.
//
// joined[i] reports that line i is the soft-wrapped continuation of line
// i-1: the two are one line in the source, broken only to fit the screen.
// Text joins such lines with nothing between them, so a wrapped paragraph or
// code line is copied as it was written. The caller keeps any space the wrap
// removed at the end of plain[i-1]. joined may be nil or short; a missing
// entry is a hard line break. Spaces at the end of a hard line are dropped,
// as a terminal does.
func (s Selection) Text(plain []string, joined []bool) string {
	if s.Empty() {
		return ""
	}
	start, end := s.bounds()
	var b strings.Builder
	for line := max(start.Line, 0); line <= end.Line && line < len(plain); line++ {
		lo, hi, _ := s.span(line)
		seg := ""
		if w := ansi.StringWidth(plain[line]); lo < w {
			seg = ansi.Cut(plain[line], lo, min(hi, w))
		}
		soft := line+1 < len(joined) && joined[line+1] && line < end.Line
		if !soft {
			seg = strings.TrimRight(seg, " ")
		}
		b.WriteString(seg)
		if !soft && line < end.Line && line+1 < len(plain) {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Copy returns the command that puts text on the system clipboard.
//
// It uses OSC 52, which the terminal carries out: kitty, WezTerm, iTerm2,
// Ghostty, Alacritty, foot, Windows Terminal, and tmux with set-clipboard
// enabled support it, also over SSH. Some terminals, such as those built on
// VTE (GNOME Terminal, Tilix), ignore it, and the terminal sends back no
// answer, so a caller cannot know whether the copy worked. Every copy goes
// through this function, so a native clipboard fallback can be added here
// without a change at any call site.
func Copy(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	return tea.SetClipboard(text)
}
