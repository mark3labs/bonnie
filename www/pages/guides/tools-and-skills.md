---
title: Tools and Skills
description: Add compiled Kit tools and on-demand skills to BONNIE agents with clear execution and approval boundaries.
---

# Tools and Skills

A tool performs an operation. A skill gives the model task-specific instructions when it activates that skill. Neither is a substitute for authorization checks.

BONNIE uses the public Kit SDK at `github.com/mark3labs/kit/pkg/kit`. An agent tree compiles tools into its binary and embeds its authored skill files.

## Know where code runs

The standard `shell`, `read_file`, `write_file`, and `list_files` tools use the selected sandbox. BONNIE disables Kit's host core tools on this path. The human-input tools run in the BONNIE process.

**Your Go tool also runs in the BONNIE process.** Registering it with `WithTools` or placing it under `tools/` does not put its callback in a container. A direct `os.ReadFile`, database request, HTTP request, or `exec.Command` in that callback uses host permissions and credentials.

Use narrow host tools for trusted operations. Validate every argument. Use sandbox execution for model-selected commands. Do not expose an arbitrary host shell through a custom tool.

## Add a tree tool

Create `tools/echo/tool.go`:

```go
package echo

import (
    "context"
    "strings"

    kit "github.com/mark3labs/kit/pkg/kit"
)

type echoInput struct {
    Text string `json:"text" description:"Short text to return."`
}

// Tool returns a bounded echo tool.
func Tool() kit.Tool {
    return kit.NewTool("echo", "Return short text without a side effect.",
        func(ctx context.Context, in echoInput) (kit.ToolOutput, error) {
            if err := ctx.Err(); err != nil {
                return kit.ToolOutput{}, err
            }
            text := strings.TrimSpace(in.Text)
            if text == "" || len(text) > 1024 {
                return kit.ErrorResult("text must contain 1 to 1024 bytes"), nil
            }
            return kit.TextResult(text), nil
        })
}
```

The typed input supplies the tool schema. Use snake_case JSON field names and clear descriptions. Schema descriptions help the model; the callback must still enforce limits.

Run discovery and tests:

```bash
bonnie dev --dry-run
go test ./...
bonnie build
```

Each immediate tool directory must export `func Tool() kit.Tool` in non-test Go code. The generator imports these packages into `bonnie_gen.go`. Do not add the same tree tool to `main.go` again. Discovery rejects missing factories and detectable duplicate literal names; it is not a complete analysis of names produced by helper functions. Keep names unique yourself.

For a host that constructs tools directly, use `bonnie.WithTools(myTool)`. For a low-level sandbox agent, use `kit.WithExtraTools(myTool)` with `sandbox.Agent`. Kit's `Tools` option replaces a tool set; `ExtraTools` adds to it. Do not replace the set or re-enable host core tools without examining the security effect.

## Make effects safe to repeat

A saved tool result is part of lossless conversation replay. BONNIE does not need to re-execute that saved call to rebuild history. This is not exactly-once execution of an external service.

A process can stop after a payment, post, or deployment succeeds but before the result commits to the journal. A user can also request a retry as a new turn. Protect important effects at the service boundary:

1. Verify the caller's authority in trusted code, not from model text.
2. Check resource IDs, amounts, paths, and allowed operations.
3. Use stable application idempotency keys and save service receipts.
4. Use deadlines and observe context cancellation.
5. Return a bounded result without keys, tokens, or private raw responses.

Test invalid input, service errors, cancellation, and duplicate requests. Do not give model input directly to a shell string or SQL statement. Use structured arguments and parameterized queries.

## Ask a human

`ask_human` and `request_approval` are available by default. A halting tool returns a `runtime.SuspendRequest` as its final value. This return fragment belongs inside a tool callback:

```go
// Import "github.com/mark3labs/bonnie/runtime" and the public Kit SDK.
return kit.ToolOutput{
    Content: "Waiting for the operator.",
    Halt:    true,
    FinalValue: runtime.SuspendRequest{
        Kind:   "approval",
        Prompt: "Approve publication of the report?",
    },
}, nil
```

The turn stops with state `waiting`. A later answer resumes it from the journal. A waiting run holds no sandbox compute.

**A halt stops the turn after the current step, not before sibling tools.** Kit runs the tool calls from one step together. An action called in the same step as `request_approval` can still run. A prompt that says “ask first” is not an enforced gate. For dangerous operations, the action tool must verify a trusted authorization record before it acts. Do not treat the model's claim that approval exists as proof.

`bonnie.WithoutHumanInput()` omits the two built-in human-input tools. It does not grant approval, change sandbox permissions, remove your tools, or stop a custom tool from suspending a run. It cannot be combined with `WithAgentFactory`.

## Add a skill

Create `skills/reporting/SKILL.md`:

```markdown
---
name: reporting
description: Prepare an operations report from CSV working files.
---

# Prepare a report

1. Read operations.csv from the working directory.
2. Check required columns and report missing values.
3. Calculate totals. Do not invent records.
4. Write report.md with a summary and the data problems.
5. Do not publish the report to an external service.
```

The supported layouts include one `.md` or `.txt` file per skill, or one directory per skill with `SKILL.md`. Each skill needs YAML frontmatter with `name` and `description`. Give descriptions enough detail for the model to select the correct skill.

The name and description enter the system prompt. The body arrives when the model calls `activate_skill`. A skill does not run its instructions automatically and does not register a new Go tool.

The tree's `skills/` is the authored set. BONNIE does not inherit skills from a host directory beside the server. The managed agent-tree path selects the authored skill directory explicitly; it does not automatically discover model-written skills in run working files. A custom Kit setup can use a different discovery policy. Treat model-written or untrusted skill text as untrusted input.

## Put readable resources in context

Kit can name a skill's bundled `scripts/`, `references/`, and `assets/` in activation text using a host path. That path is not copied into the tool sandbox merely because it is in a skill bundle.

- Put essential guidance in the skill body.
- Put a file the model must open in `context/`.
- Refer to that file by its path relative to the sandbox working directory.

For example, put a template at `context/templates/report.md` and tell the skill to read `templates/report.md`. Do not tell the model to open a host skill path. Do not bundle secrets: build embeds skill and context data.

## Managed setup and completion checks

Use `WithKitSetup` when a host must register public Kit hooks before the first prompt. Use `WithCompletionHook` to check a normal final response and optionally request another model turn. Both receive a `RunScope`; its `Exec` uses the same lazy sandbox as the model tools. Callback code itself remains host code.

Do not retain `RunScope.Exec` after execution ends. Closure state is not durable. An interrupted completion check can run again, so its external effects must tolerate retries. Set a finite continuation limit. Completion checks do not run on human-input suspension or model failure, and they delay the final outcome, not live draft events.

See the [root API godoc](https://pkg.go.dev/github.com/mark3labs/bonnie), [Kit tool API](https://pkg.go.dev/github.com/mark3labs/kit/pkg/kit#NewTool), and [Sandboxes](/guides/sandboxes) for the execution contracts.
