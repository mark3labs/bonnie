package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	kit "github.com/mark3labs/kit/pkg/kit"
)

const (
	// RecordToolIntent records a tool call before its external action starts.
	RecordToolIntent RecordKind = "tool_intent"
	// RecordToolOutcome records the result before the completed step is stored.
	RecordToolOutcome RecordKind = "tool_outcome"
)

type toolIntent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
	Safe bool   `json:"safe,omitempty"`
}

type toolOutcome struct {
	ID      string `json:"id"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error,omitempty"`
	Unknown bool   `json:"unknown,omitempty"`
}

// SetReplaySafeTools declares tools that can repeat after an interrupted call.
// Call it from KitSetup before the factory returns. A safe tool must make its
// own external actions idempotent. The default is no safe tools. The decision
// is stored with each intent; a later configuration change cannot widen it.
func (s *Session) SetReplaySafeTools(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaySafe = make(map[string]bool, len(names))
	for _, name := range names {
		s.replaySafe[name] = true
	}
}

func appendToolRecord(ctx context.Context, s *Session, kind RecordKind, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("bonnie: encode tool record: %w", err)
	}
	_, err = s.journal.Append(ctx, Record{RunID: s.runID, Kind: kind, Timestamp: now(), Payload: payload})
	return err
}

// recoverTools puts calls absent from the transcript back into a complete
// assistant/tool pair. Unsafe calls are never executed here. The model is told
// that their external effect is unknown, not that they failed without effects.
// Text outcomes are stored by Kit's public result hook. Rich media remain in
// the normal lossless step record; an interrupted pre-step recovery has only
// the hook's text projection available.
func recoverTools(ctx context.Context, k *kit.Kit, s *Session) error {
	recs, err := s.journal.Replay(ctx, s.runID)
	if errors.Is(err, ErrRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var intents []toolIntent
	outcomes := map[string]toolOutcome{}
	for _, rec := range recs {
		switch rec.Kind {
		case RecordToolIntent:
			var in toolIntent
			if err := json.Unmarshal(rec.Payload, &in); err != nil {
				return fmt.Errorf("bonnie: decode tool intent: %w", err)
			}
			intents = append(intents, in)
		case RecordToolOutcome:
			var out toolOutcome
			if err := json.Unmarshal(rec.Payload, &out); err != nil {
				return fmt.Errorf("bonnie: decode tool outcome: %w", err)
			}
			outcomes[out.ID] = out
		}
	}
	seen := map[string]bool{}
	// Scan all committed message records, not only the selected branch. A
	// deliberate clear or branch change must not resurrect old tool calls.
	for _, rec := range recs {
		if rec.Kind != RecordMessage {
			continue
		}
		msg, err := decodeMessage(rec)
		if err != nil {
			return err
		}
		for _, part := range msg.Content {
			if p, ok := part.(kit.LLMToolResultPart); ok {
				seen[p.ToolCallID] = true
			}
		}
	}
	for _, in := range intents {
		if seen[in.ID] {
			if out := outcomes[in.ID]; out.Unknown {
				s.mu.Lock()
				if s.interruptedTools == nil {
					s.interruptedTools = map[string]bool{}
				}
				s.interruptedTools[toolActionKey(in.Name, in.Args)] = true
				s.mu.Unlock()
			}
			continue
		}
		out, done := outcomes[in.ID]
		if !done {
			out = toolOutcome{ID: in.ID, IsError: true, Unknown: true, Result: "Interrupted tool call. Its external effect is unknown. Do not repeat the action without checking its effect or obtaining human approval."}
			s.mu.RLock()
			safeNow := s.replaySafe[in.Name]
			s.mu.RUnlock()
			// Both the original declaration and the current host must permit
			// recovery. Direct replay uses the tool implementation; tools that
			// need approval hooks must not be declared replay-safe.
			if in.Safe && safeNow {
				for _, tool := range k.GetTools() {
					if tool.Info().Name != in.Name {
						continue
					}
					result, runErr := tool.Run(ctx, kit.LLMToolCall{ID: in.ID, Name: in.Name, Input: in.Args})
					out.Result, out.IsError, out.Unknown = result.Content, result.IsError, false
					if runErr != nil {
						out.Result, out.IsError = runErr.Error(), true
					}
					break
				}
			}
			if err := appendToolRecord(context.WithoutCancel(ctx), s, RecordToolOutcome, out); err != nil {
				return err
			}
		}
		if out.Unknown {
			s.mu.Lock()
			if s.interruptedTools == nil {
				s.interruptedTools = map[string]bool{}
			}
			s.interruptedTools[toolActionKey(in.Name, in.Args)] = true
			s.mu.Unlock()
		}
		resultPart := kit.LLMToolResultPart{ToolCallID: in.ID, Output: kit.LLMToolResultOutputContentText{Text: out.Result}}
		if out.IsError {
			resultPart.Output = kit.LLMToolResultOutputContentError{Error: errors.New(out.Result)}
		}
		_, err := s.AppendStep(ctx, []kit.LLMMessage{
			{Role: kit.LLMMessageRole("assistant"), Content: []kit.LLMMessagePart{kit.LLMToolCallPart{ToolCallID: in.ID, ToolName: in.Name, Input: in.Args}}},
			{Role: kit.LLMMessageRole("tool"), Content: []kit.LLMMessagePart{resultPart}},
		})
		if err != nil {
			return err
		}
		seen[in.ID] = true
	}
	return nil
}

func attachToolRecovery(k *kit.Kit, s *Session) {
	k.OnBeforeToolCall(kit.HookPriorityHigh, func(h kit.BeforeToolCallHook) *kit.BeforeToolCallResult {
		s.mu.RLock()
		safe := s.replaySafe[h.ToolName]
		interrupted := s.interruptedTools[toolActionKey(h.ToolName, h.ToolArgs)]
		s.mu.RUnlock()
		if interrupted {
			return &kit.BeforeToolCallResult{Block: true, Reason: "Previous execution has an unknown external effect. A human must verify and resolve it before this action can repeat."}
		}
		in := toolIntent{ID: h.ToolCallID, Name: h.ToolName, Args: h.ToolArgs, Safe: safe}
		if err := appendToolRecord(context.Background(), s, RecordToolIntent, in); err != nil {
			return &kit.BeforeToolCallResult{Block: true, Reason: fmt.Sprintf("Cannot store tool intent: %v", err)}
		}
		return nil
	})
	k.OnAfterToolResult(kit.HookPriorityLow, func(h kit.AfterToolResultHook) *kit.AfterToolResultResult {
		out := toolOutcome{ID: h.ToolCallID, Result: h.Result, IsError: h.IsError}
		if err := appendToolRecord(context.Background(), s, RecordToolOutcome, out); err != nil {
			text, failed := fmt.Sprintf("Cannot store tool outcome; external effect may have occurred: %v", err), true
			return &kit.AfterToolResultResult{Result: &text, IsError: &failed}
		}
		return nil
	})
}

func toolActionKey(name, args string) string {
	var value any
	if json.Unmarshal([]byte(args), &value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			args = string(canonical)
		}
	}
	return name + "\x00" + args
}

// ResolveInterruptedTool records a human's verified result for an uncertain
// action and permits later calls with the same tool and arguments. It does
// not execute the action. Call it from a host-controlled KitSetup, never from
// an agent tool that can approve its own action.
func (s *Session) ResolveInterruptedTool(ctx context.Context, id, result string) error {
	recs, err := s.journal.Replay(ctx, s.runID)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Kind != RecordToolIntent {
			continue
		}
		var in toolIntent
		if err := json.Unmarshal(rec.Payload, &in); err != nil {
			return err
		}
		if in.ID == id {
			return appendToolRecord(ctx, s, RecordToolOutcome, toolOutcome{ID: id, Result: result})
		}
	}
	return errors.New("bonnie: interrupted tool call not found")
}
