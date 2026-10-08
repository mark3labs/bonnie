---
title: Agent Trees
description: Create, test, and build a BONNIE agent from instructions, tools, skills, and context files.
---

# Agent Trees

An agent tree is a Go module with files at known paths. BONNIE uses these paths to find the agent's instructions, tools, skills, and seed files. There is no BONNIE configuration manifest. Put other settings in `main.go` as Go options.

BONNIE is experimental, pre-1.0 software. Use test data first. Do not use a release for work whose loss would cause harm.

## Create a tree

You need Linux, Go 1.27 or newer, the BONNIE CLI, and a model provider key. The default sandbox needs Linux 5.13 or newer with Landlock enabled. See [Sandboxes](/guides/sandboxes) for other backends.

```bash
export ANTHROPIC_API_KEY='YOUR_PROVIDER_KEY'
bonnie init report-agent --model anthropic/claude-sonnet-4-5 --tools
cd report-agent
go mod tidy
bonnie dev
```

`--tools` is optional. It adds an example echo tool. `bonnie init .` can create a tree in the current directory. Init checks its output paths first. If a target file exists, it reports the files and refuses to replace them.

A tree has this layout:

```text
report-agent/
├── instructions.md
├── main.go
├── bonnie_gen.go
├── go.mod
├── go.sum
├── context/
│   └── operations.csv
├── skills/
│   └── reporting/
│       └── SKILL.md
└── tools/
    └── echo/
        └── tool.go
```

`go mod tidy` creates or updates `go.sum`. The scaffold starts `context/` and `skills/` with `.gitkeep` files, not the example data above.

| Path | Purpose | Owner |
|---|---|---|
| `instructions.md` | System prompt | You |
| `main.go` | Model, channels, sandbox, and other options | You |
| `tools/<name>/` | Compiled Go tool package | You |
| `skills/` | Skill names, descriptions, and activation bodies | You |
| `context/` | Seed files copied into each run's working files | You |
| `bonnie_gen.go` | Tool imports and embedded tree data | BONNIE |
| `.bonnie/` | Default journal and runtime data | Runtime |

Do not edit `bonnie_gen.go`. Dev and build generate it again. Commit the authored files and module files. Do not commit provider keys, `.env`, or runtime data.

## Write instructions

Put the agent's task and response rules in `instructions.md`:

```markdown
You prepare operations reports.

Read operations.csv in the working directory. Use the reporting skill when
its description applies. Write report.md with totals and data problems.
Do not invent missing values. Ask the operator when required data is absent.
Do not send a report to an external service.
```

Instructions guide the model. They are not a security policy. Use sandbox permissions and tool-side checks to enforce restrictions.

The default `main.go` needs only the serving call and any options you choose:

```go
package main

import "github.com/mark3labs/bonnie"

func main() {
    bonnie.New(
        bonnie.WithModel("anthropic/claude-sonnet-4-5"),
        bonnie.WithName("report-agent"),
    ).Serve()
}
```

Do not repeat the tree's prompt, skills, or context paths in options without a specific need. `WithSystemPrompt` overrides the instructions file. `WithAgentFactory` is a different execution path: the custom factory owns the agent and does not automatically receive the tree's instructions, skills, or generated tools.

## Separate instructions from working files

`context/operations.csv` is a file, not prompt text. BONNIE copies it into each run's sandbox. The agent must use a file tool to read it. Seeding does not replace a file that the run already has.

This has two effects:

- Separate runs have separate working files by default.
- A change to an authored seed file does not replace a run's existing copy.

Use a new run when you test changed seed data. Dev does not watch `context/`; restart dev after seed changes, or build again before deployment. Never put a secret in `context/`: the model can read it, and build embeds it in the binary.

Use [Tools and Skills](/guides/tools-and-skills) to add capabilities. Tools resolve at build time. There is no runtime plugin loader. Each tool directory must export `func Tool() kit.Tool`. Discovery rejects missing factories and detectable duplicate declared tool names. Keep the directory name and the name passed to `kit.NewTool` the same; use a unique runtime name.

## Work with dev

```bash
bonnie dev --dry-run
bonnie dev --addr 127.0.0.1:8080
# For a non-interactive process:
bonnie dev --addr 127.0.0.1:8080 --tui=false
```

The dry run prints the discovery plan without writing or building. Dev watches `main.go`, `instructions.md`, `skills/`, `tools/`, and module files. It regenerates wiring, builds, and restarts the child after changes. It sends SIGTERM and waits for the shutdown timeout before the next child starts.

Without an explicit address, dev selects a free loopback port from 8080 upward. Read its startup output to find the selected port. The terminal interface uses the same HTTP channel as other clients.

A restart keeps the journal. A waiting run still needs an answer; restarting does not grant approval. Scheduled cron work is off in dev unless you use `--schedule-clock`.

```bash
curl -sS -H 'Content-Type: application/json' \
  http://127.0.0.1:8080/bonnie/v1/runs \
  -d '{"address":"report-test","text":"Prepare a report from operations.csv."}'

bonnie runs list --journal .bonnie
```

The address continues the same conversation. Use a different address for an independent test. In terminal chat, `/new` creates a separate conversation. `/retry` sends the last message as a new turn; it does not undo tools or earlier history.

## Build and test

```bash
go test ./...
bonnie build --dry-run
bonnie build --output bin/report-agent
```

The output path is relative to the tree unless it is absolute. Build generates `bonnie_gen.go`, embeds instructions, skills, and context files, and compiles with `CGO_ENABLED=0`. The target host does not need Go or the BONNIE CLI. It still needs a compatible Linux system, provider credentials, storage, and the selected sandbox's runtime requirements.

Pin released dependencies in `go.mod` and keep `go.sum`. A release CLI scaffolds a BONNIE version requirement; a development CLI may leave it for `go mod tidy` to resolve. Examine the resulting version before you ship. Do not deploy a local `replace` directive.

Test custom tools without a live model. Test the agent with harmless data before you permit external actions. See [Deployment](/guides/deployment) for storage, authentication, shutdown, and backup requirements.

## Read the API contract

- [Agent discovery and generation](https://pkg.go.dev/github.com/mark3labs/bonnie/agent)
- [Root package options](https://pkg.go.dev/github.com/mark3labs/bonnie)
- [Troubleshooting](/guides/troubleshooting)
