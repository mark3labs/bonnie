# Agent commands and flags

`bonnie build` compiles the agent's Go module. Commands and flags registered
in `main.go` are part of that binary. No CLI manifest is needed.

## Built-in operator commands

`Agent.Serve` includes `chat`, `runs list/show/inspect`, `schedules
list/show/history/trigger`, and `version`. With no subcommand, it serves the
agent. The explicit `serve` command has the same action and framework flags.
These commands also work in agents compiled with `go build`.

`runs` reads the configured local journal and accepts `--journal`. `chat`
and `schedules` connect to a running HTTP server at the configured address;
they accept `--token` for bearer authentication. They do not start a server.
Help and version do not open the journal or require a model.

Disable commands with `WithChatCommand(false)`, `WithRunsCommand(false)`,
`WithSchedulesCommand(false)`, and `WithVersionCommand(false)`. Disabling a
command does not restrict HTTP routes or file access, or guarantee a smaller
binary. Disable a built-in before adding a custom command with the same name.

## Register commands and apply parsed flags

`WithCommand` gives a callback the root Cobra command and the configured
agent. Callbacks run in option order, after BONNIE registers its flags and
before argument parsing. They run only in `Serve`, not in `Run`.

This complete `main.go` adds a journal flag and a journal inspection command:

```go
package main

import (
    "context"
    "fmt"

    "github.com/spf13/cobra"

    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/runtime"
)

func main() {
    bonnie.New(
        bonnie.WithRunsCommand(false), // Replace the built-in runs command.
        bonnie.WithCommand(func(root *cobra.Command, agent *bonnie.Agent) {
            var journal string
            root.PersistentFlags().StringVar(&journal, "journal", ".bonnie", "journal directory")
            root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
                agent.Configure(bonnie.WithJournal(journal))
                return nil
            }
            root.AddCommand(&cobra.Command{
                Use: "runs", Short: "List durable run IDs", Args: cobra.NoArgs,
                RunE: func(cmd *cobra.Command, args []string) error {
                    return agent.WithJournal(cmd.Context(), func(ctx context.Context, journal runtime.Journal) error {
                        ids, err := journal.Runs(ctx, "")
                        if err != nil {
                            return err
                        }
                        for _, id := range ids {
                            if _, err := fmt.Fprintln(cmd.OutOrStdout(), id); err != nil {
                                return err
                            }
                        }
                        return nil
                    })
                },
            })
        }),
    ).Serve()
}
```

After `bonnie build`:

```sh
./my-agent --journal ./state --addr :9090
./my-agent --journal ./state runs
./my-agent --help
./my-agent runs --help
```

The root command still serves by default. Keep that action and the `-addr`
flag to retain `bonnie dev` support. BONNIE still owns help, errors, signal
handling, and process exit.

- Use `root.Flags()` for serving-only flags.
- Use `root.PersistentFlags()` for flags inherited by subcommands.
- Use `PreRunE` for serving-only validation or configuration.
- Use `PersistentPreRunE` for shared validation or configuration. Cobra's
  normal hook rules apply: a child hook can replace an inherited hook.
- Do not call `flag.Parse()` when using `Serve`. Let Cobra parse all flags.
- Prefer `--name` syntax. Root flags also accept single-dash long names,
  including BONNIE's `-addr`. Subcommand-local flags use normal Cobra syntax.

Help runs registration callbacks, but not pre-run hooks. Keep registration
free of resource setup. Multiple callbacks share the command: setting a hook
replaces the previous hook unless the callback explicitly chains it.

## Configuration and context

Registration runs before parsing. Use parsed values in pre-run hooks and
subcommand actions, not in registration callbacks.

`Agent.Configure(opts...)` applies the same options as `bonnie.New`. Options
run in order; replacement and additive rules are unchanged. It does not read
files or open resources. Call it before `Run`, `WithJournal`, or `WithRuntime`,
including from a command's pre-run hook. Do not use it concurrently or to
change a live runtime. Explicit built-in serving flags override the matching
configuration before serving starts. An omitted flag does not override a
configured value.

Use `cmd.Context()` for blocking operations. A pre-run hook can use
`cmd.SetContext` to attach application data. BONNIE passes that context to
`Agent.Run`. Context data does not automatically change agent configuration
or tool behavior; application code must read it.

## Scoped resource access

`Agent` is configuration, not a live runner. Custom commands can open only
the resources they need:

| API | Resources and behavior |
|---|---|
| `agent.WithJournal(ctx, fn)` | Opens the configured SQLite journal and closes it after the callback. No model, factory, dotenv, prompt, sandbox check, or server setup. |
| `agent.WithRuntime(ctx, fn)` | Uses the same file preparation, agent factory, and runner options as `Run`. Exposes `Runtime.Journal()` and `Runtime.Runner()`. No server, channel, schedule service, scheduler, or background cleanup starts. |

`WithJournal` is suitable for run lists and history inspection. It opens the
normal journal, which can create its directory and apply schema setup; it is
not a read-only SQLite connection.

`WithRuntime` is suitable for commands that start or resume runs through the
runner's public API. It loads dotenv, resolves agent files, seeds context
files, and checks the configured sandbox as serving does. Models are created
by the factory when a run needs one.

Both APIs return callback errors, context cancellation, and journal close
errors together. A nil callback returns an error without opening resources.
Do not close the supplied journal. Do not retain resource references after
the callback returns. Wait for all operations and goroutines before returning.
They do not intercept process signals when called outside `Serve`; the host
must supply a suitable context.

If a command uses `Runner.RunScheduler`, it must start, cancel, and wait for
that scheduler inside the callback. Only one scheduler may own a journal at
a time. Do not run an operations command that drives runs against a journal
already owned by a serving process. Use the HTTP client for live-server
operations instead. See [DURABLE_WORK.md](DURABLE_WORK.md) for ownership rules.
