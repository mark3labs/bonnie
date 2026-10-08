---
title: BONNIE
description: Durable agent runs for Go. Keep the conversation after a crash, wait for human input, and connect through HTTP or messaging channels.
toc: false
---

<div class="bonnie-hero">
  <img src="/logo.png" alt="BONNIE pixel-art logo with cyan and pink accents" width="320" />
  <p>Durable agent runs for Go.</p>
</div>

<img class="bonnie-demo" src="/quick-start.gif" alt="Terminal recording: create a BONNIE agent with openai/gpt-6.1-sol, edit its personality instructions, fetch dependencies, then chat about KITT's mission recovery and turbo-boost approval through bonnie dev." width="1100" height="720" />

Create an agent and join a Knight Rider-themed conversation with `openai/gpt-6.1-sol`. Follow the [Quick start](/quick-start) for the commands.

Build an agent once. Keep its conversation after a process stops. Wait days for a human answer without keeping an agent process running. Connect through HTTP, Slack, Discord, Telegram, GitHub, or NATS.

**BONNIE is early and experimental.** The API can change. No release is proven in production. Do not use it for work whose loss would hurt.

## Start in five minutes

```bash
curl -fsSL https://raw.githubusercontent.com/mark3labs/bonnie/master/install.sh | bash
export ANTHROPIC_API_KEY=your-provider-key
bonnie init my-agent --model anthropic/claude-sonnet-4-5
cd my-agent
go mod tidy
bonnie dev
```

Use Linux with Landlock enabled and Go 1.27 or newer to build an agent. Read [Installation](/installation) first if your host is not ready. Then follow the [Quick start](/quick-start).

## Choose your path

| Your goal | Start here |
| --- | --- |
| Build your first agent | [Quick start](/quick-start) |
| Understand durability and human input | [Core concepts](/concepts) |
| Add instructions, context files, and Go tools | [Agent trees](/guides/agent-trees) and [Tools and skills](/guides/tools-and-skills) |
| Choose containment or stronger isolation | [Sandboxes](/guides/sandboxes) |
| Connect an application or bot | [Channels](/channels/overview) |
| Run tasks on a schedule | [Scheduling](/guides/scheduling) |
| Ship a static binary | [Deployment](/guides/deployment) |
| Embed BONNIE in Go | [Runtime API](/reference/runtime) and [Options](/reference/options) |
| Find a flag or inspect a run | [CLI reference](/reference/cli) |
| Diagnose a failure | [Troubleshooting](/guides/troubleshooting) |

## What BONNIE adds to Kit

BONNIE wraps the [public Kit SDK](https://go-kit.dev). It adds a SQLite journal, lossless conversation replay, human-input suspension, channel routing, and sandbox lifecycle management. It does not fork Kit.

The journal stores typed messages, including tool calls and results. A committed tool-calling step is stored atomically. A new process can restore that history instead of asking the model to reconstruct it.

**Durability is not exactly-once execution.** An external action can finish before its journal commit. A crash in that interval can cause the action to repeat. Use idempotency keys for payments, deployments, and other external actions.

## Configuration is code

An agent tree has `instructions.md`, `context/`, `skills/`, and optional Go packages under `tools/`. Settings belong in `main.go`. There is no BONNIE configuration manifest.

```go
package main

import "github.com/mark3labs/bonnie"

func main() {
    bonnie.New(
        bonnie.WithModel("anthropic/claude-sonnet-4-5"),
    ).Serve()
}
```

`bonnie build` embeds the authored files and compiles one static binary. The target host does not need Go or the BONNIE CLI.

## Before deployment

- The default Landlock sandbox confines filesystem access. It does not block network access or provide a separate kernel.
- Custom Go tools run in the host process. Their permissions and secrets are your responsibility.
- Human approval stops the next model step, not another tool call in the same step.
- Authenticate HTTP callers. Use one server to execute runs from a journal. SQLite write safety does not coordinate turns across servers.
- Keep the journal and sandbox data. Waiting runs must retain both.

Read [Deployment](/guides/deployment) for the operating checklist and [Sandboxes](/guides/sandboxes) for the security boundaries.

## Project resources

- [Source and issues](https://github.com/mark3labs/bonnie)
- [Go package reference](https://pkg.go.dev/github.com/mark3labs/bonnie)
- [Release history](https://github.com/mark3labs/bonnie/blob/master/CHANGELOG.md)
- [Example agent trees](https://github.com/mark3labs/bonnie/tree/master/examples)
