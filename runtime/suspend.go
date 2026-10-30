package runtime

import (
	"context"
	"encoding/json"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/wire"
)

// SuspendRequest is what a tool returns to park a run until a human or an
// external system answers. It rides out of the agent loop on
// [kit.TurnResult.FinalValue] via a tool that sets ToolOutput.Halt.
type SuspendRequest = wire.SuspendRequest

// InputResponse is a reply to a [SuspendRequest].
type InputResponse = wire.InputResponse

// Approve and Reject build an approval answer. They exist so a caller does
// not have to take the address of a bool literal to answer a question the
// agent asked.
func Approve(note string) InputResponse {
	return wire.Approve(note)
}

// Reject is [Approve]'s refusal. The note, when given, tells the agent why,
// which is usually what decides what it does next.
func Reject(note string) InputResponse {
	return wire.Reject(note)
}

// Suspension kinds recognised by the runtime.
const (
	SuspendQuestion = wire.SuspendQuestion
	SuspendApproval = wire.SuspendApproval
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

// stepPayload is the durable form of a [RecordStep] payload. Kind and Prompt
// keep the [SuspendRequest] shape that the step records had before the
// model was added, so a reader that decodes a SuspendRequest still works.
type stepPayload struct {
	Kind   string `json:"kind"`
	Prompt string `json:"prompt"`
	// Model is the "provider/model" that served the step, as Kit reports it
	// in [kit.StepFinishEvent].
	Model string `json:"model,omitempty"`
}

func encodeStep(p stepPayload) json.RawMessage {
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return b
}

// StepModel returns the "provider/model" that served the agent step rec
// records. It returns "" when rec is not a [RecordStep], and for a step
// record written before BONNIE recorded the model.
func StepModel(rec Record) string {
	if rec.Kind != RecordStep || len(rec.Payload) == 0 {
		return ""
	}
	var p stepPayload
	if json.Unmarshal(rec.Payload, &p) != nil {
		return ""
	}
	return p.Model
}
