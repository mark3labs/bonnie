package runtime

import (
	"context"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

func user(text string) kit.LLMMessage { return kit.NewLLMUserMessage(text) }

func assistant(text string) kit.LLMMessage {
	return kit.LLMMessage{
		Role:    kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: text}},
	}
}

func TestSessionSatisfiesKitContract(t *testing.T) {
	t.Parallel()
	// The compile-time assertion lives in session.go; this test documents why
	// it matters and fails loudly if the interface is ever widened.
	var sm kit.SessionManager = NewSession("s", NewMemoryJournal())
	if sm.GetSessionID() != "s" {
		t.Fatalf("GetSessionID = %q", sm.GetSessionID())
	}
}

func TestAppendAndReadBranch(t *testing.T) {
	t.Parallel()
	s := NewSession("s1", NewMemoryJournal())

	if _, err := s.AppendMessage(user("one")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.AppendMessage(assistant("two")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	msgs := s.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if messageText(msgs[0]) != "one" || messageText(msgs[1]) != "two" {
		t.Fatalf("branch order wrong: %q, %q", messageText(msgs[0]), messageText(msgs[1]))
	}
}

func TestBranchingForksConversation(t *testing.T) {
	t.Parallel()
	s := NewSession("s2", NewMemoryJournal())

	first, _ := s.AppendMessage(user("root"))
	if _, err := s.AppendMessage(assistant("left")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Fork from the root and take a different path.
	if err := s.Branch(first); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if _, err := s.AppendMessage(assistant("right")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	msgs := s.GetMessages()
	if len(msgs) != 2 || messageText(msgs[1]) != "right" {
		t.Fatalf("branch tip wrong: %+v", msgs)
	}
	if kids := s.GetChildren(first); len(kids) != 2 {
		t.Fatalf("got %d children, want 2", len(kids))
	}
}

func TestBranchUnknownEntry(t *testing.T) {
	t.Parallel()
	s := NewSession("s3", NewMemoryJournal())
	if err := s.Branch("missing"); err == nil {
		t.Fatal("want error for unknown entry")
	}
}

func TestCompactionReplacesHistory(t *testing.T) {
	t.Parallel()
	s := NewSession("s4", NewMemoryJournal())

	_, _ = s.AppendMessage(user("old one"))
	_, _ = s.AppendMessage(assistant("old two"))
	keep, _ := s.AppendMessage(user("keep me"))

	if _, err := s.AppendCompaction("SUMMARY", keep, 100, 10, 2, nil, nil); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}

	msgs, _, _ := s.BuildContext()
	if len(msgs) != 2 {
		t.Fatalf("got %d context messages, want summary + kept", len(msgs))
	}
	if messageText(msgs[0]) != "SUMMARY" {
		t.Fatalf("first context message = %q, want the summary", messageText(msgs[0]))
	}
	if messageText(msgs[1]) != "keep me" {
		t.Fatalf("second context message = %q", messageText(msgs[1]))
	}
	if c := s.GetLastCompaction(); c == nil || c.Summary != "SUMMARY" {
		t.Fatalf("GetLastCompaction = %+v", c)
	}
}

func TestExtensionDataIsDurableState(t *testing.T) {
	t.Parallel()
	s := NewSession("s5", NewMemoryJournal())

	if _, err := s.AppendExtensionData("counter", `{"n":1}`); err != nil {
		t.Fatalf("AppendExtensionData: %v", err)
	}
	if _, err := s.AppendExtensionData("other", `{"x":true}`); err != nil {
		t.Fatalf("AppendExtensionData: %v", err)
	}

	if got := s.GetExtensionData("counter"); len(got) != 1 || got[0].Data != `{"n":1}` {
		t.Fatalf("GetExtensionData(counter) = %+v", got)
	}
	if got := s.GetExtensionData(""); len(got) != 2 {
		t.Fatalf("GetExtensionData(all) returned %d, want 2", len(got))
	}
}

func TestRestoreRebuildsConversation(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()
	ctx := context.Background()

	orig := NewSession("s6", journal)
	_, _ = orig.AppendMessage(user("first"))
	_, _ = orig.AppendMessage(assistant("second"))

	restored, err := Restore(ctx, "s6", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	msgs := restored.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("restored %d messages, want 2", len(msgs))
	}
	if messageText(msgs[0]) != "first" || messageText(msgs[1]) != "second" {
		t.Fatalf("restored content wrong: %+v", msgs)
	}
}

func TestIsPersistedTracksJournalKind(t *testing.T) {
	t.Parallel()
	if NewSession("a", NewMemoryJournal()).IsPersisted() {
		t.Fatal("memory journal must report ephemeral")
	}
}
