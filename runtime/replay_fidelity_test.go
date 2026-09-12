package runtime

import (
	"context"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// TestReplayPreservesToolCalls guards replay fidelity. See docs/SPEC.md §4.1.
//
// Restore decodes messages from Record.Payload, so tool calls, tool results,
// files, and reasoning parts survive a resume in a new process. An earlier
// version stored only flattened text and silently dropped everything except
// text parts, which left a resumed run with no memory of what it had called.
//
// The step is journalled whole — call and result — because an unanswered call
// at the tail is a torn write, and Restore drops it by design (§4.2).
//
// Keep this test passing. A regression here is invisible at the type level and
// only shows up as an agent that repeats work it already did.
func TestReplayPreservesToolCalls(t *testing.T) {
	t.Parallel()

	journal := NewMemoryJournal()
	s := NewSession("replay-fidelity", journal)

	assistant := kit.LLMMessage{
		Role: kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{
			kit.LLMTextPart{Text: "Let me ask."},
			kit.LLMToolCallPart{
				ToolCallID: "tc-1",
				ToolName:   "ask_human",
				Input:      `{"question":"Which region?"}`,
			},
		},
	}
	if _, err := s.AppendMessage(assistant); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	result := kit.LLMMessage{
		Role: kit.LLMMessageRole("tool"),
		Content: []kit.LLMMessagePart{kit.LLMToolResultPart{
			ToolCallID: "tc-1",
			Output:     kit.LLMToolResultOutputContentText{Text: "Awaiting operator response."},
		}},
	}
	if _, err := s.AppendMessage(result); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	restored, err := Restore(context.Background(), "replay-fidelity", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var toolCalls, toolResults int
	for _, m := range restored.GetMessages() {
		for _, part := range m.Content {
			switch part.(type) {
			case kit.LLMToolCallPart:
				toolCalls++
			case kit.LLMToolResultPart:
				toolResults++
			}
		}
	}
	if toolCalls != 1 {
		t.Fatalf("restored %d tool calls, want 1 — replay is lossy", toolCalls)
	}
	if toolResults != 1 {
		t.Fatalf("restored %d tool results, want 1 — replay is lossy", toolResults)
	}
}
