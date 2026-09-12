package runtime

import (
	"context"
	"encoding/json"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// SuspendRequest is what a tool returns to park a run until a human or an
// external system answers. It rides out of the agent loop on
// [kit.TurnResult.FinalValue] via a tool that sets ToolOutput.Halt.
type SuspendRequest struct {
	// Kind distinguishes question, approval, or a host-defined reason.
	Kind string `json:"kind"`
	// Prompt is the question or approval text shown to the responder.
	Prompt string `json:"prompt"`
	// Options, when non-empty, constrains the answer to a choice.
	Options []string `json:"options,omitempty"`
	// ToolCallID identifies the halting tool call.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// InputResponse is a reply to a [SuspendRequest].
type InputResponse struct {
	// Text is the freeform or selected answer.
	Text string `json:"text"`
	// Approved answers approval-kind suspensions.
	Approved bool `json:"approved,omitempty"`
}

// Suspension kinds recognised by the runtime.
const (
	SuspendQuestion = "question"
	SuspendApproval = "approval"
)

// AskTool returns a tool the model can call to ask the operator a question and
// park the run. The run resumes through [Runner.Resume].
//
// This is BONNIE's human-in-the-loop primitive. It needs no changes to Kit:
// Halt plus FinalValue is already a complete suspension contract.
func AskTool() kit.Tool {
	type input struct {
		Question string   `json:"question" description:"The question to ask the operator."`
		Options  []string `json:"options,omitempty" description:"Optional list of allowed answers."`
	}
	return kit.NewTool("ask_human",
		"Ask the operator a question and pause until they answer.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return kit.ToolOutput{
				Content: "Awaiting operator response.",
				Halt:    true,
				FinalValue: SuspendRequest{
					Kind:       SuspendQuestion,
					Prompt:     in.Question,
					Options:    in.Options,
					ToolCallID: kit.ToolCallIDFromContext(ctx),
				},
			}, nil
		})
}

// ApprovalTool returns a tool that pauses for an explicit approve or reject
// before a side effect proceeds.
func ApprovalTool() kit.Tool {
	type input struct {
		Action string `json:"action" description:"The action that needs approval."`
	}
	return kit.NewTool("request_approval",
		"Request operator approval before performing a sensitive action.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return kit.ToolOutput{
				Content: "Awaiting operator approval.",
				Halt:    true,
				FinalValue: SuspendRequest{
					Kind:       SuspendApproval,
					Prompt:     in.Action,
					Options:    []string{"approve", "reject"},
					ToolCallID: kit.ToolCallIDFromContext(ctx),
				},
			}, nil
		})
}

// suspensionFrom extracts a [SuspendRequest] from a finished turn. It reports
// false when the turn ended for any other reason.
func suspensionFrom(res *kit.TurnResult) (SuspendRequest, bool) {
	if res == nil || res.FinalValue == nil {
		return SuspendRequest{}, false
	}
	switch v := res.FinalValue.(type) {
	case SuspendRequest:
		return v, true
	case *SuspendRequest:
		if v != nil {
			return *v, true
		}
	}
	return SuspendRequest{}, false
}

func encodeSuspend(s SuspendRequest) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return b
}
