---
title: Options
description: All BONNIE root options, their defaults, validation rules, and managed callback contracts.
---

# Options

Import `github.com/mark3labs/bonnie`. Pass `bonnie.Option` values to `bonnie.New(opts...)` or `Agent.Configure(opts...)` before resource setup.

`New` returns `*bonnie.Agent` without opening files, checking backends, or binding a listener. Invalid combinations fail when execution starts. `Agent.Serve()` owns flags, signals, help, and process exit. `Agent.Run(ctx)` returns an error and does not read process flags or install signal handlers.

Configuration is code, not a manifest. The tree provides instructions, skills, compiled tools, and seed files. See [Agent Trees](/guides/agent-trees) and [CLI](/reference/cli).

## Defaults

| Constant | Value | Purpose |
| --- | --- | --- |
| `DefaultAddr` | `:8080` | HTTP listen address |
| `DefaultJournal` | `.bonnie` | Journal directory |
| `DefaultInstructions` | `instructions.md` | System prompt file |
| `DefaultSkills` | `skills` | Skill directory |
| `DefaultContextFiles` | `context` | Files copied into each new run |

The default backend is Landlock. Human-input tools are enabled. Shutdown timeout is 30 seconds. Activity logging and automatic working-file cleanup are disabled. The schedule clock is enabled unless disabled explicitly. Kit selects its default model when no model is given.

Most scalar options replace an earlier value. Additive options and options with stricter validation are identified below.

## Agent and tree data

| Exact API | Behavior |
| --- | --- |
| `WithName(name string)` | Reports the agent name through `GET /bonnie/v1/info`. It does not select a run or model. |
| `WithModel(model string)` | Selects a Kit model with a `provider/name` string. |
| `WithSystemPrompt(prompt string)` | A non-empty direct prompt takes precedence over the instructions file. |
| `WithInstructions(path string)` | Reads another prompt file. An empty path disables the instructions file. |
| `WithSkills(dir string)` | Reads the complete tree skill set from this directory. An empty directory value disables tree skills and automatic discovery; explicit Kit skills remain. |
| `WithContextFiles(dir string)` | Selects seed files copied into each new run. An empty directory value disables copying, not sandboxing. |
| `WithTools(tools ...kit.Tool)` | Adds tools beside generated tree tools and the managed standard tools. Repeated calls append. |
| `WithoutHumanInput()` | Omits BONNIE's `ask_human` and `request_approval`. Custom tools can still suspend; response text can still ask questions. Sandbox permissions do not change. |
| `WithKit(opts ...kit.Option)` | Appends public Kit options for settings BONNIE does not name. BONNIE still owns its durable session and managed tool boundary. |

A skill can be a `.md` or `.txt` file with YAML frontmatter, or a subdirectory with `SKILL.md`. Kit adds the skill name and description to the prompt. The body arrives when the model calls `activate_skill`. Bundled scripts, references, and assets remain on the host. Put required instructions in the body and files the model must open in `context/`.

Generated `bonnie_gen.go` calls `Register(Tree)` from `init`. `Tree` holds `Tools`, embedded `Instructions`, `Skills`, and `ContextFiles`. Disk data takes precedence when present; embedded data is the fallback for a deployed binary. `Registered()` returns that tree. Call it from `main` or later, not from a package-level variable initialized before generated `init` runs.

Custom Go tool callbacks run on the host. Registration does not confine them to the sandbox. See [Tools and Skills](/guides/tools-and-skills).

## Sandbox and working files

| Exact API | Behavior |
| --- | --- |
| `WithSandbox(p sandbox.Provider)` | Selects one backend. Without this option BONNIE still uses Landlock. `Serve --sandbox` can select only this provider. |
| `WithSandboxes(providers ...sandbox.Provider)` | Declares the permitted backends for operator selection. The first is the default and is used by `Run`. The selected configured instance is retained. |
| `WithSandboxDownload(enabled bool)` | Controls automatic installation of the tracked Microsandbox release at startup. Enabled by default, only when Microsandbox is selected and no `msb` exists in PATH or journal storage. False still permits an existing installation. |
| `WithSandboxEnv(env map[string]string)` | Injects variables into each sandbox command. Repeated calls merge; later keys win. Injected values override same-name per-command variables. |
| `WithNetwork(p sandbox.NetworkPolicy)` | Requires the selected provider to enforce the policy. Unsupported policies fail startup rather than being ignored. |
| `WithSharedDirectory(dir string)` | Uses one host directory for all runs. Supports Landlock and Local only. Landlock retains filesystem confinement; Local provides none. |
| `WithRunSandboxCleanup(policy SandboxCleanupPolicy)` | Enables retention-based deletion of run-owned files. The journal and channel addresses remain. |

`WithSandbox` and `WithSandboxes` cannot be combined. `WithSandboxes` can be called only once. Its list must be non-empty, with non-nil providers and unique non-empty `Provider.Name()` values. Only the selected provider is checked for availability. There is no fallback if it is unavailable.

`WithSharedDirectory` cannot be combined with an explicit `WithContextFiles`, including an empty seed path. Overlapping opens through one shared-directory provider are refused. Separate processes and provider instances are not coordinated. Run pruning never removes the shared directory.

`SandboxCleanupPolicy` aliases `runtime.SandboxCleanupPolicy`. It has `CompletedAfter`, `FailedAfter`, `CancelledAfter`, and `RetiredAfter`, all `time.Duration`. Zero retains files for that state; a positive value permits deletion after the latest terminal checkpoint; negative values fail validation. Cleanup runs at startup and once per minute, with a 30-second timeout per deletion. Errors are logged and retried. The provider must implement `sandbox.RunDeleter`. Cleanup cannot be combined with `WithSharedDirectory` or `WithAgentFactory`.

A later turn on a completed run can find an empty directory after cleanup. Publish output before completion. Waiting runs keep their files. One process must own execution and cleanup.

Injected credentials are not encrypted or hidden from sandbox commands. A command can print them. Landlock does not enforce a network policy. See [Sandboxes](/guides/sandboxes) for backend limits.

## Managed setup and completion

| Exact API | Behavior |
| --- | --- |
| `WithKitSetup(setup KitSetup)` | Appends setup callbacks in registration order. Runs after BONNIE installs its session and hooks, before the first prompt, on each Start or Resume. |
| `WithCompletionHook(policy CompletionPolicy)` | Installs one policy that checks a normal model response before BONNIE publishes a final outcome. |

`KitSetup` has signature `func(context.Context, *kit.Kit, RunScope) error`. `RunScope` provides `RunID` and `Exec func(context.Context, sandbox.Command) (*sandbox.Result, error)`. `Exec` opens the sandbox lazily and shares the tools' handle. BONNIE owns cleanup. Do not retain `Exec` after execution ends. Callback code itself runs on the host.

`CompletionPolicy` has:

- `NewHook CompletionHookFactory`, with signature `func(context.Context, RunScope) (CompletionHook, error)`.
- `MaxContinuations int`, the maximum additional model turns. Zero permits no additional turns.

`CompletionHook` accepts a `CompletionCandidate` and returns `CompletionFeedback` and an error. The candidate contains `Response` and `ContinuationsUsed`. Empty `ContinueWith` accepts it; non-empty `ContinueWith` requests another model prompt. Suspension and recovery do not reset the saved budget. A request beyond the limit fails with `ErrContinuationLimit`; test it with `errors.Is`.

The hook is skipped on suspension and model failure. Hook errors fail the run. A second policy, nil factory, negative limit, nil setup callback, or factory returning a nil hook is invalid. Setup and completion factory errors prevent execution. Closure state is not durable. Interrupted callbacks can repeat, so external effects must be safe to retry. See [Runtime](/reference/runtime) for recovery limits.

## Custom agent factory

`WithAgentFactory(f runtime.AgentFactory)` replaces the managed model agent. The factory receives the restored session and builds a `runtime.Agent` for each execution.

It cannot be combined with `WithModel`, `WithSystemPrompt`, `WithSandbox`, `WithSandboxes`, `WithSandboxEnv`, `WithNetwork`, `WithTools`, `WithKit`, `WithoutHumanInput`, `WithKitSetup`, `WithCompletionHook`, or `WithRunSandboxCleanup`. BONNIE refuses agent settings that it would otherwise ignore.

Tree instructions, skills, and generated tools do not automatically reach the custom factory. Use `Registered()` if the host needs them. BONNIE still owns journal storage, working files, channels, and shutdown. The host owns its agent's isolation.

## Command configuration

`WithCommand(fn func(*cobra.Command, *Agent))` appends a root command configuration callback. Callbacks run in registration order after BONNIE adds its built-in and host Go flags, but before argument parsing. They run only in `Serve`, not in `Run`. Add custom flags, subcommands, and Cobra hooks here.

Use `Agent.Configure(opts ...Option)` from a pre-run hook to apply parsed values. It keeps the option replacement, additive, and validation rules. It opens no resources. Do not call it concurrently or to change a live agent. Explicit built-in serving flags override matching options before `Run` starts.

Use `cmd.Context()` for blocking operations. A context set in a pre-run hook reaches `Agent.Run`. Setting a hook replaces the existing hook unless you explicitly chain it. Help runs registration callbacks, but not pre-run hooks.

See [Agent Commands](/guides/agent-commands) for a complete example and scoped journal and runner access.

## HTTP and process hosting

| Exact API | Behavior |
| --- | --- |
| `WithAddr(addr string)` | Replaces the listen address when non-empty. |
| `WithJournal(dir string)` | Replaces the journal directory when non-empty. Root hosting uses SQLite. |
| `WithListener(ln net.Listener)` | Serves on an already-bound listener instead of binding the configured address. Useful for tests with port zero. |
| `WithShutdownTimeout(d time.Duration)` | Replaces the 30-second shutdown timeout when positive. Shutdown phases use this bound. |
| `WithHTTPAuthenticator(fn bonniehttp.Authenticator)` | Verifies callers of the framework HTTP API and supplies their principal. Health remains unauthenticated. |
| `Quiet()` | Suppresses the startup banner, not authentication or isolation checks. |
| `WithActivityLogger(logger runtime.ActivityLogger)` | Enables live activity logging across channels. Nil disables it. Replay is not logged again. |

Without an HTTP authenticator, the API verifies no caller. Use an authenticator or a trusted authentication layer before exposing it. An authentication callback is not a sandbox policy. See [HTTP](/channels/http), [Deployment](/guides/deployment), and [Events](/reference/events).

`NewActivityLogger(nil)` creates a timestamped Info logger on stdout. You can supply a `*log.Logger` or implement `runtime.ActivityLogger` yourself.

## Channels

`WithChannel(f ChannelFunc)` appends a transport beside HTTP. `ChannelFunc` is `func(*runtime.Runner) (Channel, error)`. `Channel` combines `channel.Channel` and `channel.Inbound`. Construction occurs once at startup; an error stops startup. Optional lifecycle channels start after listener binding and shut down before journal close, including startup failures.

| Exact API | Environment fallback for empty fields |
| --- | --- |
| `WithSlack(cfg slack.Config)` | `SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET`, optional `SLACK_API_URL` |
| `WithDiscord(cfg discord.Config)` | `DISCORD_BOT_TOKEN`, `DISCORD_PUBLIC_KEY`, optional `DISCORD_API_URL` |
| `WithTelegram(cfg telegram.Config)` | `TELEGRAM_BOT_TOKEN`, `TELEGRAM_WEBHOOK_SECRET`, optional `TELEGRAM_API_URL` |
| `WithGitHub(cfg github.Config)` | `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`, optional `GITHUB_INSTALLATION_ID` and `GITHUB_API_URL` |
| `WithNATS(cfg natschannel.Config)` | `NATS_URL`, `NATS_NKEY_SEED`, `NATS_TOKEN`, `NATS_USERNAME`, `NATS_PASSWORD` |

Explicit non-empty fields win. Missing required platform verification credentials fail startup. GitHub webhook events carry installation identity; hand-offs need a configured installation ID. A non-numeric `GITHUB_INSTALLATION_ID` fails validation. The GitHub bot name is a code setting, not an environment fallback.

For NATS, select only one authentication method, including environment fallbacks. NKey seeds are values, not file paths. A supplied `Conn` disables connection environment fallback; the caller owns that connection. `Stream` or `RootSubject` enables JetStream. Core NATS does not retain tasks or retry result delivery. See [Channels](/channels/overview) for transport selection and delivery limits.

## Schedules

| Exact API | Behavior |
| --- | --- |
| `WithSchedule(definitions ...schedule.Definition)` | Appends durable time-based dispatch definitions. |
| `WithScheduleClock(enabled bool)` | Enables or disables cron evaluation. Production default is true; `bonnie dev` passes false unless enabled. |
| `WithScheduleTriggerAuthorizer(fn func(*http.Request) error)` | Mounts and authorizes external/manual schedule triggers. This is separate from the normal HTTP authenticator. |

Definitions and revisions are code. Prepared dispatches and delivery progress are durable, but delivery is at least once. The scheduler takes a journal-directory lock; this is not a general distributed run lock. See [Scheduling](/guides/scheduling).
