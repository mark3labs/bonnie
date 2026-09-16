# BONNIE examples

Every example is a real program. `go build ./...` compiles them, so they
cannot go stale.

They are **library examples, not agent trees.** Each one is a package inside
BONNIE's own module, which is what keeps them compiled and fresh, so each runs
with `go run`. Your own agent is a tree instead — its own Go module from
`bonnie init`, run with `bonnie dev` and shipped with `bonnie build`. Run
`bonnie init myagent` to see that shape.

You need a model provider key. Kit reads the usual environment variables:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
# or OPENAI_API_KEY, or GEMINI_API_KEY
```

## github-bot

An agent that lives on GitHub: mention it in an issue, a pull request, or a
review thread and it answers there, and the thread is a durable run.

```bash
go run ./examples/github-bot
```

It needs a GitHub App and a public URL, so it has its own instructions —
including how to make the App by hand and how to run the real tree with
`bonnie dev`: see [github-bot/README.md](github-bot/README.md).

## slack-bot

An agent that lives in Slack: mention it in a channel, reply in a thread it
joined, or DM it, and it answers there, and the conversation is a durable run.

```bash
go run ./examples/slack-bot
```

It needs a Slack app and a public URL, so it has its own instructions —
including how to make the app by hand and how to run the real tree with
`bonnie dev`: see [slack-bot/README.md](slack-bot/README.md).
