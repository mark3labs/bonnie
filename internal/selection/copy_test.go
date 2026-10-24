package selection

import (
	"context"
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// isSetClipboard reports whether msg is tea.SetClipboard's message, whose
// type is not exported.
func isSetClipboard(msg tea.Msg, text string) bool {
	v := reflect.ValueOf(msg)
	return v.IsValid() && v.Type().Name() == "setClipboardMsg" && v.String() == text
}

// Outside tmux, a copy is OSC 52 only. The test sets an environment
// variable, so it cannot run in parallel.
func TestCopyOutsideTmuxIsOSC52(t *testing.T) {
	t.Setenv("TMUX", "")
	called := false
	loadTmuxBuffer = func(context.Context, string) error { called = true; return nil }
	t.Cleanup(func() { loadTmuxBuffer = defaultLoadTmuxBuffer })

	if msg := Copy("text")(); !isSetClipboard(msg, "text") {
		t.Fatalf("Copy sent %T; want tea.SetClipboard", msg)
	}
	if called {
		t.Error("Copy called tmux outside tmux")
	}
	if Copy("") != nil {
		t.Error("Copy of empty text returned a command")
	}
}

// tmux drops OSC 52 from an application at its default set-clipboard
// "external", so inside tmux a copy also loads the tmux buffer.
func TestCopyInsideTmuxLoadsTheBuffer(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	var got string
	loadTmuxBuffer = func(_ context.Context, text string) error { got = text; return nil }
	t.Cleanup(func() { loadTmuxBuffer = defaultLoadTmuxBuffer })

	batch, ok := Copy("text")().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("Copy inside tmux sent %T; want a batch of OSC 52 and tmux", batch)
	}
	if !isSetClipboard(batch[0](), "text") {
		t.Error("the batch does not start with OSC 52")
	}
	if msg := batch[1](); msg != nil {
		t.Errorf("the tmux command sent %T; want no message", msg)
	}
	if got != "text" {
		t.Errorf("tmux got %q; want %q", got, "text")
	}
}
