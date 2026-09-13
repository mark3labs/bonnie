package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

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

// TestClosedAssistantEntryIsCachedPerWidth: transcript() rebuilds every frame,
// so a closed entry must render once per width and reuse the cache after —
// even when the underlying text would render differently.
func TestClosedAssistantEntryIsCachedPerWidth(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.commit(kindAssistant, "first")
	if got := m.transcript(); !strings.Contains(got, "first") {
		t.Fatalf("transcript misses the entry:\n%s", got)
	}
	e := m.entries[0]
	if e.rendered == "" || e.renderedWidth != m.textWidth() {
		t.Fatalf("entry = %+v, want a cached render at width %d", e, m.textWidth())
	}

	// Same width: the cache answers even though the text changed underneath.
	e.text = "changed"
	m.entries[0] = e
	if got := m.transcript(); strings.Contains(got, "changed") {
		t.Errorf("cache was not used:\n%s", got)
	}

	// A new width re-renders.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	m = next.(Model)
	if got := m.transcript(); !strings.Contains(got, "changed") {
		t.Errorf("a new width did not re-render:\n%s", got)
	}
	if m.entries[0].renderedWidth != 58 {
		t.Errorf("renderedWidth = %d, want 58 (width 60 less the margin)",
			m.entries[0].renderedWidth)
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
