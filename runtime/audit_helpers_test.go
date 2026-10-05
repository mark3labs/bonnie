package runtime

import kit "github.com/mark3labs/kit/pkg/kit"

// toolResultText extracts the text of a tool-result output part.
//
// It exists so BONNIE never names charm.land/fantasy directly. Kit re-exports
// the fantasy types as aliases, but not fantasy's generic accessor, so this is
// the public-API-only way to read a tool result. The pointer case mirrors
// fantasy's own accessor, which accepts either form.
func toolResultText(out kit.LLMToolResultOutputContent) (string, bool) {
	switch v := out.(type) {
	case kit.LLMToolResultOutputContentText:
		return v.Text, true
	case *kit.LLMToolResultOutputContentText:
		return v.Text, true
	}
	return "", false
}

// repairTrailingOrphan removes an incomplete trailing tool-calling step.
//
// It is the pure form of the repair, over a plain message slice. The
// production path is [Session.repairTail], which does the same thing to a
// restored conversation tree and journals a [RecordRepair]; this function
// states the rule on its own, and the repair tests exercise it directly.
//
// Why the shape exists: a tool-calling step is an assistant message that
// carries the call and a tool message that carries the result. Kit hands
// both to [Session.AppendStep] in one call as of v0.106.0, and a
// [StepJournal] commits them together — so current BONNIE cannot produce
// the orphan. It is still on disk in journals BONNIE inherited: those
// written before v0.106.0, when a step was two independent AppendMessage
// calls, those imported from the original JSONL format, and those from any
// [Journal] that does not implement [StepJournal] and still takes the
// per-record fallback.
//
// An assistant tool_use with no tool_result is a conversation every provider
// rejects, so leaving it would make the run permanently unresumable.
// Dropping the step is the correct semantics: the step never finished, so
// the model is free to run it again.
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
