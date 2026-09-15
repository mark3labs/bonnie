package runtime

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// tornSession journals a step whose tool result never arrived, which is what a
// crash between Kit's two AppendMessage calls leaves behind.
func tornSession(t *testing.T, runID string) Journal {
	t.Helper()
	j := NewMemoryJournal()
	s := NewSession(runID, j)
	mustAppend(t, s, user("deploy the app"))
	mustAppend(t, s, toolCall("Deploying.", "c1", "deploy", `{"region":"eu"}`))
	// The tool message is never written — the process died here.
	return j
}

func mustAppend(t *testing.T, s *Session, m kit.LLMMessage) string {
	t.Helper()
	id, err := s.AppendMessage(m)
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	return id
}

// TestRestoreDropsTornStep is the core of T-003. A provider rejects a
// conversation whose last assistant message has an unanswered tool call, so
// without this the run would never resume.
func TestRestoreDropsTornStep(t *testing.T) {
	t.Parallel()
	j := tornSession(t, "torn")

	s, err := Restore(context.Background(), "torn", j)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	msgs := s.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("restored %d messages, want only the user turn", len(msgs))
	}
	if messageText(msgs[0]) != "deploy the app" {
		t.Fatalf("kept the wrong message: %q", messageText(msgs[0]))
	}
	assertNoOrphan(t, msgs)

	// The session must keep working: an append after the repair extends the
	// repaired branch, it does not resurrect the dropped step.
	mustAppend(t, s, assistant("Which region?"))
	if got := len(s.GetMessages()); got != 2 {
		t.Fatalf("after append the branch has %d messages, want 2", got)
	}
}

// TestRepairIsJournalled keeps the repair auditable: a dropped step must leave
// a trace, or a run silently loses work with no explanation.
func TestRepairIsJournalled(t *testing.T) {
	t.Parallel()
	j := tornSession(t, "torn-audit")
	ctx := context.Background()

	if _, err := Restore(ctx, "torn-audit", j); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	recs, err := j.Replay(ctx, "torn-audit")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	var repairs int
	for _, rec := range recs {
		if rec.Kind == RecordRepair {
			repairs++
			if len(rec.Payload) == 0 {
				t.Fatal("repair record carries no dropped entry IDs")
			}
		}
	}
	if repairs != 1 {
		t.Fatalf("journalled %d repairs, want 1", repairs)
	}

	// A second Restore finds the same orphan — the journal is append-only —
	// but must not journal the repair again.
	if _, err := Restore(ctx, "torn-audit", j); err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	recs, _ = j.Replay(ctx, "torn-audit")
	repairs = 0
	for _, rec := range recs {
		if rec.Kind == RecordRepair {
			repairs++
		}
	}
	if repairs != 1 {
		t.Fatalf("journalled %d repairs after a second restore, want 1", repairs)
	}
}

// TestRepairLeavesWholeConversationAlone is the safety half of the claim: a
// journal with nothing wrong with it must come back byte-identical.
func TestRepairLeavesWholeConversationAlone(t *testing.T) {
	t.Parallel()
	msgs := []kit.LLMMessage{
		user("deploy the app"),
		toolCall("Deploying.", "c1", "deploy", `{"region":"eu"}`),
		toolResult("c1", "ok"),
		assistant("Deployed."),
	}
	got, err := repairTrailingOrphan(msgs)
	if err != nil {
		t.Fatalf("repairTrailingOrphan: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("repair dropped %d messages from a whole conversation", len(msgs)-len(got))
	}
	for i := range got {
		if messageText(got[i]) != messageText(msgs[i]) {
			t.Fatalf("message %d changed: %q -> %q", i, messageText(msgs[i]), messageText(got[i]))
		}
	}
}

// TestRepairDropsWholeMultiCallStep covers the partial-result case. Keeping the
// answered calls and dropping only the unanswered one would leave exactly the
// orphan the repair exists to remove.
func TestRepairDropsWholeMultiCallStep(t *testing.T) {
	t.Parallel()
	step := kit.LLMMessage{
		Role: kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{
			kit.LLMTextPart{Text: "Working."},
			kit.LLMToolCallPart{ToolCallID: "c1", ToolName: "read", Input: "{}"},
			kit.LLMToolCallPart{ToolCallID: "c2", ToolName: "write", Input: "{}"},
		},
	}
	msgs := []kit.LLMMessage{user("go"), step, toolResult("c1", "ok")}

	got, err := repairTrailingOrphan(msgs)
	if err != nil {
		t.Fatalf("repairTrailingOrphan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("kept %d messages, want only the user turn", len(got))
	}
	assertNoOrphan(t, got)
}

// TestRepairRefusesMidConversationMismatch draws the line between a torn write
// and a damaged journal. Rewriting the middle of a conversation would hide
// real corruption.
func TestRepairRefusesMidConversationMismatch(t *testing.T) {
	t.Parallel()
	msgs := []kit.LLMMessage{
		user("one"),
		toolCall("Calling.", "c1", "deploy", "{}"),
		user("two"), // no result ever arrived, yet the run carried on
		assistant("three"),
	}
	if _, err := repairTrailingOrphan(msgs); !errors.Is(err, ErrCorruptConversation) {
		t.Fatalf("err = %v, want ErrCorruptConversation", err)
	}
}

// TestRestoreRefusesCorruptJournal checks the same rule end to end, through
// Restore rather than the helper.
func TestRestoreRefusesCorruptJournal(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s := NewSession("corrupt", j)
	mustAppend(t, s, user("one"))
	mustAppend(t, s, toolCall("Calling.", "c1", "deploy", "{}"))
	mustAppend(t, s, user("two"))
	mustAppend(t, s, assistant("three"))

	_, err := Restore(context.Background(), "corrupt", j)
	if !errors.Is(err, ErrCorruptConversation) {
		t.Fatalf("err = %v, want ErrCorruptConversation", err)
	}
}

// TestRepairKeepsTrailingTextOnlyTurn guards against an over-eager repair: an
// assistant message with no tool calls is always complete.
func TestRepairKeepsTrailingTextOnlyTurn(t *testing.T) {
	t.Parallel()
	msgs := []kit.LLMMessage{user("hi"), assistant("hello")}
	got, err := repairTrailingOrphan(msgs)
	if err != nil {
		t.Fatalf("repairTrailingOrphan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("kept %d messages, want 2", len(got))
	}
}

// assertNoOrphan states the no-orphan invariant as an assertion: every
// tool call in the conversation has a result.
func assertNoOrphan(t *testing.T, msgs []kit.LLMMessage) {
	t.Helper()
	results := make(map[string]bool)
	for _, m := range msgs {
		for _, id := range toolResultIDs(m) {
			results[id] = true
		}
	}
	for i, m := range msgs {
		for _, id := range toolCallIDs(m) {
			if !results[id] {
				t.Fatalf("message %d calls %q with no result — a provider rejects this", i, id)
			}
		}
	}
}
