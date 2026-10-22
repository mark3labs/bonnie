---
title: Self-Modification
description: Let an agent change its own instructions, tools, skills, and context files in development, or propose a change as a draft pull request from a deployment.
---

# Self-Modification

Package `selfmod` lets an agent change its own source. It has two tools with two trust models:

| Tool | Option | Where | What it changes |
| --- | --- | --- | --- |
| `modify_agent` | `selfmod.Local` | `bonnie dev` only | The agent tree on disk. The dev loop rebuilds and restarts the agent. |
| `propose_agent_change` | `selfmod.Remote` | A deployed agent | Nothing in the deployment. It opens a draft pull request for human review. |

In both tools, a coding subagent does the work in a staged copy of the tree. The copy holds only authored files. The subagent never sees the live tree, the journal, a `.env` file, or a credential.

## Change the agent in development

`bonnie dev` gives the agent `modify_agent` by default. Ask the agent to change itself:

```bash
bonnie dev
```

```text
Add a tool that converts temperatures between Celsius and Fahrenheit.
```

A call does these steps:

1. Copy the authored files of the tree to `.bonnie/selfmod/<call>/tree`.
2. Run a subagent with BONNIE's sandbox tools in that copy. The subagent edits the copy, not the tree.
3. Generate the wiring and build the tree with the changes, with `go build -overlay` in the tree's own directory. If the build fails, the subagent gets the compiler output and tries again, up to `MaxAttempts` times.
4. Write the changed files to the tree, one atomic rename for each file, and return a summary and a diff.

The dev loop then sees the change, rebuilds, and restarts the agent. The conversation stays in the journal. **The next message on the same run is answered by the new build**, with the new tools. A tool that the change adds is not available in the turn that made the change.

Review the change as you review any source change: `git diff`. Nothing is committed.

### What the subagent can change

| Path | Changed by `selfmod` |
| --- | --- |
| `instructions.md` | Yes |
| `*.go` at the root, such as `main.go` | Yes |
| `go.mod`, `go.sum` | Yes, but the subagent cannot download a new module |
| `tools/**`, `skills/**`, `context/**` | Yes |
| `bonnie_gen.go` | No: the dev loop regenerates it |
| Hidden files and directories (`.env`, `.bonnie`, `.git`) | No |
| Other paths, such as `README.md` | No |

The tool result names a file that the subagent wrote outside this layout. Such a file is not applied. `selfmod.Authored` is the exact rule.

### Conflicts

The tool records a hash of each file before the subagent starts. If you change one of the files that the subagent changed while it works, the tool writes nothing and reports a conflict. Ask again to start from the current tree.

### Choose the model, or turn the tool off

The subagent uses the agent's model by default. To change it, pass `selfmod.Local` in `main.go`. It replaces the default that `bonnie dev` installs:

```go
package main

import (
    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/selfmod"
)

func main() {
    bonnie.New(
        selfmod.Local(selfmod.Config{
            Model:       "anthropic/claude-sonnet-4-5",
            MaxAttempts: 5,
        }),
    ).Serve()
}
```

You can also ask the agent to make this change to `main.go`.

| `selfmod.Config` field | Default | Meaning |
| --- | --- | --- |
| `Model` | the agent's model | The subagent's model, as `provider/name` |
| `Timeout` | 20 minutes | Limit for one subagent attempt |
| `MaxAttempts` | 3 | Attempts to get a change that builds |
| `Authorize` | none | Optional for `Local`; see [Authorize the caller](#authorize-the-caller) |
| `Workspace` | Landlock | The sandbox of the subagent; see [The subagent's sandbox](#the-subagents-sandbox) |
| `Disabled` | `false` | Remove the tool |

To turn off the default for one session, start the dev loop with `bonnie dev --no-self-modify`. That flag does not remove a `selfmod.Local` that `main.go` passes. To remove that too, set `Disabled: true`.

### Why production builds do not have it

`bonnie dev` adds the default with `go build -overlay`. It writes no file to the tree, and `bonnie build` does not use the overlay. The tool also reads the tree root from `BONNIE_DEV_ROOT`, which only `bonnie dev` sets on its child. A binary from `bonnie build` has no such variable, so it has no `modify_agent` tool, also when `main.go` passes `selfmod.Local`.

A tree that pins a BONNIE release older than the `selfmod` package builds without the tool. The dev loop says so once.

## Propose changes from a deployed agent

`selfmod.Remote` adds `propose_agent_change`. Mount it beside the default. The two tools use different names and do not replace each other:

```go
package main

import (
    "context"

    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/selfmod"
)

func main() {
    bonnie.New(
        bonnie.WithHTTPAuthenticator(verifyBearer),
        selfmod.Remote(selfmod.RemoteConfig{
            Config: selfmod.Config{
                Authorize: func(_ context.Context, c selfmod.Caller) (bool, error) {
                    return c.Origin.Channel == "http" &&
                        c.Principal != nil && c.Principal.ID == "release-bot", nil
                },
            },
            Repository:  "github.com/acme/agents",
            Directory:   "apps/support",
            Branch:      "main",
            Credentials: selfmod.Credentials{PAT: true},
        }),
    ).Serve()
}
```

A call does these steps:

1. Clone `Branch` of `Repository` into `.bonnie/selfmod-remote/<call>/repo`, with depth 1.
2. Let a subagent change a staged copy of the tree under `Directory`, as in development.
3. Build the change when the host has a `go` command. A deployed binary usually runs on a host without Go. Then the change is not built, and the pull request says so. The repository's CI must build it.
4. Commit the change on its own branch, `bonnie/self-modification-<id>`, and push it.
5. Open a **draft** pull request against `Branch`, and return its URL.

**It never changes the running deployment.** A change takes effect only after a human reviews, merges, and deploys it. The tool is never offered in a process that `bonnie dev` started.

| `selfmod.RemoteConfig` field | Required | Meaning |
| --- | --- | --- |
| `Config` | `Authorize` | Subagent settings, as for `Local` |
| `Repository` | Yes | `github.com/owner/repo` |
| `Directory` | No | The tree's directory in the repository; empty or `.` is the root |
| `Branch` | Yes | The branch to clone and to target |
| `Credentials` | Yes | `{PAT: true}` or `{Resolve: func}` |
| `WorkDir` | No | Clones and staged changes; default `.bonnie/selfmod-remote` |
| `APIURL` | No | GitHub REST API root; default `https://api.github.com` |

A configuration error, such as a missing `Authorize`, stops each run that would offer the tool. It cannot go unnoticed.

### Credentials

`Credentials{PAT: true}` reads a token from `BONNIE_SELF_MODIFICATION_GITHUB_TOKEN`. The token needs contents and pull request write permission on the repository. `Credentials{Resolve: fn}` asks `fn` for a token for each capability: `selfmod.CapabilityRead` for the clone, and `selfmod.CapabilityWrite` for the push and the pull request. Use it for a GitHub App installation token.

The BONNIE process is the only user of the token. Git gets it as an HTTP header in the environment of the git process. It is not on the command line and not in `.git/config`. The subagent's sandbox holds only a copy of the authored files, without `.git`. No command that the subagent runs can read the token.

## Authorize the caller

`Authorize` gets a `selfmod.Caller` and returns whether that caller may use the tool. The tool is hidden from the model when it returns `false` or an error. BONNIE runs it on each Start or Resume of a run.

| `selfmod.Caller` field | Meaning |
| --- | --- |
| `RunID` | The run that would call the tool |
| `Origin` | The channel and surface of the conversation, such as `http` or `slack` |
| `Principal` | The identity that sent the run's most recent message, or `nil` |

`Principal` comes from `RunScope.Caller`, which reads the identity that the channel layer records for each message. Most channels record a verified identity only with an authenticator. Without one, the HTTP channel records the identity in the request body, which any caller can set. **An authorizer that accepts a nil principal accepts each caller that can reach the agent.** See [HTTP](/channels/http) for `WithHTTPAuthenticator`.

`Authorize` is optional for `Local`. A dev server is local, but channels such as Slack or GitHub can also be mounted under `bonnie dev`, and their messages can ask the agent to change itself. Set `Authorize` when the dev server takes messages from other people.

## The subagent's sandbox

The subagent works with BONNIE's sandbox tools (`shell`, `read_file`, `write_file`, `edit`, `grep`, and others) in the staged copy. The default sandbox is Landlock, with the staged copy as its shared work directory. On a host without Landlock, set `Workspace`. Only a provider that can use one exact host directory works:

```go
selfmod.Config{
    Workspace: func(_ context.Context, dir string) (sandbox.Provider, error) {
        p := sandbox.Local() // no isolation: development only
        return p, p.UseSharedDirectory(dir)
    },
}
```

## Interruptions and repeats

Both tools are replay-safe (see [Runtime](/reference/runtime)). Each call keeps its durable state in its work directory. A call that a crash or a restart cut off continues from that state:

- During the subagent's work, a repeat starts a new subagent attempt on the staged copy, with the edits that are already there.
- During the write to the tree, a repeat writes the files that are not yet written. A file that already has its new content is left alone.
- After the write, or after the pull request opened, a repeat returns the first result. It does not run the subagent again or open a second pull request.

The subagent itself is a `Kit.Subagent` call, not a durable BONNIE run. An interrupted attempt is repeated, not resumed.

## Limits

- **Go modules.** The subagent cannot download a module. A change that needs a new dependency fails to build. Add the dependency yourself, then ask again.
- **One tree at a time.** Calls on one tree run one after the other.
- **Not built without Go.** A remote proposal from a host without a `go` command is not built before it is proposed.
- **The subagent's output is not reviewed for you.** Read the diff before you commit a local change or merge a pull request. A change to `main.go` can change the agent's model, channels, and sandbox.

## What to read next

- [Agent Trees](/guides/agent-trees): the layout that the subagent edits.
- [Tools and Skills](/guides/tools-and-skills): the tool and skill contract.
- [CLI](/reference/cli#bonnie-dev-dir): `bonnie dev` flags.
- [Options](/reference/options): `WithNamedToolFactory` and `RegisterDefaults`.
