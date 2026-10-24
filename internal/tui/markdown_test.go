package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mark3labs/bonnie/runtime"
)

// TestRenderMarkdownKeepsWordsAndDropsMarkers: a closed assistant entry
// renders as markdown — the words survive, the markup markers do not.
func TestRenderMarkdownKeepsWordsAndDropsMarkers(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.commitUser("hi")
	m.appendAssistant("**bold** start\n\n- item one\n- item two\n")
	m.apply(runtime.Event{Seq: 2, Type: runtime.EventResponse, Text: "**bold** start\n\n- item one\n- item two\n"})

	rendered := m.transcript()
	for _, want := range []string{"bold", "start", "item one", "item two"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("transcript misses %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "**") {
		t.Errorf("transcript leaks markdown markers:\n%s", rendered)
	}
}

// TestRenderMarkdownWrapsInsteadOfTruncating pins the wrap: the old
// MaxWidth(100) style truncated any assistant line past 100 cells and the
// tail was lost. herald output is wrapped with lipgloss.Wrap, so a long
// paragraph becomes several lines and keeps every word.
func TestRenderMarkdownWrapsInsteadOfTruncating(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 100)
	got := renderMarkdown(content, 40)

	lines := strings.Split(got, "\n")
	if len(lines) <= 1 {
		t.Fatalf("long prose did not wrap:\n%s", got)
	}
	if !strings.Contains(got, "word") || strings.Count(got, "word") != 100 {
		t.Errorf("wrapped render lost words: %d of 100 remain", strings.Count(got, "word"))
	}
}

// TestRenderMarkdownEmptyIsEmpty keeps an empty entry from adding a blank
// transcript line.
func TestRenderMarkdownEmptyIsEmpty(t *testing.T) {
	t.Parallel()
	if got := renderMarkdown("", 80); got != "" {
		t.Errorf("renderMarkdown(\"\") = %q, want empty", got)
	}
	if got := renderMarkdown("   \n\t", 80); got != "" {
		t.Errorf("renderMarkdown(whitespace) = %q, want empty", got)
	}
}

// TestClosedAssistantEntryIsCachedOnce: transcript() rebuilds every frame,
// so a closed entry must render once and reuse the cache after — even when
// the underlying text would render differently. The render does not depend
// on the width, so a resize reuses it too; the viewport wraps the transcript
// to the new width.
func TestClosedAssistantEntryIsCachedOnce(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.commit(kindAssistant, "first")
	if got := m.transcript(); !strings.Contains(got, "first") {
		t.Fatalf("transcript misses the entry:\n%s", got)
	}
	if m.entries[0].rendered == "" {
		t.Fatalf("entry = %+v, want a cached render", m.entries[0])
	}

	// The cache answers even though the text changed underneath, also at a
	// new width.
	e := m.entries[0]
	e.text = "changed"
	m.entries[0] = e
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	m = next.(Model)
	if got := m.transcript(); strings.Contains(got, "changed") {
		t.Errorf("cache was not used:\n%s", got)
	}
}

// TestTranscriptWrapsToTheWidth: entries render unwrapped, so the viewport
// must wrap them. A long assistant paragraph fills rows no wider than the
// terminal and keeps every word.
func TestTranscriptWrapsToTheWidth(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = next.(Model)
	m.commit(kindAssistant, strings.Repeat("word ", 100))
	m.syncViewport()
	if len(m.rows.Rows) < 10 {
		t.Fatalf("long prose made %d rows at width 40", len(m.rows.Rows))
	}
	words := 0
	for _, row := range m.rows.Rows {
		if w := ansi.StringWidth(row); w > 40 {
			t.Errorf("row is %d cells wide; want at most 40: %q", w, row)
		}
		words += strings.Count(row, "word")
	}
	if words != 100 {
		t.Errorf("wrapped transcript holds %d of 100 words", words)
	}
}

// TestFinishAssistantInvalidatesCache: the durable final response replaces
// the streamed draft, so any cached render of the draft must go.
func TestFinishAssistantInvalidatesCache(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.appendAssistant("draft")
	m.apply(runtime.Event{Seq: 2, Type: runtime.EventResponse, Text: "final"})
	if m.entries[0].rendered != "" {
		t.Errorf("cached render = %q, want it dropped when the text changed",
			m.entries[0].rendered)
	}
	rendered := m.transcript()
	if !strings.Contains(rendered, "final") || strings.Contains(rendered, "draft") {
		t.Errorf("transcript shows the wrong text:\n%s", rendered)
	}
}

// TestUserMessageIsNotMarkdownRendered: typed text is not markdown. A user
// message that happens to hold markdown-looking characters must show them
// exactly as typed.
func TestUserMessageIsNotMarkdownRendered(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.commitUser("**not bold** and *not italic*")
	rendered := m.transcript()
	if !strings.Contains(rendered, "**not bold** and *not italic*") {
		t.Errorf("user message was rewritten:\n%s", rendered)
	}
}

// TestStreamedAssistantEntryRendersLive: the open entry renders markdown
// every frame rather than waiting for the turn to close.
func TestStreamedAssistantEntryRendersLive(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.appendAssistant("streaming **text**")
	rendered := m.transcript()
	if !strings.Contains(rendered, "streaming") || strings.Contains(rendered, "**") {
		t.Errorf("open entry did not render as live markdown:\n%s", rendered)
	}
}
