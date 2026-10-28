package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestLogWriterHoldsLinesUntilAttached: the first child's banner is written
// before the TUI starts. It must reach the transcript, in order, and not the
// terminal.
func TestLogWriterHoldsLinesUntilAttached(t *testing.T) {
	t.Parallel()
	var fallback bytes.Buffer
	w := NewLogWriter(&fallback)
	_, _ = w.Write([]byte("bonnie: serving on 127.0.0.1:8081\nbonnie: jour"))
	_, _ = w.Write([]byte("nal .bonnie\n"))

	got := make(chan []string, 4)
	p := tea.NewProgram(logRecorder{got: got},
		tea.WithContext(t.Context()), tea.WithInput(nil), tea.WithOutput(&bytes.Buffer{}),
		tea.WithoutRenderer(), tea.WithoutSignals())
	w.Attach(p)
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	// A line written after Attach waits until the held lines are sent; it
	// must not pass them.
	_, _ = w.Write([]byte("bonnie: dev: change detected, rebuilding\n"))

	var lines []string
	for len(lines) < 3 {
		select {
		case l := <-got:
			lines = append(lines, l...)
		case <-time.After(5 * time.Second):
			t.Fatalf("lines in transcript = %q, want 3", lines)
		}
	}
	want := []string{"bonnie: serving on 127.0.0.1:8081", "bonnie: journal .bonnie", "bonnie: dev: change detected, rebuilding"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	p.Quit()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	w.Detach()
	_, _ = w.Write([]byte("bonnie: shutting down\n"))
	if fallback.String() != "bonnie: shutting down\n" {
		t.Fatalf("fallback = %q, want only the line after Detach", fallback.String())
	}
}

// TestLogWriterDetachFlushesHeldLines: a TUI that never attached must not
// swallow output, such as a build error before the first child started.
func TestLogWriterDetachFlushesHeldLines(t *testing.T) {
	t.Parallel()
	var fallback bytes.Buffer
	w := NewLogWriter(&fallback)
	_, _ = w.Write([]byte("line one\npartial"))
	w.Detach()
	if fallback.String() != "line one\npartial\n" {
		t.Fatalf("fallback = %q", fallback.String())
	}
}

// TestLogLinesStayBeforeTheStreamingEntry: each chunk is added to the last
// entry. A log line put after an open assistant entry would take the rest of
// the reply.
func TestLogLinesStayBeforeTheStreamingEntry(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.openEntry()
	m.appendAssistant("Your shop")
	next, _ := m.Update(logMsg{lines: []string{"bonnie: dev: change detected, rebuilding"}})
	m = next.(Model)
	m.appendAssistant(" is open.")

	n := len(m.entries)
	if m.entries[n-1].kind != kindAssistant || m.entries[n-1].text != "Your shop is open." {
		t.Fatalf("last entry = %+v, want the whole reply", m.entries[n-1])
	}
	if m.entries[n-2].kind != kindLog {
		t.Fatalf("entry before the reply = %+v, want the log line", m.entries[n-2])
	}
}

type logRecorder struct{ got chan<- []string }

func (r logRecorder) Init() tea.Cmd { return nil }

func (r logRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if l, ok := msg.(logMsg); ok {
		r.got <- l.lines
	}
	return r, nil
}

func (r logRecorder) View() tea.View { return tea.NewView("") }
