---
title: Decisions
description: Ask a decision model typed questions from host code or from the model, with each call journalled and keyed calls idempotent across restarts.
---

# Decisions

A decision model does not chat. It answers typed questions about a state with probabilities:

- A **boolean** question gives P(true).
- A **choice** question gives one option from a set, and a probability for each option.
- A **score** question gives a position on an ordered scale.

Decision models are cheap and fast. Use them to classify, route, score, and gate. BONNIE gives the primitive and decides no policy: your code decides what an answer means.

**A probability is not an authorization.** Enforce access rules in host code. Keep the sandbox and the network policy as the hard limits.

## Configure a model

```go
bonnie.New(
    bonnie.WithDecisionModel("openrouter/typesafe/jev-1.14"),
).Serve()
```

Find the models with `kit.GetDecisionModels()`, or run `kit update-models` to refresh the catalog. A local Ollama server takes any model it has, for example `ollama/pplx-decider`. `Agent.Run` refuses a model that Kit cannot call as a decision model. It does not check credentials or other environment settings; an error there shows on the first call. Kit reads the provider key from the environment or stored credentials, as for a chat model. A local Ollama model needs no key.

The option gives decisions to three places:

| Where | API | Journal |
| --- | --- | --- |
| Tool factories, Kit setup callbacks, completion hooks | `RunScope.Decide` | A `decision` record for each call |
| The model | the `decide` tool, from `WithDecisionTool()` | A `decision` record for each call |
| Host code with no run | `Agent.Decide` | None |

## Durable decisions

`RunScope.Decide` writes a `decision` record for each call that reaches the decision model or the `Decider`, also for a failed call. A call that does not reach it writes no record: a key answered from the journal, a key used again with different input, or input that cannot be encoded. The record holds the model, the questions, the answers, the token usage, and a hash of the input. It does not hold the state, which can be large or sensitive, or a provider's error body. The run console shows each record in the Journal and Trace tabs, and tracing gives a `decide` span.

Set `Key` to make a call idempotent in the run. When the run has a successful answer with the same key and the same input (model, state, questions, and provider options), `Decide` returns it from the journal and does not ask the model. This is also true in a new process after a crash, so a gate gives the same answer when its turn is recovered. Use a key that is unique in the run, for example the ID of the tool call the decision gates.

- A key used again with different input returns `runtime.ErrDecisionKeyReused`. A stored answer is never given for different input.
- A failed call is not stored as an answer. The next call with its key asks again.
- Calls with the same key wait for each other, so parallel calls make one model call.
- Without a key, each call asks the model.

If the record cannot be written, `Decide` returns an error and no result: an answer that is not durable must not drive an action. If the process stops after the model answers and before the record is written, a later call asks again. This is not exactly-once execution, and a repeated call can cost money.

## Gate risky tool calls

This setup callback asks before each shell command. It blocks a command that is probably destructive, and tells the model to get approval first. The key is the tool call ID, so a recovered turn gets the same answer.

```go
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
            // Fail closed: the gate could not decide.
            return &kit.BeforeToolCallResult{Block: true, Reason: "The safety check failed. Ask for approval with request_approval."}
        }
        if a, err := res.Boolean("destructive"); err != nil || a.Probability > 0.2 {
            return &kit.BeforeToolCallResult{Block: true, Reason: "This command can destroy data. Ask for approval with request_approval first."}
        }
        return nil
    })
    return nil
}),
```

## Check that a task is done

A completion hook can ask whether the response finishes the task. A low probability sends the model back to work. A completion check can repeat after a crash, and a run has many checks, so this call has no key.

```go
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
                return bonnie.CompletionFeedback{}, nil // accept; do not fail the run on a check
            }
            if a, err := res.Boolean("done"); err == nil && a.Probability < 0.5 {
                return bonnie.CompletionFeedback{ContinueWith: "The task is not complete. Continue."}, nil
            }
            return bonnie.CompletionFeedback{}, nil
        }, nil
    },
}),
```

## Skip a quiet schedule

`Agent.Decide` works without a run, so a schedule's `Prepare` can ask whether there is work before it starts an agent turn. An empty dispatch list sets the occurrence to `Skipped`. `Agent.Decide` writes no record and ignores `Key`.

```go
agent := bonnie.New(bonnie.WithDecisionModel("ollama/pplx-decider"))
agent.Configure(bonnie.WithSchedule(schedule.Definition{
    Name: "inbox", Cron: "*/15 * * * *", Revision: "1",
    Prepare: func(ctx context.Context, fire schedule.Fire) ([]schedule.Dispatch, error) {
        inbox, err := readInbox(ctx) // your code
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
agent.Serve()
```

## Give the model the decide tool

`WithDecisionTool()` adds the `decide` tool. The model gives a text of at most 32 KiB and 1 to 16 typed questions, and gets the answers as JSON. It is useful to classify or score many items cheaply. The chat model can usually make one judgment itself, so the tool is not added by default.

Each call is journalled with the key `tool:<tool call ID>`, and the tool is replay-safe. If a call is interrupted after its answer was journalled, recovery returns the stored answer and does not ask the model again.

## Test without a provider

`WithDecider` replaces the provider call. Calls in a run are still journalled:

```go
bonnie.WithDecider(func(_ context.Context, req kit.DecisionRequest) (*kit.DecisionResult, error) {
    return &kit.DecisionResult{Answers: []kit.DecisionAnswer{
        {ID: req.Questions[0].ID, Type: kit.DecisionBoolean, Probability: 0.9},
    }}, nil
}),
```

Use it also to reach a model that Kit cannot call. With `WithDecider`, `Agent.Run` does not check the model against the catalog.
