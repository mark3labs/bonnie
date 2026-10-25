package runsui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// copied runs cmd and returns the text it puts on the clipboard, looking
// inside a batch. tea.SetClipboard's message type is not exported, so the
// test matches it by name. A command that does not return soon is a timer,
// such as the toast's 4 s, and is skipped. The wait is long enough that a
// slow -race run does not skip the clipboard write.
func copied(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command; want the clipboard write")
	}
	var found []string
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- c() }()
		var msg tea.Msg
		select {
		case msg = <-done:
		case <-time.After(500 * time.Millisecond):
			return
		}
		switch msg := msg.(type) {
		case tea.BatchMsg:
			for _, c := range msg {
				walk(c)
			}
		default:
			if v := reflect.ValueOf(msg); v.IsValid() && v.Type().Name() == "setClipboardMsg" {
				found = append(found, v.String())
			}
		}
	}
	walk(cmd)
	if len(found) != 1 {
		t.Fatalf("command wrote the clipboard %d times; want once", len(found))
	}
	return found[0]
}

// update sends msg without running its command, so the test can examine it.
func (h *harness) update(msg tea.Msg) tea.Cmd {
	h.t.Helper()
	next, cmd := h.m.Update(msg)
	h.m = next.(model)
	return cmd
}

// cellOf returns the screen cell where text starts.
func cellOf(t *testing.T, h *harness, text string) (x, y int) {
	t.Helper()
	for y, row := range strings.Split(h.screen(), "\n") {
		if before, _, ok := strings.Cut(row, text); ok {
			return ansi.StringWidth(before), y
		}
	}
	t.Fatalf("%q is not on the screen:\n%s", text, h.screen())
	return 0, 0
}

// A drag in the main panel selects and copies what it shows.
func TestConsoleDragCopiesMainPanelText(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.keys("]", "3")
	x, y := cellOf(t, h, "deploy it")
	h.update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.update(tea.MouseMotionMsg{X: x + len("deploy it") - 1, Y: y, Button: tea.MouseLeft})
	cmd := h.update(tea.MouseReleaseMsg{X: x + len("deploy it") - 1, Y: y, Button: tea.MouseLeft})
	if got := copied(t, cmd); got != "deploy it" {
		t.Errorf("copied %q; want %q", got, "deploy it")
	}
	if !strings.Contains(h.m.View().Content, "\x1b[7m") {
		t.Error("the copied text is not highlighted")
	}
}

// A selection stays inside the panel it started in: a drag over the runs
// list or the border copies none of their text.
func TestConsoleSelectionStaysInMainPanel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.keys("]", "3")
	body := h.m.mainBody()
	h.update(tea.MouseClickMsg{X: body.x, Y: body.y, Button: tea.MouseLeft})
	h.update(tea.MouseMotionMsg{X: 0, Y: body.y + 2, Button: tea.MouseLeft})
	cmd := h.update(tea.MouseReleaseMsg{X: 0, Y: body.y + 2, Button: tea.MouseLeft})
	text := copied(t, cmd)
	if strings.ContainsAny(text, "│╭╰") || strings.Contains(text, "Runs") {
		t.Errorf("the copy holds text from outside the main panel: %q", text)
	}
}

// A click in the main panel still does what it did: on the journal tab, a
// second click on the selected record opens it. The action waits for the
// release so a drag does not trigger it.
func TestConsoleClickInMainPanelStillActs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.send(tea.MouseClickMsg{X: h.m.tabX[tabJournal] + 1, Y: h.m.rTabs.y, Button: tea.MouseLeft})
	if h.m.tab != tabJournal {
		t.Fatalf("tab = %s; want the journal", h.m.tab)
	}
	// The first record row is two lines below the panel's top border.
	x, y := h.m.rMain.x+3, h.m.rMain.y+2
	h.send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if h.m.recOpen {
		t.Fatal("the record opened on press; want the release")
	}
	h.send(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if !h.m.recOpen {
		t.Error("a click on the selected record did not open it")
	}

	// A drag over the same row opens nothing.
	h.m.recOpen = false
	h.update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.update(tea.MouseMotionMsg{X: x + 5, Y: y, Button: tea.MouseLeft})
	h.update(tea.MouseReleaseMsg{X: x + 5, Y: y, Button: tea.MouseLeft})
	if h.m.recOpen {
		t.Error("a drag opened the record")
	}
}

// longRun is a run with a user message that wraps in the main panel, then
// enough replies to scroll it.
func longRun(t *testing.T, msg string) *fakeJournal {
	b := newBuilder(t, "run-1")
	b.turn("T1", "").state(runtime.RunRunning).message("user", kit.LLMTextPart{Text: msg})
	for i := range 30 {
		b.message("assistant", kit.LLMTextPart{Text: fmt.Sprintf("reply %d", i)})
	}
	b.state(runtime.RunCompleted)
	j := newFakeJournal()
	j.put("run-1", runtime.RunCompleted, b.recs)
	return j
}

// selectRows drags from the start of the row that holds from to the end of
// the row that holds to, both in the main panel, and returns the command of
// the release.
func selectRows(t *testing.T, h *harness, from, to string) tea.Cmd {
	t.Helper()
	body := h.m.mainBody()
	_, y0 := cellOf(t, h, from)
	_, y1 := cellOf(t, h, to)
	h.update(tea.MouseClickMsg{X: body.x, Y: y0, Button: tea.MouseLeft})
	h.update(tea.MouseMotionMsg{X: body.x + body.w - 1, Y: y1, Button: tea.MouseLeft})
	return h.update(tea.MouseReleaseMsg{X: body.x + body.w - 1, Y: y1, Button: tea.MouseLeft})
}

// A message that wraps to the panel is one line in the journal. It must be
// copied as one line, without the bar drawn at the left of each row.
func TestConsoleDragCopiesWrappedMessageAsOneLine(t *testing.T) {
	t.Parallel()
	msg := strings.Repeat("the deploy needs a second look ", 12) + "before noon"
	h := newHarness(t, longRun(t, msg), nil)
	h.keys("]", "3")
	got := copied(t, selectRows(t, h, "the deploy needs", "before noon"))
	if got != msg {
		t.Errorf("copied %q; want the message as one line: %q", got, msg)
	}
}

// On the tabs that scroll, a selection is held in rows of the text, so a
// scroll moves the highlight with its text and keeps the selection.
func TestConsoleSelectionStaysOnItsTextWhenScrolled(t *testing.T) {
	t.Parallel()
	h := newHarness(t, longRun(t, "first message"), nil)
	h.keys("]", "3")
	h.m.main.GotoTop()
	body := h.m.mainBody()
	wheel := tea.MouseWheelMsg{X: body.x, Y: body.y, Button: tea.MouseWheelDown}
	// Select after a scroll, so the rows of the text are not the rows of the
	// panel.
	h.send(wheel)
	if got := copied(t, selectRows(t, h, "reply 3", "reply 3")); got != "reply 3" {
		t.Fatalf("copied %q; want %q", got, "reply 3")
	}
	_, before := cellOf(t, h, "reply 3")
	h.send(wheel)
	if h.m.textSel.Empty() {
		t.Fatal("the wheel dropped the selection")
	}
	_, after := cellOf(t, h, "reply 3")
	if after >= before {
		t.Fatalf("the wheel did not scroll: row %d, then %d", before, after)
	}
	rows := strings.Split(h.m.View().Content, "\n")
	if !strings.Contains(rows[after], "\x1b[7m") {
		t.Errorf("the highlight did not move with its text to row %d", after)
	}
	for i, r := range rows {
		if i != after && strings.Contains(r, "\x1b[7m") {
			t.Errorf("row %d is highlighted, and only row %d holds the selection", i, after)
		}
	}
}

// A drag below the panel scrolls it, so a selection can be longer than the
// panel.
func TestConsoleDragPastThePanelScrolls(t *testing.T) {
	t.Parallel()
	h := newHarness(t, longRun(t, "first message"), nil)
	h.keys("]", "3")
	h.m.main.GotoTop()
	body := h.m.mainBody()
	h.update(tea.MouseClickMsg{X: body.x, Y: body.y, Button: tea.MouseLeft})
	h.update(tea.MouseMotionMsg{X: body.x, Y: body.y + body.h + 2, Button: tea.MouseLeft})
	if got := h.m.main.YOffset(); got != 1 {
		t.Errorf("YOffset = %d; want one row of scroll", got)
	}
}

// A key press moves on, and a new width wraps the rows again, so both drop
// the selection. A new height does not move the text.
func TestConsoleScrolledSelectionClears(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		msg  tea.Msg
		kept bool
	}{
		{"key", keyPress("j"), false},
		{"width", tea.WindowSizeMsg{Width: 120, Height: 40}, false},
		{"height", tea.WindowSizeMsg{Width: 140, Height: 30}, true},
	} {
		h := newHarness(t, longRun(t, "first message"), nil)
		h.keys("]", "3")
		selectRows(t, h, "first message", "first message")
		if h.m.textSel.Empty() {
			t.Fatalf("%s: no selection", tc.name)
		}
		h.send(tc.msg)
		if kept := !h.m.textSel.Empty(); kept != tc.kept {
			t.Errorf("%s: kept the selection = %v; want %v", tc.name, kept, tc.kept)
		}
	}
}

// The journal and trace tabs are lists drawn for each frame, so their
// selection marks screen cells: anything that moves the list drops it.
func TestConsoleListSelectionClearsOnScrollAndKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		msg  func(h *harness) tea.Msg
	}{
		{"wheel", func(h *harness) tea.Msg {
			return tea.MouseWheelMsg{X: h.m.mainBody().x, Y: h.m.mainBody().y, Button: tea.MouseWheelDown}
		}},
		{"key", func(*harness) tea.Msg { return keyPress("j") }},
		{"resize", func(*harness) tea.Msg { return tea.WindowSizeMsg{Width: 140, Height: 30} }},
	} {
		h := newHarness(t, twoRuns(t), nil)
		h.send(tea.MouseClickMsg{X: h.m.tabX[tabJournal] + 1, Y: h.m.rTabs.y, Button: tea.MouseLeft})
		body := h.m.mainBody()
		h.update(tea.MouseClickMsg{X: body.x, Y: body.y, Button: tea.MouseLeft})
		h.update(tea.MouseMotionMsg{X: body.x + 4, Y: body.y, Button: tea.MouseLeft})
		h.update(tea.MouseReleaseMsg{X: body.x + 4, Y: body.y, Button: tea.MouseLeft})
		if h.m.textSel.Empty() || h.m.selScroll {
			t.Fatalf("%s: no list selection to clear", tc.name)
		}
		h.send(tc.msg(h))
		if !h.m.textSel.Empty() {
			t.Errorf("%s kept the selection", tc.name)
		}
	}
}

// A bracketed paste reaches the field that has the keys.
func TestConsolePasteIntoFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.keys("/")
	h.send(tea.PasteMsg{Content: "run-0"})
	if !h.m.filtering || h.m.filter.Value() != "run-0" {
		t.Fatalf("filter = %q; want the pasted text", h.m.filter.Value())
	}
	if len(h.m.runs) != 1 || h.m.runs[0].ID != "run-0" {
		t.Errorf("the paste did not filter the runs: %d runs", len(h.m.runs))
	}
}

// A paste into the send dialog keeps its newlines and does not submit.
func TestConsolePasteIntoModal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), &fakeServer{})
	h.keys("n")
	if h.m.modal == nil {
		t.Fatal("no send dialog")
	}
	h.send(tea.PasteMsg{Content: "first\nsecond"})
	if h.m.modal == nil {
		t.Fatal("the paste closed the dialog")
	}
	if got := h.m.modal.input.Value(); got != "first\nsecond" {
		t.Errorf("input = %q; want the pasted text", got)
	}
}
