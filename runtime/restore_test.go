package runtime

import (
	"context"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// toolCall builds an assistant message that both speaks and calls a tool — the
// shape that a human-in-the-loop suspension always produces.
func toolCall(text, callID, tool, input string) kit.LLMMessage {
	return kit.LLMMessage{
		Role: kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{
			kit.LLMTextPart{Text: text},
			kit.LLMToolCallPart{ToolCallID: callID, ToolName: tool, Input: input},
		},
	}
}

func toolResult(callID, output string) kit.LLMMessage {
	return kit.LLMMessage{
		Role: kit.LLMMessageRole("tool"),
		Content: []kit.LLMMessagePart{kit.LLMToolResultPart{
			ToolCallID: callID,
			Output:     kit.LLMToolResultOutputContentText{Text: output},
		}},
	}
}

// TestRestorePreservesToolCalls is the regression test for a replay that kept
// only the text of each message. A resumed run must still know which tools it
// called, or it re-runs their side effects.
func TestRestorePreservesToolCalls(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()
	ctx := context.Background()

	orig := NewSession("tools", journal)
	if _, err := orig.AppendMessage(user("deploy the app")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := orig.AppendMessage(toolCall("Checking.", "c1", "deploy", `{"region":"eu"}`)); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := orig.AppendMessage(toolResult("c1", "ok")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	restored, err := Restore(ctx, "tools", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	msgs := restored.GetMessages()
	if len(msgs) != 3 {
		t.Fatalf("restored %d messages, want 3", len(msgs))
	}

	if got := len(msgs[1].Content); got != 2 {
		t.Fatalf("assistant message restored with %d parts, want text + tool call", got)
	}
	call, ok := msgs[1].Content[1].(kit.LLMToolCallPart)
	if !ok {
		t.Fatalf("part 1 is %T, want a tool call", msgs[1].Content[1])
	}
	if call.ToolCallID != "c1" || call.ToolName != "deploy" {
		t.Fatalf("tool call = %+v", call)
	}
	if call.Input != `{"region":"eu"}` {
		t.Fatalf("tool call input = %q", call.Input)
	}

	res, ok := msgs[2].Content[0].(kit.LLMToolResultPart)
	if !ok {
		t.Fatalf("part is %T, want a tool result", msgs[2].Content[0])
	}
	if res.ToolCallID != "c1" {
		t.Fatalf("tool result call ID = %q", res.ToolCallID)
	}
	out, ok := toolResultText(res.Output)
	if !ok || out != "ok" {
		t.Fatalf("tool result output = %+v", res.Output)
	}
}

// TestRestorePreservesReasoning covers the other non-text part a real turn
// carries.
func TestRestorePreservesReasoning(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()

	orig := NewSession("reason", journal)
	msg := kit.LLMMessage{
		Role: kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{
			kit.LLMReasoningPart{Text: "weighing the options"},
			kit.LLMTextPart{Text: "Done."},
		},
	}
	if _, err := orig.AppendMessage(msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	restored, err := Restore(context.Background(), "reason", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	parts := restored.GetMessages()[0].Content
	if len(parts) != 2 {
		t.Fatalf("restored %d parts, want 2", len(parts))
	}
	if r, ok := parts[0].(kit.LLMReasoningPart); !ok || r.Text != "weighing the options" {
		t.Fatalf("part 0 = %#v, want the reasoning", parts[0])
	}
}

// TestRestorePreservesExtensionData guards BONNIE's durable per-run state.
// Extension entries must come back as extension entries, not as conversation.
func TestRestorePreservesExtensionData(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()

	orig := NewSession("ext", journal)
	if _, err := orig.AppendMessage(user("hello")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := orig.AppendExtensionData("counter", `{"n":1}`); err != nil {
		t.Fatalf("AppendExtensionData: %v", err)
	}

	restored, err := Restore(context.Background(), "ext", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got := restored.GetExtensionData("counter")
	if len(got) != 1 {
		t.Fatalf("restored %d extension entries, want 1", len(got))
	}
	if got[0].ExtType != "counter" || got[0].Data != `{"n":1}` {
		t.Fatalf("extension entry = %+v", got[0])
	}
	// Run state is not conversation: it must not reappear as a message.
	if msgs := restored.GetMessages(); len(msgs) != 1 {
		t.Fatalf("restored %d messages, want only the user turn", len(msgs))
	}
}

// TestRestorePreservesCompaction checks that a compacted context stays
// compacted after a resume, instead of silently re-expanding to full history.
func TestRestorePreservesCompaction(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()

	orig := NewSession("compact", journal)
	_, _ = orig.AppendMessage(user("old one"))
	_, _ = orig.AppendMessage(assistant("old two"))
	keep, _ := orig.AppendMessage(user("keep me"))
	if _, err := orig.AppendCompaction("SUMMARY", keep, 100, 10, 2, []string{"a.go"}, nil); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}

	restored, err := Restore(context.Background(), "compact", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	c := restored.GetLastCompaction()
	if c == nil {
		t.Fatal("compaction lost on restore")
	}
	if c.Summary != "SUMMARY" || c.FirstKeptEntryID != keep {
		t.Fatalf("compaction = %+v", c)
	}
	if c.TokensBefore != 100 || c.TokensAfter != 10 || c.MessagesRemoved != 2 {
		t.Fatalf("compaction metadata = %+v", c)
	}
	if len(c.ReadFiles) != 1 || c.ReadFiles[0] != "a.go" {
		t.Fatalf("compaction read files = %v", c.ReadFiles)
	}

	msgs, _, _ := restored.BuildContext()
	if len(msgs) != 2 {
		t.Fatalf("restored context has %d messages, want summary + kept", len(msgs))
	}
	if messageText(msgs[0]) != "SUMMARY" || messageText(msgs[1]) != "keep me" {
		t.Fatalf("restored context = %q, %q", messageText(msgs[0]), messageText(msgs[1]))
	}
}

// TestRestorePreservesModelChange guards the tree against a hole. A model
// change joins the conversation tree and becomes the parent of the next
// message, so a version that kept it only in memory left every later message
// pointing at an entry the journal had never heard of, and replay silently
// dropped the whole conversation before the switch.
func TestRestorePreservesModelChange(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()

	orig := NewSession("model", journal)
	_, _ = orig.AppendMessage(user("one"))
	if _, err := orig.AppendModelChange("anthropic", "claude"); err != nil {
		t.Fatalf("AppendModelChange: %v", err)
	}
	_, _ = orig.AppendMessage(assistant("two"))

	restored, err := Restore(context.Background(), "model", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got := len(restored.GetCurrentBranch()); got != 3 {
		t.Fatalf("restored %d entries, want 3", got)
	}
	msgs := restored.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("restored %d messages, want 2 — the branch was severed", len(msgs))
	}
	if messageText(msgs[0]) != "one" || messageText(msgs[1]) != "two" {
		t.Fatalf("restored conversation = %+v", msgs)
	}

	// A resumed run must report the model it switched to, not an empty one.
	if _, provider, model := restored.BuildContext(); provider != "anthropic" || model != "claude" {
		t.Fatalf("restored model = %q/%q, want anthropic/claude", provider, model)
	}
}

// TestRestoreContinuesEntryIDs makes sure a resumed session does not reissue an
// ID that a replayed entry already owns, which would silently overwrite an
// entry instead of extending the branch.
func TestRestoreContinuesEntryIDs(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()

	orig := NewSession("ids", journal)
	_, _ = orig.AppendMessage(user("one"))
	_, _ = orig.AppendModelChange("anthropic", "claude")
	_, _ = orig.AppendExtensionData("state", `{}`)
	last, _ := orig.AppendMessage(assistant("two"))

	restored, err := Restore(context.Background(), "ids", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	branch := restored.GetCurrentBranch()
	if len(branch) != 4 {
		t.Fatalf("restored %d entries, want 4", len(branch))
	}

	id, err := restored.AppendMessage(user("three"))
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if id == last {
		t.Fatalf("new entry ID %q collides with replayed entry %q", id, last)
	}
	for _, e := range branch {
		if e.ID == id {
			t.Fatalf("new entry ID %q collides with replayed entry %q", id, e.ID)
		}
	}
	if got := len(restored.GetCurrentBranch()); got != 5 {
		t.Fatalf("branch has %d entries after append, want 5", got)
	}
	if msgs := restored.GetMessages(); len(msgs) != 3 {
		t.Fatalf("restored %d messages after append, want 3", len(msgs))
	}
}

func TestEntrySeq(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id   string
		want int
	}{
		{"m-12", 12},
		{"x-1", 1},
		{"", 0},
		{"m", 0},
		{"m-", 0},
		{"m-abc", 0},
		{"m--3", 0},
	}
	for _, c := range cases {
		if got := entrySeq(c.id); got != c.want {
			t.Errorf("entrySeq(%q) = %d, want %d", c.id, got, c.want)
		}
	}
}

// TestDecodeMessageFallsBackToText documents the compatibility path for journal
// records written before messages carried a payload.
func TestDecodeMessageFallsBackToText(t *testing.T) {
	t.Parallel()
	msg, err := decodeMessage(Record{EntryID: "m-1", Role: "user", Text: "legacy"})
	if err != nil {
		t.Fatalf("decodeMessage: %v", err)
	}
	if len(msg.Content) != 1 || messageText(msg) != "legacy" {
		t.Fatalf("fallback message = %+v", msg)
	}
}

func TestDecodeMessageRejectsCorruptPayload(t *testing.T) {
	t.Parallel()
	_, err := decodeMessage(Record{EntryID: "m-1", Payload: []byte("{not json")})
	if err == nil {
		t.Fatal("want an error for a corrupt payload")
	}
}
