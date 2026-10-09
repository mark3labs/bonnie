package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mark3labs/bonnie/runtime"
)

func command(m Model, text string) (Model, tea.Cmd) {
	m.input.SetValue(text)
	next, cmd := m.Update(keyEnter())
	return next.(Model), cmd
}

// Resize uses the full width and keeps the complete frame inside the terminal.
func TestResponsiveFrame(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.commitUser(strings.Repeat("wide text ", 50))
	m.commit(kindAssistant, strings.Repeat("assistant text ", 50))
	for _, width := range []int{160, 45, 12, 160} {
		next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
		m = next.(Model)
		v := m.View()
		if m.textWidth() != width || m.viewport.Width() != width {
			t.Fatalf("widths = %d, %d, want %d", m.textWidth(), m.viewport.Width(), width)
		}
		if lipgloss.Width(v.Content) > width || lipgloss.Height(v.Content) != 20 {
			t.Fatalf("frame = %dx%d at width %d", lipgloss.Width(v.Content), lipgloss.Height(v.Content), width)
		}
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 3})
	if v := next.(Model).View(); lipgloss.Height(v.Content) > 3 || v.Cursor.Y >= 3 {
		t.Fatal("tiny window lost its input")
	}
}

// New output follows the bottom but does not interrupt a reader who scrolls up.
func TestViewportScrollAndFollow(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	for i := range 100 {
		m.commitUser(fmt.Sprintf("line %d", i))
	}
	m.syncViewport()
	if !m.viewport.AtBottom() {
		t.Fatal("did not follow output")
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = next.(Model)
	offset := m.viewport.YOffset()
	if m.viewport.AtBottom() {
		t.Fatal("page up did not scroll")
	}
	next, _ = m.Update(streamMsg{ev: runtime.Event{Type: runtime.EventResponse, Text: "new output"}})
	m = next.(Model)
	if m.viewport.YOffset() != offset {
		t.Fatal("output moved the reader")
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	m = next.(Model)
	if !m.viewport.AtBottom() {
		t.Fatal("ctrl+end did not return to live output")
	}
	next, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	m = next.(Model)
	if m.viewport.AtBottom() {
		t.Fatal("mouse wheel did not scroll")
	}
	m, _ = command(m, "/help")
	if got := m.entries[len(m.entries)-1].text; !strings.HasPrefix(got, "Help\n\n/new —") {
		t.Fatalf("help must put the heading before the command list: %q", got)
	}
	if !strings.Contains(m.transcript(), "/retry") {
		t.Fatal("missing command help")
	}
}

// A new address leaves journal history intact and rejects late stream results.
func TestNewConversationRejectsOldResults(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.runID = "old-run"
	m.commitUser("old message")
	closed := false
	m.streamUn = func() { closed = true }
	next, _ := command(m, "/new")
	if !closed || next.address == m.address || next.runID != "" || len(next.entries) != 0 || next.loading {
		t.Fatal("new did not reset the conversation")
	}
	staleClosed := false
	updated, _ := next.Update(streamReadyMsg{unsub: func() { staleClosed = true }})
	next = updated.(Model)
	if !staleClosed {
		t.Fatal("stale stream was not closed")
	}
	for _, msg := range []tea.Msg{
		lookupMsg{runID: "old-run"},
		runMsg{run: &runtime.Run{ID: "old-run", State: runtime.RunCompleted, Response: "old answer"}},
		streamMsg{ev: runtime.Event{Type: runtime.EventResponse, Text: "old answer"}},
		streamEndMsg{}, reconnectMsg{},
	} {
		updated, cmd := next.Update(msg)
		next = updated.(Model)
		if cmd != nil || next.runID != "" || len(next.entries) != 0 {
			t.Fatal("accepted an old result")
		}
	}
	next, cmd := command(next, "hello")
	if cmd == nil || next.pending != "hello" {
		t.Fatal("new message did not start the handshake")
	}
}

func TestSlashCommands(t *testing.T) {
	t.Parallel()
	f := &fakeClient{}
	m := newTestModel(f)
	m.runID = "run-1"
	m.commitUser("repeat me")
	m.commit(kindAssistant, "answer")
	next, cmd := command(m, "/retry")
	if cmd == nil || !next.spin || next.entries[len(next.entries)-1].text != "repeat me" {
		t.Fatal("retry did not resend the last user message")
	}
	next = drive(t, next, cmd)
	if f.turns != 1 {
		t.Fatalf("turns = %d", f.turns)
	}
	next.spin = true
	blocked, _ := command(next, "/new")
	if blocked.address != next.address {
		t.Fatal("new left an active turn")
	}
	blocked, _ = command(next, "/retry")
	if len(blocked.entries) != len(next.entries) {
		t.Fatal("retry sent during an active turn")
	}
	_, cmd = command(next, "/cancel")
	if cmd == nil {
		t.Fatal("cancel produced no request")
	}
	_ = cmd()
	if f.cancelled != 1 {
		t.Fatal("cancel did not reach the server")
	}
	_, cmd = command(next, "/exit")
	if cmd == nil {
		t.Fatal("exit did not quit during a turn")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("exit did not produce quit")
	}
	m, cmd = command(m, "/unknown")
	if cmd != nil || !strings.Contains(m.label, "unknown command") {
		t.Fatal("unknown command was sent as a prompt")
	}
}

// Multiline input does not dispatch a turn until Enter is pressed.
func TestShiftEnterInsertsNewline(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.input.SetValue("first")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	m = next.(Model)
	if m.input.Value() != "first\n" || m.spin {
		t.Fatalf("input = %q, spin = %v", m.input.Value(), m.spin)
	}
}
