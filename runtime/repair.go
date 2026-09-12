package runtime

import (
	"errors"
	"fmt"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// ErrCorruptConversation is returned when a replayed conversation has an
// unanswered tool call that is not at its tail. A tail orphan is a torn write
// and BONNIE repairs it; a mismatch in the middle means the journal itself is
// damaged, and rewriting history silently would hide the damage.
var ErrCorruptConversation = errors.New("bonnie: replayed conversation is corrupt")

// repairTrailingOrphan removes an incomplete trailing tool-calling step.
//
// Kit appends the assistant message that carries a tool call and the tool
// message that carries its result as two separate AppendMessage calls (see
// docs/SPEC.md §3.3). A crash between them leaves the journal with an
// assistant message whose tool_use has no tool_result. Every provider rejects
// such a conversation, so the run would become permanently unresumable.
//
// Dropping the step is the correct semantics: the step never finished, so the
// model is free to run it again.
//
// It returns the messages unchanged when the conversation is whole, and
// [ErrCorruptConversation] when the unanswered call is not at the tail.
func repairTrailingOrphan(msgs []kit.LLMMessage) ([]kit.LLMMessage, error) {
	cut, err := orphanCut(msgs)
	if err != nil {
		return nil, err
	}
	if cut < 0 {
		return msgs, nil
	}
	return msgs[:cut], nil
}

// orphanCut returns the index of the first message of an incomplete trailing
// tool-calling step, or -1 when every tool call has a result.
func orphanCut(msgs []kit.LLMMessage) (int, error) {
	results := make(map[string]struct{})
	for _, m := range msgs {
		for _, id := range toolResultIDs(m) {
			results[id] = struct{}{}
		}
	}

	for i, m := range msgs {
		calls := toolCallIDs(m)
		if len(calls) == 0 {
			continue
		}
		missing := ""
		for _, id := range calls {
			if _, ok := results[id]; !ok {
				missing = id
				break
			}
		}
		if missing == "" {
			continue
		}
		// A partially answered step is dropped whole. Keeping the answered
		// calls and dropping the rest would leave the same orphan.
		if isTailStep(msgs, i) {
			return i, nil
		}
		return 0, fmt.Errorf("%w: message %d calls %q with no result", ErrCorruptConversation, i, missing)
	}
	return -1, nil
}

// isTailStep reports whether everything after index i belongs to that step,
// which is true when no later message carries new content — only tool results.
func isTailStep(msgs []kit.LLMMessage, i int) bool {
	for _, m := range msgs[i+1:] {
		if m.Role != kit.LLMMessageRole("tool") {
			return false
		}
	}
	return true
}

// toolCallIDs returns the IDs of every tool call a message carries.
func toolCallIDs(m kit.LLMMessage) []string {
	var out []string
	for _, p := range m.Content {
		if c, ok := p.(kit.LLMToolCallPart); ok {
			out = append(out, c.ToolCallID)
		}
	}
	return out
}

// toolResultIDs returns the call IDs of every tool result a message carries.
func toolResultIDs(m kit.LLMMessage) []string {
	var out []string
	for _, p := range m.Content {
		if r, ok := p.(kit.LLMToolResultPart); ok {
			out = append(out, r.ToolCallID)
		}
	}
	return out
}
