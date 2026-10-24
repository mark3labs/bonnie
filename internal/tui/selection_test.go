package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// clipboardText runs cmd and returns the text it puts on the clipboard.
// tea.SetClipboard's message type is not exported, so the test matches it
// by name.
func clipboardText(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command; want the clipboard write")
	}
	msg := cmd()
	v := reflect.ValueOf(msg)
	if v.Type().Name() != "setClipboardMsg" || v.Kind() != reflect.String {
		t.Fatalf("command sent %T; want tea.SetClipboard", msg)
	}
	return v.String()
}

// press, move, and release send one left-button mouse event at a screen
// cell.
func press(m Model, x, y int) (Model, tea.Cmd) {
	next, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return next.(Model), cmd
}

func move(m Model, x, y int) (Model, tea.Cmd) {
	next, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
	return next.(Model), cmd
}

func release(m Model, x, y int) (Model, tea.Cmd) {
	next, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	return next.(Model), cmd
}

// rowOf returns the screen row where text is shown. The transcript starts
// below the header.
func rowOf(t *testing.T, m Model, text string) int {
	t.Helper()
	for i, row := range m.rows.Plain {
		if strings.Contains(row, text) {
			return i - m.viewport.YOffset() + transcriptTop
		}
	}
	t.Fatalf("%q is not in the transcript: %q", text, m.rows.Plain)
	return 0
}

// The mouse is captured so the wheel scrolls, which means the terminal
// cannot select text. A drag over the transcript must select and copy it.
func TestDragCopiesTranscriptText(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.commitUser("first message")
	m.commit(kindAssistant, "the reply")
	m.syncViewport()

	y0, y1 := rowOf(t, m, "first message"), rowOf(t, m, "the reply")
	m, _ = press(m, 2, y0)
	m, _ = move(m, 4, y1)
	m, cmd := release(m, 4, y1)

	if got, want := clipboardText(t, cmd), "first message\nthe r"; got != want {
		t.Errorf("copied %q; want %q", got, want)
	}
	if m.label != "copied 19 characters" {
		t.Errorf("label = %q; want the copy count", m.label)
	}
	// The highlight stays, so the reader can see what was copied.
	if !strings.Contains(m.View().Content, "\x1b[7m") {
		t.Error("the copied text is not highlighted")
	}
}

// A paragraph that wraps to the width is one line in the source. It must be
// copied as one line, not as one line per screen row.
func TestDragCopiesWrappedParagraphAsOneLine(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 30})
	m = next.(Model)
	para := "one two three four five six seven eight nine ten eleven twelve"
	m.commit(kindAssistant, para)
	m.syncViewport()
	if len(m.rows.Rows) < 3 {
		t.Fatalf("paragraph made %d rows; want it wrapped", len(m.rows.Rows))
	}

	last := len(m.rows.Rows) - 1
	m, _ = press(m, 0, transcriptTop)
	m, _ = move(m, 29, transcriptTop+last)
	_, cmd := release(m, 29, transcriptTop+last)
	if got := clipboardText(t, cmd); got != para {
		t.Errorf("copied %q; want %q", got, para)
	}
}

// A click selects nothing, so it must not replace the clipboard.
func TestClickDoesNotCopy(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.commit(kindAssistant, "the reply")
	m.syncViewport()

	y := rowOf(t, m, "the reply")
	m, _ = press(m, 2, y)
	m, cmd := release(m, 2, y)
	if cmd != nil {
		t.Errorf("a click returned a command: %T", cmd())
	}
	if !m.sel.Empty() {
		t.Error("a click left a selection")
	}
}

// Output that streams in during a drag must not scroll the transcript: the
// text under the pointer would move, and the drag would select the wrong
// rows.
func TestDragStopsFollowingOutput(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	for range 40 {
		m.commit(kindAssistant, "filler")
		m.commitUser("more")
	}
	m.syncViewport()
	m.viewport.GotoBottom()
	off := m.viewport.YOffset()

	m, _ = press(m, 0, 5)
	m, _ = move(m, 3, 6)
	m.appendAssistant("new output\nmore\nmore\n")
	m.syncViewport()
	if got := m.viewport.YOffset(); got != off {
		t.Errorf("the transcript scrolled from %d to %d during a drag", off, got)
	}
}

// A drag below the transcript scrolls it, so a selection can be longer than
// the screen.
func TestDragPastTheEdgeScrolls(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	for i := range 60 {
		m.commitUser(strings.Repeat("x", i%5+1))
	}
	m.syncViewport()
	m.viewport.GotoTop()

	m, _ = press(m, 0, transcriptTop)
	m, _ = move(m, 0, m.height)
	if m.viewport.YOffset() != 1 {
		t.Errorf("YOffset = %d; want one row of scroll", m.viewport.YOffset())
	}
}

// A key press or a new width drops the selection: the first moves on, and
// the second wraps the rows again so the cells no longer hold the text.
func TestSelectionClears(t *testing.T) {
	t.Parallel()
	selected := func() Model {
		m := newTestModel(&fakeClient{})
		m.commit(kindAssistant, "the reply")
		m.syncViewport()
		y := rowOf(t, m, "the reply")
		m, _ = press(m, 0, y)
		m, _ = move(m, 4, y)
		m, _ = release(m, 4, y)
		if m.sel.Empty() {
			t.Fatal("no selection to clear")
		}
		return m
	}

	next, _ := selected().Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if !next.(Model).sel.Empty() {
		t.Error("a key press kept the selection")
	}
	next, _ = selected().Update(tea.WindowSizeMsg{Width: 50, Height: 30})
	if !next.(Model).sel.Empty() {
		t.Error("a new width kept the selection")
	}
}

// A click outside the transcript, on the input, does not start a selection.
func TestPressOnInputDoesNotSelect(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.commit(kindAssistant, "the reply")
	m, _ = press(m, 0, m.height-2)
	m, _ = move(m, 5, transcriptTop)
	if m.sel.Dragging() || !m.sel.Empty() {
		t.Error("a press on the input started a selection")
	}
}

// A terminal with bracketed paste sends a paste as one message. It must reach
// the input, newlines included, and must not send the message.
func TestPasteGoesToTheInput(t *testing.T) {
	t.Parallel()
	c := &fakeClient{}
	m := newTestModel(c)
	next, _ := m.Update(tea.PasteMsg{Content: "line one\nline two"})
	m = next.(Model)
	if got := m.input.Value(); got != "line one\nline two" {
		t.Errorf("input = %q; want the pasted text", got)
	}
	if c.started || c.ensured > 0 || len(m.entries) > 0 {
		t.Error("the paste sent a message")
	}
}
