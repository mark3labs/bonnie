package selection

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func drag(s *Selection, from, to Point) {
	s.Press(from)
	s.Drag(to)
	s.Release()
}

// A click that never moves selects nothing, so it cannot replace the
// clipboard with one character.
func TestClickSelectsNothing(t *testing.T) {
	t.Parallel()
	var s Selection
	s.Press(Point{0, 3})
	s.Release()
	if !s.Empty() {
		t.Fatal("a click without a drag made a selection")
	}
	if got := s.Text([]string{"hello"}, nil); got != "" {
		t.Errorf("Text() = %q; want empty", got)
	}
}

// Both end cells are selected, in whichever direction the drag went.
func TestSelectionIncludesBothEnds(t *testing.T) {
	t.Parallel()
	plain := []string{"hello world"}
	for _, tc := range []struct {
		name     string
		from, to Point
	}{
		{"forward", Point{0, 6}, Point{0, 10}},
		{"backward", Point{0, 10}, Point{0, 6}},
	} {
		var s Selection
		drag(&s, tc.from, tc.to)
		if got := s.Text(plain, nil); got != "world" {
			t.Errorf("%s: Text() = %q; want %q", tc.name, got, "world")
		}
	}
}

// A drag that leaves the anchor and comes back selects the one cell.
func TestDragBackToAnchorSelectsOneCell(t *testing.T) {
	t.Parallel()
	var s Selection
	s.Press(Point{0, 1})
	s.Drag(Point{0, 2})
	s.Drag(Point{0, 1})
	if got := s.Text([]string{"abc"}, nil); got != "b" {
		t.Errorf("Text() = %q; want %q", got, "b")
	}
}

// Across lines, the selection runs to the end of the first line and from
// the start of the last; spaces at the end of a hard line are dropped.
func TestMultiLineText(t *testing.T) {
	t.Parallel()
	plain := []string{"first line   ", "second", "third line"}
	var s Selection
	drag(&s, Point{0, 6}, Point{2, 4})
	if got, want := s.Text(plain, nil), "line\nsecond\nthird"; got != want {
		t.Errorf("Text() = %q; want %q", got, want)
	}
}

// Left and Right keep a panel's border out of the copy.
func TestColumnBounds(t *testing.T) {
	t.Parallel()
	plain := []string{"│ alpha │", "│ beta  │"}
	s := Selection{Left: 2, Right: 7}
	drag(&s, Point{0, 2}, Point{1, 5})
	if got, want := s.Text(plain, nil), "alpha\nbeta"; got != want {
		t.Errorf("Text() = %q; want %q", got, want)
	}
}

// Clear keeps the column bounds, which belong to the caller's layout.
func TestClearKeepsBounds(t *testing.T) {
	t.Parallel()
	s := Selection{Left: 2, Right: 7}
	drag(&s, Point{0, 2}, Point{0, 4})
	s.Clear()
	if !s.Empty() || s.Dragging() || s.Left != 2 || s.Right != 7 {
		t.Errorf("after Clear: %+v", s)
	}
}

// Highlight draws only the selected cells, keeps the styling around them,
// and does not change the caller's lines.
func TestHighlightKeepsSurroundingStyle(t *testing.T) {
	t.Parallel()
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	lines := []string{red.Render("redtext") + " plain", "untouched"}
	orig := append([]string(nil), lines...)
	var s Selection
	drag(&s, Point{0, 3}, Point{0, 8})

	out := s.Highlight(lines, 0, lipgloss.NewStyle().Reverse(true))
	if lines[0] != orig[0] || lines[1] != orig[1] {
		t.Fatal("Highlight changed its input")
	}
	if got := ansi.Strip(out[0]); got != "redtext plain" {
		t.Errorf("highlighted text = %q; want the same cells", got)
	}
	if !strings.HasPrefix(out[0], ansi.Cut(lines[0], 0, 3)) {
		t.Errorf("the cells before the selection lost their style: %q", out[0])
	}
	if !strings.Contains(out[0], "\x1b[7m") {
		t.Errorf("no reverse video in %q", out[0])
	}
	if out[1] != lines[1] {
		t.Errorf("a line outside the selection changed: %q", out[1])
	}
}

// Highlight maps lines to the selection's space through first.
func TestHighlightUsesFirstLine(t *testing.T) {
	t.Parallel()
	var s Selection
	drag(&s, Point{10, 0}, Point{10, 2})
	style := lipgloss.NewStyle().Reverse(true)
	if out := s.Highlight([]string{"abc"}, 0, style); out[0] != "abc" {
		t.Errorf("line 0 highlighted for a selection on line 10: %q", out[0])
	}
	if out := s.Highlight([]string{"abc"}, 10, style); out[0] == "abc" {
		t.Error("line 10 not highlighted")
	}
}

// A paragraph that Wrap broke is copied as the one line it was, with the
// spaces at its breaks; a real line break stays.
func TestWrappedParagraphCopiesAsOneLine(t *testing.T) {
	t.Parallel()
	src := "the quick brown fox jumps  over\nnext"
	w := Wrap(src, 10)
	if len(w.Rows) < 4 {
		t.Fatalf("Wrap made %d rows; want the paragraph broken: %q", len(w.Rows), w.Rows)
	}
	if got := strings.Join(w.Rows, "\n"); got != lipgloss.Wrap(src, 10, "") {
		t.Errorf("rows = %q; want what lipgloss.Wrap draws", got)
	}
	var s Selection
	drag(&s, Point{0, 0}, Point{len(w.Rows) - 1, 3})
	if got := s.Text(w.Plain, w.Joined); got != src {
		t.Errorf("Text() = %q; want %q", got, src)
	}
}

// A word longer than the width is cut with no space, and joins with none.
func TestWrappedLongWordJoinsWithoutSpace(t *testing.T) {
	t.Parallel()
	w := Wrap("abcdefghijklmnop", 7)
	var s Selection
	drag(&s, Point{0, 0}, Point{len(w.Rows) - 1, 6})
	if got := s.Text(w.Plain, w.Joined); got != "abcdefghijklmnop" {
		t.Errorf("Text() = %q", got)
	}
}

// Styled rows wrap and copy the same as plain ones.
func TestWrapStyledText(t *testing.T) {
	t.Parallel()
	src := lipgloss.NewStyle().Bold(true).Render("bold words that wrap around")
	w := Wrap(src, 8)
	var s Selection
	drag(&s, Point{0, 0}, Point{len(w.Rows) - 1, 7})
	if got := s.Text(w.Plain, w.Joined); got != "bold words that wrap around" {
		t.Errorf("Text() = %q", got)
	}
}
