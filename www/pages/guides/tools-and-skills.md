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

Each immediate tool directory must export `func Tool() kit.Tool` or `func Tool(scope bonnie.ToolScope) kit.Tool` in non-test Go code. The generator imports these packages into `bonnie_gen.go`. Do not add the same tree tool to `main.go` again. Discovery rejects missing factories and detectable duplicate literal names; it is not a complete analysis of names produced by helper functions. Keep names unique yourself.

For a host that constructs tools directly, use `bonnie.WithTools(myTool)`. For a low-level sandbox agent, use `kit.WithExtraTools(myTool)` with `sandbox.Agent`. Kit's `Tools` option replaces a tool set; `ExtraTools` adds to it. Do not replace the set or re-enable host core tools without examining the security effect.

## Call a model from a tool

Use a scoped tool when its callback needs the current agent or run. This avoids a global agent pointer and gives each Start or Resume its own tool instance. Create `tools/review/tool.go`:

```go
package review

import (
    "context"
    "fmt"

    "github.com/mark3labs/bonnie"
    kit "github.com/mark3labs/kit/pkg/kit"
)

type input struct {
    Text string `json:"text" description:"Text to review."`
}

// Tool returns a review tool bound to the current execution.
func Tool(scope bonnie.ToolScope) kit.Tool {
    return kit.NewTool("review", "Review text with a child agent.",
        func(ctx context.Context, in input) (kit.ToolOutput, error) {
            if len(in.Text) == 0 || len(in.Text) > 8192 {
                return kit.ErrorResult("text must contain 1 to 8192 bytes"), nil
            }
            result, err := scope.Kit.Subagent(ctx, kit.SubagentConfig{
                Prompt:    "Review this text:\n\n" + in.Text,
                NoSession: true,
                Tools:     []kit.Tool{}, // Do not give the child tools.
            })
            if err != nil {
                return kit.ToolOutput{}, fmt.Errorf("bonnie: review text: %w", err)
            }
            return kit.TextResult(result.Response), nil
        })
}
```

Run `bonnie dev` or `bonnie build` to generate the scoped registration. Do not register the tree tool again in `main.go`. Existing zero-argument tools need no change.

`scope.Kit` is the current `*kit.Kit`. The embedded `RunScope` supplies `RunID`, `Session`, and `Exec`. Use `scope.Exec` for sandbox commands. Do not retain the scope after execution ends. Use `Kit.Subagent`, not `Kit.Prompt`, from an active tool: the child has a separate agent loop and conversation. With no `Model` override, it uses the parent's model and provider settings. Set `SubagentConfig.Model` to select another model. An explicit empty `Tools` slice prevents tool use; omitted tools use Kit's inherited default tool set.

**Child calls are not durable BONNIE runs.** `NoSession: true` prevents a child session file; it does not add crash recovery. The outer tool result enters BONNIE's journal, but unfinished child work does not. An interrupted outer tool can cause another model call and another charge. Pass the tool context to the child and make any external effects safe to repeat.

For tools constructed by a host application, register a factory:

```go
// Import the review package from your application's module.
bonnie.New(
    bonnie.WithToolFactory(func(ctx context.Context, scope bonnie.ToolScope) ([]kit.Tool, error) {
        return []kit.Tool{review.Tool(scope)}, nil
    }),
).Serve()
```

BONNIE calls discovered factories first, then application factories in registration order, then `WithKitSetup` callbacks. Each factory runs before the first prompt on every Start or Resume. Factory errors, a nil factory, or a nil returned tool stop agent construction. A nil or empty tool slice is permitted. Keep tool names unique across static and scoped registrations.

Use `WithTools` for tools that need no run scope. Keep `WithKitSetup` for hooks and other advanced setup. `WithToolFactory` cannot be combined with `WithAgentFactory`. See [Options](/reference/options#managed-setup-and-completion) for the exact types and callback contracts.

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

## Workspace skills

Workspace discovery is off by default. Enable it in Go:

```go
bonnie.New(
    bonnie.WithSkills(""), // Optional: disable authored tree skills only.
    bonnie.WithWorkspaceSkills(bonnie.WorkspaceSkills{
        Exclude: []string{"archives/**", "fixtures/**"},
    }),
).Serve()
```

This lets repository metadata enter model context. Descriptions and bodies are
untrusted input and can contain hostile instructions. Progressive disclosure
is not security approval. Repository skills apply to their source scope; they
do not replace main instructions or change tool permissions. Activating a skill
does not authorize execution of its scripts.

The scanner uses sandbox filesystem operations. It recursively recognizes only
`<scope>/.agents/skills/<bundle>/SKILL.md` and
`<scope>/.kit/skills/<bundle>/SKILL.md`. Plain `skills/`, loose Markdown files,
and nested asset files do not qualify. Each file needs complete frontmatter,
nonempty name and description, and a nonempty body. `when: always` is forced to
on-demand. Invocation-disabled files are not advertised. Unknown metadata
cannot grant tools. File and directory symlinks, nonregular files and multiply
linked files are rejected. No host home directory is scanned.

### Names, scope and activation

Names have the form `workspace/<scope>/<declared-name>--<source-hash>`.
Readable scope and name components use ASCII letters, digits, hyphens and
underscores, capped at 24 bytes each; other characters become hyphens. Root
scope is `root`. The full lowercase SHA-256 suffix hashes the exact relative
source path. All names get a suffix, so scan order and later collisions cannot
rename existing entries. Moves and declared-name changes select a new identity.
Workspace names never replace authored tree or explicit Kit skills.

Only name, description, source location and scope/trust text enter the catalogue.
`activate_skill` returns the selected body, version hash, source and relative
base directory. Read bundled assets through ordinary sandbox tools relative to
that base. BONNIE never enumerates host assets for workspace entries. Assets
are not journal snapshots: they can change or disappear while a saved body
remains available.

### Ignore rules and limits

Nested `.gitignore` rules use a Git-compatible matcher. Later rules and valid
`!` exceptions apply, but an excluded parent is pruned before any child can be
re-included. A regular `.git` file or directory starts an independent repository
boundary and resets inherited Git rules. Gitdir pointers are not followed.
Outside repositories, workspace `.gitignore` rules apply downward. Discovery
uses these rules even for tracked files. It does not read `.git/info/exclude`,
Git configuration, global excludes or ancestors of the workspace.

Mandatory directory segments are `.git`, `node_modules`, `vendor`, `.direnv`,
`.venv`, `venv`, `__pycache__`, `.cache` and `.bonnie`. `Exclude` adds
workspace-root-relative directory patterns. `/` separates segments; `*`, `?`
and character classes match within a segment; a whole `**` segment matches
zero or more segments. A matching directory and all descendants are pruned.
Absolute paths, empty/dot/dot-dot segments, negation and malformed classes are
invalid. Neither Git exceptions nor explicit patterns can remove mandatory
exclusions. There is no ignore override.

| Limit | Default | Meaning |
| --- | --- | --- |
| `MaxDepth` | 12 | Directory segments below workspace root (root depth 0); file parent must fit |
| `MaxSkills` | 100 | Selected workspace skills |
| `MaxSkillBytes` | 128 KiB | Original bytes including frontmatter |
| `MaxMetadataBytes` | 32 KiB | Exact escaped Kit workspace catalogue block, including fixed text |

Zero selects defaults; negative limits fail before resources open. Defensive
ceilings are depth 256, 10,000 selected skills, 16 MiB per file and 16 MiB
metadata. Scans also have a 30-second deadline, 10,000 visited entries, 64 KiB
per ignore file, 1 MiB aggregate ignore bytes, and 32 MiB retained candidate
content. Diagnostics are capped at 100 per scan with 512-byte path excerpts.
Journal snapshot decoding is capped at 64 MiB. Selection is in exact source
path order. An over-count or over-metadata entry is skipped, not truncated;
later entries are still considered. Bodies are never truncated to fit.

Malformed, unstable, inaccessible and oversized skills are skipped with
bounded diagnostics through `WithActivityLogger`. Invalid edits remove the
live entry at the next complete boundary. Unreadable or invalid ignore rules
fail closed for their subtree. Depth limits report omissions. Global timeout,
entry/candidate exhaustion or root failure keeps the last complete catalogue
(empty initially), rather than publishing partial removals. Cancellation stops
the request. Every candidate is read twice and its bytes/stat identity compared.
This detects unstable writes, not all temporarily stable intermediate content.
Later scans retry incomplete files.

### Timing and recovery

Enabling discovery opens the sandbox at startup, even before a model tool call.
BONNIE scans at startup/resume and before later model requests in the same tool
loop, after the previous tool batch and step checkpoint. A changed catalogue
is appended to the journal before its metadata is exposed. Activation uses
that selected immutable body, not the current source file. Identical scans
append no new catalogue records. Append failures and corrupt restored content
stop execution before a request with unrecorded instructions.

A resumed execution restores its recorded catalogue before tool recovery. Its
first request uses that saved version even if files changed or disappeared;
the following boundary can select the current workspace. Earlier activation
results remain lossless history. Catalogue records are run-scoped, append-only
metadata, not branch messages; clear and branch operations do not erase them.
Protect and retain the journal as instruction-content storage.

### Remote sandboxes

Custom sandboxes must implement `sandbox.SkillDiscovery`; missing support fails
clearly with no host fallback. Local and Landlock use rooted no-follow Linux
opens. **Built-in Docker and Microsandbox discovery is not enabled in this WIP:**
backend-controlled helper storage is still required. They fail closed instead
of executing a helper that model commands could replace. The `bonnie-skillfs`
helper and bounded JSON protocol are prepared, but their presence is not proof
of safe installation. Linux `openat2` support is required; there is no weaker
fallback.

Release builds package Linux amd64 and arm64 helpers. Source builders must run
`bash scripts/build-skillfs.sh` before compiling BONNIE or an agent binary that
uses remote discovery. `task build` and GoReleaser do this. Packaged helpers alone do not enable remote scanning. A secure backend-owned
installation path must be implemented before this WIP is complete. Sandbox
policy and compute lifecycle remain in force. No watcher, arbitrary host search root,
manual registration callback or configuration file is added.

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
