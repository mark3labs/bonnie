package evalui

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mark3labs/bonnie/runtime"
)

// sized is the sample session on a screen of 140×30.
func sized(t *testing.T) model {
	t.Helper()
	m := newModel(context.Background(), sampleSession(t, "/j/TestA"), "")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	return next.(model)
}

// send gives msg to the model and returns the command without running it.
func send(m model, msg tea.Msg) (model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(model), cmd
}

// cellOf returns the screen cell where text starts, in the column range
// [from, to) of the screen.
func cellOf(t *testing.T, m model, text string, from int) (x, y int) {
	t.Helper()
	for y, row := range strings.Split(screen(m), "\n") {
		if from > 0 {
			row = ansi.Cut(row, from, ansi.StringWidth(row))
		}
		if before, _, ok := strings.Cut(row, text); ok {
			return from + ansi.StringWidth(before), y
		}
	}
	t.Fatalf("%q is not on the screen:\n%s", text, screen(m))
	return 0, 0
}

// clipboard runs cmd and returns the text it puts on the clipboard, as
// copied does in internal/runsui. tea.SetClipboard's message type is not
// exported, so it is matched by name.
func clipboard(t *testing.T, cmd tea.Cmd) string {
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

func click(m model, x, y int) model {
	m, _ = send(m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m, _ = send(m, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	return m
}

// A click on an eval selects it, and a click on a tab shows it, as in
// `bonnie runs`. Each click moves the focus to its panel.
func TestMouseClickSelectsEvalAndTab(t *testing.T) {
	t.Parallel()
	m := sized(t)

	x, y := cellOf(t, m, "TestB/case", 0)
	m = click(m, x, y)
	if m.selName != "TestB/case" || m.focus != focusList {
		t.Fatalf("after a click on TestB/case: selected %q, focus %d", m.selName, m.focus)
	}

	x, y = cellOf(t, m, "2 Conversation", m.listWidth())
	m = click(m, x+2, y)
	if m.tab != tabConversation || m.focus != focusDetail {
		t.Fatalf("after a click on the tab: tab %v, focus %d", m.tab, m.focus)
	}

	// A click on the border or the header does nothing.
	before := m.selName
	m = click(m, 0, 0)
	if m.selName != before || m.tab != tabConversation {
		t.Fatalf("a click on the header changed the screen: %q %v", m.selName, m.tab)
	}
}

// A drag in the detail text copies it and highlights it. A key drops the
// selection.
func TestMouseDragCopiesDetailText(t *testing.T) {
	t.Parallel()
	m := sized(t)
	x, y := cellOf(t, m, "journal /j/TestA", m.listWidth())
	end := x + len("journal /j/TestA") - 1
	m, _ = send(m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m, _ = send(m, tea.MouseMotionMsg{X: end, Y: y, Button: tea.MouseLeft})
	m, cmd := send(m, tea.MouseReleaseMsg{X: end, Y: y, Button: tea.MouseLeft})
	if got := clipboard(t, cmd); got != "journal /j/TestA" {
		t.Errorf("copied %q; want %q", got, "journal /j/TestA")
	}
	if !strings.Contains(m.View().Content, "\x1b[7m") {
		t.Error("the copied text is not highlighted")
	}
	if !strings.Contains(screen(m), "copied 16 characters") {
		t.Errorf("the footer does not report the copy:\n%s", screen(m))
	}

	m, _ = press(t, m, "j")
	if !m.textSel.Empty() || strings.Contains(m.View().Content, "\x1b[7m") {
		t.Error("a key did not drop the selection")
	}
}

// A press in the detail text that does not move is a click, not a copy.
func TestMouseClickInDetailDoesNotCopy(t *testing.T) {
	t.Parallel()
	m := sized(t)
	x, y := cellOf(t, m, "polite", m.listWidth())
	m, _ = send(m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m, cmd := send(m, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if !m.textSel.Empty() || m.focus != focusDetail {
		t.Fatalf("a click selected text or did not focus the detail: %+v, focus %d", m.textSel, m.focus)
	}
	if cmd != nil {
		if msg := cmd(); msg != nil && reflect.ValueOf(msg).Type().Name() == "setClipboardMsg" {
			t.Fatal("a click wrote the clipboard")
		}
	}
}

// The wheel acts on the panel under the mouse, not on the focused one.
func TestMouseWheelFollowsThePointer(t *testing.T) {
	t.Parallel()
	m := sized(t)
	m, _ = press(t, m, "tab")
	if m.focus != focusDetail {
		t.Fatal("tab did not focus the detail")
	}
	m, _ = send(m, tea.MouseWheelMsg{X: 2, Y: 5, Button: tea.MouseWheelDown})
	if m.selName != "TestB/case" {
		t.Fatalf("the wheel over the list selected %q", m.selName)
	}
}

// The scores name the agent's model, from the steps of its journal, and
// each judge's model, from the reports.
func TestScoresNameTheModels(t *testing.T) {
	t.Parallel()
	step := func(model string) runtime.Record {
		b, err := json.Marshal(map[string]string{"kind": "step", "prompt": "step 1", "model": model})
		if err != nil {
			t.Fatal(err)
		}
		return runtime.Record{Kind: runtime.RecordStep, Payload: b}
	}
	models := stepModels([]runtime.Record{step("anthropic/claude-sonnet-4-5"), step("anthropic/claude-sonnet-4-5"), {Kind: runtime.RecordStep}, step("openrouter/qwen/qwen3")})
	if strings.Join(models, ",") != "anthropic/claude-sonnet-4-5,openrouter/qwen/qwen3" {
		t.Fatalf("step models = %q", models)
	}

	m := sized(t)
	m.convs["/j/TestA"] = &conversationState{models: models[:1]}
	r := Result{Name: "TestA", Status: StatusPass, Journal: "/j/TestA", Reports: []Report{
		{Label: "polite", Kind: "judge", Status: "passed", Model: "openrouter/typesafe/jev-1.14"},
		{Label: "clear", Kind: "llm", Status: "passed", Model: "openrouter/anthropic/claude-sonnet-4.5"},
		{Label: "steps", Kind: "score", Status: "recorded"},
	}}
	text := ansi.Strip(strings.Join(m.scoreLines(r, 100), "\n"))
	for _, want := range []string{"agent     anthropic/claude-sonnet-4-5", "judge     openrouter/typesafe/jev-1.14", "llm judge openrouter/anthropic/claude-sonnet-4.5"} {
		if !strings.Contains(text, want) {
			t.Errorf("scores do not contain %q:\n%s", want, text)
		}
	}

	// With no journal and no judge model, no model line shows.
	if lines := m.modelLines(Result{Name: "TestB"}, 100); len(lines) != 0 {
		t.Errorf("model lines = %q, want none", lines)
	}
}
