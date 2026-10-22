---
title: CLI
description: BONNIE commands, exact flags and defaults, compiled-agent flags, and operator inspection commands.
---

# CLI

The developer CLI and a compiled agent are different executables. `bonnie serve` is a generic host with no tree. `bonnie dev` and `bonnie build` run the configuration in a tree's `main.go`. A compiled tree calls `bonnie.New(...).Serve()` and has its own smaller flag set.

Use `bonnie --help` and `bonnie COMMAND --help` for command help. `bonnie version` and `bonnie --version` report the CLI version. `eval` is planned, not an implemented command.

See [Agent Trees](/guides/agent-trees) for the development sequence and [Options](/reference/options) for code configuration.

## `bonnie init [dir]`

Creates an agent tree in `dir`, default `.`. It does not overwrite target files. The tree is an independent Go module, not a package in the BONNIE module.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--model` | empty | Model string to write into `main.go`, for example `anthropic/claude-sonnet-4-5` |
| `--tools` | `false` | Add a sample Go tool under `tools/echo` |

```bash
bonnie init report-agent --model anthropic/claude-sonnet-4-5 --tools
cd report-agent
go mod tidy
bonnie dev
```

There is no manifest. Instructions are `instructions.md`; skills are under `skills/`; seed files are under `context/`; other settings are Go options.

## `bonnie dev [dir]`

Generates tool wiring, builds the tree, starts a serving child, and watches for changes. Default directory is `.`. The TUI talks to the child's HTTP channel. Its conversation address is `dev-<tree-directory-name>`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Print discovery without writing, building, or watching |
| `--tui` | `true` | Open terminal chat; use `--tui=false` for a non-interactive host |
| `--web` | `false` | Enable the child's browser interface and open it in a browser; turns off the default TUI. Explicit `--tui=true` with `--web` is refused. |
| `--schedule-clock` | `false` | Enable the child's cron schedule clock |
| `--addr` | empty | Explicit bind address; otherwise try loopback ports 8080 through 8179 |
| `--shutdown-timeout` | `30s` | Parent wait before killing the old child during restart |
| `--no-self-modify` | `false` | Leave the default `modify_agent` tool out of the dev build |

Changes to code, instructions, skills, and module files cause regeneration and rebuild. Generated files, `.bonnie`, `.git`, and `.direnv` are excluded. The authored context directory is not watched: seed changes affect later runs, not existing working files. A failed rebuild leaves the current child running. A successful rebuild sends SIGTERM to the old child and restarts it at the same address. The journal retains the conversation; the TUI reconnects.

The dev build gives the agent the `modify_agent` tool from package `selfmod`: ask the agent to change its own instructions, tools, skills, or context files. The tool is added with `go build -overlay`, so no file is written to the tree and `bonnie build` never includes it. The child gets `BONNIE_DEV_ROOT`, the absolute tree root. See [Self-Modification](/guides/self-modification).

The dev restart timeout is not a new root option in the child. Set `WithShutdownTimeout` in code when you need to change the child's own drain period.

## `bonnie build [dir]`

Generates `bonnie_gen.go`, embeds tree data, and runs `go build` with `CGO_ENABLED=0`. Default directory is `.`. The build host needs Go; the target does not need Go or BONNIE installed. The selected sandbox still needs its runtime prerequisites.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Print files, discovered tools, and embed plan without writing or building |
| `--output` | empty | Output path relative to the tree, or an absolute path; default is the sanitized module base name under the tree |

```bash
bonnie build --dry-run
bonnie build --output bin/report-agent
./bin/report-agent --help
```

See [Deployment](/guides/deployment) for persistent storage and host requirements.

## `bonnie chat`

Connects terminal chat to an existing HTTP server. It does not start one.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | `127.0.0.1:8080` | Server address or full HTTP/HTTPS URL; `:9090` means loopback port 9090 |
| `--run` | empty | Existing run ID to continue with its saved history |
| `--token` | empty | Bearer token sent on each HTTP request |

Enter sends, Ctrl+W requests cancellation, and Ctrl+C quits. `/cancel` is also available. Use `--run <run-id>` to load an existing run and its saved history. An unknown ID shows an error and does not create a run. Without `--run`, chat uses the default `tui-default` conversation address. Closing the terminal does not cancel the server's durable turn.

## `bonnie serve`

Serves a generic model agent over HTTP. It does not read a tree's `main.go`, instructions, skills, or context seed directory.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | `:8080` | Listen address |
| `--journal` | `.bonnie` | SQLite journal directory |
| `--model` | empty | Kit model selection |
| `--system-prompt` | empty | Direct system prompt |
| `--sandbox` | `landlock` | `landlock`, `docker`, `microsandbox` (alias `msb`), `local`, or `auto` |
| `--sandbox-image` | empty | Image for Docker or Microsandbox; refused for Landlock or Local |
| `--sandbox-deny-network` | `false` | Block sandbox egress; requires an enforcing backend |
| `--shutdown-timeout` | `30s` | Drain timeout |
| `--slack` | `false` | Mount Slack with environment credentials |
| `--discord` | `false` | Mount Discord with environment credentials |
| `--telegram` | `false` | Mount Telegram with environment credentials |

`auto` tries Microsandbox, then Docker, then Landlock. With an image, Landlock is not a candidate. Local is never an automatic fallback. `none` is refused. Local provides no isolation and emits a warning. Landlock cannot enforce `--sandbox-deny-network`; use Docker or Microsandbox. Backend unavailability is an error, not permission to run without confinement.

```bash
bonnie serve --addr 127.0.0.1:8080 \
  --sandbox docker --sandbox-deny-network \
  --model anthropic/claude-sonnet-4-5
```

Generic serve has no Iroh, GitHub, NATS, HTTP authentication, automatic cleanup, or schedule-definition flags. Configure these through code in a tree or a library host. Do not expose unauthenticated HTTP to untrusted callers. See [HTTP](/channels/http), [Slack](/channels/slack), and [Sandboxes](/guides/sandboxes).

Iroh is configured in an agent tree with `bonnie.WithIroh`, not with a CLI
flag. `bonnie chat` does not accept Iroh tickets. Pairing is programmatic;
there is no built-in `bonnie pair` command. See [Iroh](/channels/iroh) for
peer approval, stable keys, and the Go client.

## Provider login

Both the CLI and compiled agents include `auth login openai`, `auth status`, and
`auth logout openai`. These manage Kit's shared stored model-provider credentials,
not HTTP bearer tokens or MCP authentication. Login supports ChatGPT/Codex
subscription OAuth, not general OpenAI API OAuth. It prints a URL rather than
opening a browser. The browser must reach `localhost:1455` on the login host;
forward that loopback port for remote use. There is no manual code-entry fallback.
Login has a five-minute limit. Status reports stored credentials only: it does
not check environment keys or token validity. See [Installation](/installation#provider-login).

## Compiled-agent commands

By default, compiled agents include `serve`, `auth login/status/logout`, `chat`,
`runs list/show/inspect`, `schedules list/show/history/trigger`, and `version`.
With no subcommand, the binary serves the agent; explicit `serve` has the same action and flags.
Other commands do not start a server. Help and version do not require a model
or open storage. `chat` and `schedules` use the configured HTTP address; `runs`
uses the configured local journal and accepts `--journal`.

Disable operator commands with `WithAuthCommand(false)`, `WithChatCommand(false)`,
`WithRunsCommand(false)`, `WithSchedulesCommand(false)`, or `WithVersionCommand(false)`.
This does not restrict stored credentials, files or HTTP routes, or remove
command dependencies. Disable a built-in before registering the same name.
See [Agent Commands](/guides/agent-commands).

## Compiled-agent flags

An agent that calls `Agent.Serve()` registers these framework flags. A host can register additional Go flags before Serve; existing names are retained. Use `WithCommand` to add Cobra root flags, persistent flags, subcommands, and hooks. `bonnie build` includes them in the agent binary, not in the developer `bonnie` CLI. See [Agent Commands](/guides/agent-commands).

Keep the root serving action and `-addr` support for `bonnie dev`. Apply parsed custom options with `Agent.Configure` in a pre-run hook. Inspection commands can use `Agent.WithJournal`; execution commands can use `Agent.WithRuntime` without starting HTTP.

| Flag | Registered default | Meaning |
| --- | --- | --- |
| `--addr` | empty | Non-empty value overrides `WithAddr` |
| `--model` | empty | Non-empty value overrides `WithModel` |
| `--sandbox` | empty | Select a provider permitted by `WithSandboxes` or `WithSandbox`; otherwise only Landlock is permitted |
| `--schedule-clock` | `true` | Controls cron evaluation for this process |
| `--web` | `false` | Serve `/web`; an explicit flag overrides `WithWebUI`. Does not open a browser. |

Single-dash long names such as `-addr` remain supported. With no sandbox flag, the first permitted provider is used. Selection retains that provider's configured options. Compiled agents do not automatically inherit generic serve flags such as `--journal`, `--system-prompt`, or `--sandbox-image`. Fang's automatic version, completion, and manpage features are disabled on this surface; BONNIE's explicit `version` subcommand is separate and enabled by default. Help does not open storage or check backend availability.

`Agent.Run(ctx)` reads none of these flags. It uses code options, including the first permitted sandbox and the configured clock setting.

## `bonnie runs`

These commands open the journal directly and work with the server stopped. All use `--journal .bonnie` by default in the developer CLI. Opening storage can create the SQLite database or import legacy JSONL records; this is not a read-only filesystem tool.

| Command | Flags | Output |
| --- | --- | --- |
| `runs list` | `--journal`, `--state` (empty means all), `--json` (false) | Run ID, title, state, step count, and recent text |
| `runs show <run-id>` | `--journal`, `--json` (false) | Timeline; JSON gives raw records |
| `runs inspect [run-id]` | `--journal` | Read-only terminal inspector with filtering, refresh and record payload view; opening the journal can still create or migrate storage |

Reserved `bonnie.*` bookkeeping runs are hidden from list output. The table labels the count `STEPS`; the JSON field is named `turns` but counts `RecordStep` records, not logical user turns. Raw records can contain private prompts, tool input, and media payloads.

```bash
bonnie runs list --journal /var/lib/report-agent --state waiting
bonnie runs show RUN_ID --journal /var/lib/report-agent --json
```

## `bonnie sandbox prune`

Deletes working files of terminal runs; it keeps history and state. Stop the server first. Cleanup does not coordinate with another process or Runner.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--journal` | `.bonnie` | Journal directory |
| `--sandbox` | `docker` | Backend used by these runs: Landlock, Docker, Microsandbox/`msb`, or Local |
| `--sandbox-image` | empty | Image needed to construct the selected backend |
| `--dry-run` | `false` | Report eligible terminal runs without deleting files |
| `--recheck` | `false` | Retry cleanup of terminal runs even when cleanup is already recorded |

Use the actual backend. Landlock and Local use the journal's `workspaces` root. Microsandbox uses the same runtime lookup as agent startup, including the journal-local installation; prune does not download a runtime. Use the same runtime environment and backend context as the server. To remove a sandbox that remains after a cleanup record, stop the server and run `bonnie sandbox prune --journal .bonnie --sandbox microsandbox --recheck`. Add `--dry-run` to check eligibility without deletion. A recheck keeps non-terminal runs and does not change old journal records. Successful cleanup is recorded so later passes skip it until another terminal checkpoint. Completed runs can receive new turns with an empty directory after pruning. There is no built-in transcript deletion command.

## `bonnie schedules`

These commands call the running schedule HTTP API, not journal files.

| Command | Arguments | Extra flags |
| --- | --- | --- |
| `schedules list` | none | none |
| `schedules show <name>` | schedule name | none |
| `schedules history <name>` | schedule name | none; prints the history array |
| `schedules trigger <name>` | schedule name | `--id` (empty), `--scheduled-at` (empty), `--kind` (`manual`) |

All four accept `--url` (default `http://127.0.0.1:8080`) and `--token` (empty). Token sets the bearer Authorization header. Requests have a 30-second HTTP timeout.

Manual triggers without ID or scheduled time use the current UTC time and a generated ID. External triggers require a stable ID and RFC3339 scheduled time. The server must configure `WithScheduleTriggerAuthorizer` to mount the trigger route.

```bash
bonnie schedules trigger daily-report --kind external \
  --id report-2026-10-07 --scheduled-at 2026-10-07T09:00:00Z
```

See [Scheduling](/guides/scheduling) for occurrence identity, revisions, overlap, and retry limits.
