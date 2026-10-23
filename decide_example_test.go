package bonnie_test

import (
	"context"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/schedule"
)

// ExampleWithDecisionModel gates shell commands with a decision model and
// checks that each response completes the task. The recipes are those of
// www/pages/guides/decisions.md; this example keeps them compiling. It
// needs a provider, so it has no Output and does not run.
func ExampleWithDecisionModel() {
	bonnie.New(
		bonnie.WithDecisionModel("openrouter/typesafe/jev-1.14"),
		bonnie.WithKitSetup(func(ctx context.Context, k *kit.Kit, scope bonnie.RunScope) error {
			k.OnBeforeToolCall(kit.HookPriorityNormal, func(h kit.BeforeToolCallHook) *kit.BeforeToolCallResult {
				if h.ToolName != "shell" {
					return nil
				}
				res, err := scope.Decide(ctx, bonnie.DecisionRequest{
					State: kit.DecisionState{Text: h.ToolArgs},
					Questions: []kit.DecisionQuestion{
						kit.BooleanQuestion("destructive", "Does this command delete or overwrite data?"),
					},
					Key: "gate:" + h.ToolCallID,
				})
				if err != nil {
					return &kit.BeforeToolCallResult{Block: true, Reason: "The safety check failed. Ask for approval with request_approval."}
				}
				if a, err := res.Boolean("destructive"); err != nil || a.Probability > 0.2 {
					return &kit.BeforeToolCallResult{Block: true, Reason: "This command can destroy data. Ask for approval with request_approval first."}
				}
				return nil
			})
			return nil
		}),
		bonnie.WithCompletionHook(bonnie.CompletionPolicy{
			MaxContinuations: 2,
			NewHook: func(_ context.Context, scope bonnie.RunScope) (bonnie.CompletionHook, error) {
				return func(ctx context.Context, c bonnie.CompletionCandidate) (bonnie.CompletionFeedback, error) {
					res, err := scope.Decide(ctx, bonnie.DecisionRequest{
						State: kit.DecisionState{Text: c.Response},
						Questions: []kit.DecisionQuestion{
							kit.BooleanQuestion("done", "Does this response complete the task, with no step left for later?"),
						},
					})
					if err != nil {
						return bonnie.CompletionFeedback{}, nil
					}
					if a, err := res.Boolean("done"); err == nil && a.Probability < 0.5 {
						return bonnie.CompletionFeedback{ContinueWith: "The task is not complete. Continue."}, nil
					}
					return bonnie.CompletionFeedback{}, nil
				}, nil
			},
		}),
		bonnie.WithDecisionTool(),
	)
}

// ExampleAgent_Decide skips a quiet schedule before it starts a turn.
func ExampleAgent_Decide() {
	readInbox := func(context.Context) (string, error) { return "", nil }
	agent := bonnie.New(bonnie.WithDecisionModel("ollama/pplx-decider"))
	agent.Configure(bonnie.WithSchedule(schedule.Definition{
		Name: "inbox", Cron: "*/15 * * * *", Revision: "1",
		Prepare: func(ctx context.Context, fire schedule.Fire) ([]schedule.Dispatch, error) {
			inbox, err := readInbox(ctx)
			if err != nil {
				return nil, err
			}
			res, err := agent.Decide(ctx, bonnie.DecisionRequest{
				State:     kit.DecisionState{Text: inbox},
				Questions: []kit.DecisionQuestion{kit.BooleanQuestion("work", "Does a message need a reply?")},
			})
			if err != nil {
				return nil, err
			}
			if a, err := res.Boolean("work"); err == nil && a.Probability < 0.3 {
				return nil, nil
			}
			return []schedule.Dispatch{{Key: "reply", Input: runtime.Input{Text: "Reply to the inbox."}}}, nil
		},
	}))
}
