package runtime

import (
	"context"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// TestSandboxRecordSurvivesRestore covers the record's whole purpose: a run
// that opened a sandbox can, after a resume in a fresh process, say which
// backend it used and which sandbox held its workspace.
func TestSandboxRecordSurvivesRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()

	s1 := NewSession("sandbox-record", j)
	if err := s1.RecordSandboxOpen(ctx, "docker", "bonnie-sandbox-record"); err != nil {
		t.Fatalf("RecordSandboxOpen: %v", err)
	}

	s2, err := Restore(ctx, "sandbox-record", j)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	backend, id, gone, ok := s2.LastSandbox()
	if !ok {
		t.Fatal("the sandbox record did not survive the restore")
	}
	if backend != "docker" || id != "bonnie-sandbox-record" {
		t.Fatalf("backend=%q id=%q, want docker / bonnie-sandbox-record", backend, id)
	}
	if gone {
		t.Fatal("a live sandbox must not read as gone")
	}
}

// TestNoteSandboxUnavailableIsANoteNotASilence covers the decision recorded
// in docs/SPEC.md §4.10: a vanished workspace is not a failure, but it must
// never be silent. The model gets one note, the journal gets one gone
// record, and a second discovery appends nothing.
func TestNoteSandboxUnavailableIsANoteNotASilence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()

	s := NewSession("sandbox-gone", j)
	if err := s.RecordSandboxOpen(ctx, "docker", "bonnie-sandbox-gone"); err != nil {
		t.Fatalf("RecordSandboxOpen: %v", err)
	}
	if err := s.NoteSandboxUnavailable(ctx, "docker", "bonnie-sandbox-gone", true); err != nil {
		t.Fatalf("NoteSandboxUnavailable: %v", err)
	}

	before := len(mustReplay(t, j, "sandbox-gone"))
	if err := s.NoteSandboxUnavailable(ctx, "docker", "bonnie-sandbox-gone", true); err != nil {
		t.Fatalf("second note: %v", err)
	}
	if after := len(mustReplay(t, j, "sandbox-gone")); after != before {
		t.Fatalf("%d records after a repeated note, want the same %d: "+
			"the loss is reported once", after, before)
	}

	// The note must reach the model: a user-role message, journalled with
	// its payload like any other message.
	msgs := s.GetMessages()
	if len(msgs) == 0 {
		t.Fatal("the conversation holds no note")
	}
	last := msgs[len(msgs)-1]
	if last.Role != kit.LLMMessageRole("user") {
		t.Fatalf("note role = %q, want user: a system-role note mid-conversation "+
			"is rejected by some providers", last.Role)
	}
	if !strings.Contains(messageText(last), "no longer available") {
		t.Fatalf("note does not say the workspace is gone: %q", messageText(last))
	}

	// A resume in a fresh process sees the loss as already reported, so it
	// does not note it a second time.
	restored, err := Restore(ctx, "sandbox-gone", j)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, _, gone, ok := restored.LastSandbox(); !ok || !gone {
		t.Fatalf("restored sandbox gone=%v ok=%v, want gone", gone, ok)
	}
	recordsBefore := len(mustReplay(t, j, "sandbox-gone"))
	if err := restored.NoteSandboxUnavailable(ctx, "docker", "bonnie-sandbox-gone", true); err != nil {
		t.Fatalf("note after restore: %v", err)
	}
	if recordsAfter := len(mustReplay(t, j, "sandbox-gone")); recordsAfter != recordsBefore {
		t.Fatalf("a resumed run noted the same loss twice")
	}
}

// TestUnverifiedLossIsANoteWithoutAGoneRecord pins the honesty of the two
// report shapes: when the loss is not verified — the record points at a
// backend this host no longer uses — the note lands but the journal gets no
// gone record, because nothing verified that the sandbox is gone.
func TestUnverifiedLossIsANoteWithoutAGoneRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()

	s := NewSession("sandbox-mismatch", j)
	if err := s.RecordSandboxOpen(ctx, "docker", "bonnie-sandbox-mismatch"); err != nil {
		t.Fatalf("RecordSandboxOpen: %v", err)
	}
	if err := s.NoteSandboxUnavailable(ctx, "docker", "bonnie-sandbox-mismatch", false); err != nil {
		t.Fatalf("NoteSandboxUnavailable: %v", err)
	}

	_, _, gone, ok := s.LastSandbox()
	if !ok || gone {
		t.Fatalf("an unverified loss must not read as verified (gone=%v ok=%v)", gone, ok)
	}
	if msgs := s.GetMessages(); len(msgs) == 0 ||
		!strings.Contains(messageText(msgs[len(msgs)-1]), "no longer available") {
		t.Fatal("the note did not reach the conversation")
	}
}

// TestSandboxRecordIsNotATreeEntry pins that a sandbox record does not join
// the conversation tree: it is metadata, and a message appended after it must
// chain to the previous message, not to the record.
func TestSandboxRecordIsNotATreeEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()

	s := NewSession("sandbox-tree", j)
	if _, err := s.AppendMessage(kit.NewLLMUserMessage("first")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.RecordSandboxOpen(ctx, "docker", "bonnie-sandbox-tree"); err != nil {
		t.Fatalf("RecordSandboxOpen: %v", err)
	}
	if _, err := s.AppendMessage(kit.NewLLMUserMessage("second")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	restored, err := Restore(ctx, "sandbox-tree", j)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	msgs := restored.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("%d messages restored, want the 2 real ones — the sandbox "+
			"record leaked into the conversation", len(msgs))
	}
	if got := messageText(msgs[1]); got != "second" {
		t.Fatalf("second message = %q, want \"second\": the record broke the chain", got)
	}
}
