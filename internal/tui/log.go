package tui

import (
	"bytes"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// logMsg carries complete lines of process output into the transcript.
type logMsg struct{ lines []string }

// LogWriter shows process output in the transcript of a running TUI.
//
// The TUI owns the terminal while it runs. A write to stderr from the same
// process, or from a child that shares it, lands at the cursor position of
// the alternate screen: `bonnie dev` printed each hot-reload message and the
// child's startup banner across the transcript. Give a LogWriter to each of
// those writers instead.
//
// Lines written before [LogWriter.Attach] are held and shown when the TUI
// starts, so the first child's banner is not lost. Lines written after
// [LogWriter.Detach] go to the fallback writer, because the terminal is free
// again. A partial line is held until its newline arrives.
type LogWriter struct {
	mu       sync.Mutex
	fallback io.Writer
	program  *tea.Program
	detached bool
	held     []string
	partial  []byte
}

// NewLogWriter returns a LogWriter that writes to fallback when no TUI is
// attached.
func NewLogWriter(fallback io.Writer) *LogWriter {
	return &LogWriter{fallback: fallback}
}

// Attach sends held lines, and each later line, to p. Call it before p.Run.
func (w *LogWriter) Attach(p *tea.Program) {
	w.mu.Lock()
	w.program = p
	held := w.held
	w.held = nil
	if len(held) == 0 {
		w.mu.Unlock()
		return
	}
	// Send blocks until the event loop runs, so it cannot run here: the
	// caller starts the loop after Attach returns. The goroutine keeps the
	// lock until the held lines are sent, so a later line cannot pass them.
	go func() {
		defer w.mu.Unlock()
		p.Send(logMsg{lines: held})
	}()
}

// Detach sends each later line to the fallback writer. Call it after the
// program has returned. A partial line still held is written then.
func (w *LogWriter) Detach() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.program = nil
	w.detached = true
	for _, line := range w.held {
		_, _ = io.WriteString(w.fallback, line+"\n")
	}
	w.held = nil
	if len(w.partial) > 0 {
		_, _ = w.fallback.Write(append(w.partial, '\n'))
		w.partial = nil
	}
}

// Write implements io.Writer.
func (w *LogWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.detached {
		return w.fallback.Write(b)
	}
	w.partial = append(w.partial, b...)
	var lines []string
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(bytes.TrimRight(w.partial[:i], "\r")))
		w.partial = w.partial[i+1:]
	}
	if len(lines) == 0 {
		return len(b), nil
	}
	if w.program == nil {
		w.held = append(w.held, lines...)
		return len(b), nil
	}
	// Send returns when the program reads the message or has ended, so a
	// line written while the TUI exits is dropped, never blocked on.
	w.program.Send(logMsg{lines: lines})
	return len(b), nil
}
