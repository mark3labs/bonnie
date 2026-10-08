---
title: Quick start
description: Create an agent tree, ask for human input, inspect its durable run, and compile a static binary.
---

Complete [Installation](/installation) first. These steps need Linux, Go 1.27 or newer, and a provider key.

## 1. Create an agent

```bash
bonnie init my-agent --model anthropic/claude-sonnet-4-5
cd my-agent
go mod tidy
```

The scaffold contains:

```text
my-agent/
  instructions.md   # system prompt
  main.go           # options you own
  bonnie_gen.go     # generated registration
  go.mod
  skills/           # authored skills
  context/          # seed files copied to each run
```

Edit `instructions.md`:

```text
You are a deployment planning assistant.
Before you prepare a deployment plan, use ask_human to ask which region to use.
Write the plan to deployment-plan.md. Do not deploy anything.
```

Do not edit `bonnie_gen.go`. The CLI generates it again. Put a model-readable seed file in `context/` if needed. Seed files are copied into the sandbox; they are not automatically added to the prompt.

## 2. Run the agent

```bash
bonnie dev
```

The CLI builds the tree, starts the HTTP server, and opens its terminal chat interface. It rebuilds when authored files change. Ask:

```text
Prepare a deployment plan for my web service.
```

The model decides whether to call `ask_human`. When it does, the run becomes `waiting`. Enter an answer to continue. Approval is not an automatic policy; prompts alone do not enforce it.

Use `/help` for terminal commands. Enter sends a message; Shift+Enter adds a line. `/new` starts a separate conversation. `/retry` sends the last message again as a new turn and can repeat external actions.

## 3. Use HTTP instead

For a predictable HTTP exercise, stop the interactive development server, then start it without the TUI:

```bash
bonnie dev --tui=false
```

In another terminal:

```bash
curl -sS http://localhost:8080/bonnie/v1/health
curl -sS http://localhost:8080/bonnie/v1/runs \
  -H 'Content-Type: application/json' \
  -d '{"address":"quick-start","text":"Prepare a deployment plan. Ask me the region with ask_human first."}'
```

The reply contains `run_id` and `state`. If it is `waiting`, save the returned run ID and answer it:

```bash
curl -sS http://localhost:8080/bonnie/v1/runs/REPLACE_WITH_RUN_ID/respond \
  -H 'Content-Type: application/json' \
  -d '{"responses":[{"text":"eu-west-1"}]}'
```

You can stop the server while the run waits, then restart it from the same tree and journal directory before sending the answer. Keep `.bonnie` and its sandbox files. The process is not required while the run waits.

The default HTTP channel has no caller authentication. Use these commands only on a trusted local host. Configure authentication before exposing the service. See [HTTP](/channels/http).

## 4. Inspect the journal

From the agent directory:

```bash
bonnie runs list --journal .bonnie
bonnie runs show --journal .bonnie REPLACE_WITH_RUN_ID
```

These commands read the journal directly and work with the server stopped. Send another message to the same `quick-start` address to continue its conversation. Resetting an address retires its old run; it does not remove history.

## 5. Build the agent

Stop the development server, then run:

```bash
bonnie build --output bin/my-agent
./bin/my-agent --help
./bin/my-agent --addr 127.0.0.1:8080
```

The binary embeds instructions, tools, skills, and context files. It does not embed provider credentials. Set credentials on the target host. Keep the journal on a persistent local volume and use one server for it.

## Next steps

- [Core concepts](/concepts): what a run preserves and what can repeat.
- [Agent trees](/guides/agent-trees): discovery, generated wiring, and shared directories.
- [Tools and skills](/guides/tools-and-skills): add capabilities.
- [Channels](/channels/overview): connect a bot or task worker.
- [Deployment](/guides/deployment): isolate tools, authenticate requests, and back up state.
