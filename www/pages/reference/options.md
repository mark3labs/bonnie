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
| `DevRootEnv` | `BONNIE_DEV_ROOT` | Environment variable that `bonnie dev` sets on its child to the tree root |

The default backend is Landlock. Human-input tools are enabled. Shutdown timeout is 30 seconds. Activity logging and automatic working-file cleanup are disabled. The schedule clock is enabled unless disabled explicitly. Kit selects its default model when no model is given.

`RegisterDefaults(opts ...Option)` adds options that `New` applies before its own arguments. Generated code calls it from `init`; `bonnie dev` uses it to add the default self-modification tool. An option in `main.go` applies later, so it replaces a default in the same slot. A default tool factory is dropped, not refused, beside `WithAgentFactory`.

Most scalar options replace an earlier value. Additive options and options with stricter validation are identified below.

## Agent and tree data

| Exact API | Behavior |
| --- | --- |
| `WithName(name string)` | Reports the agent name through `GET /bonnie/v1/info`. It does not select a run or model. |
| `WithModel(model string)` | Selects a Kit model with a `provider/name` string. |
| `WithSystemPrompt(prompt string)` | A non-empty direct prompt takes precedence over the instructions file. |
| `WithInstructions(path string)` | Reads another prompt file. An empty path disables the instructions file. |
| `WithSkills(dir string)` | Reads the complete tree skill set from this directory. An empty directory value disables tree skills and Kit automatic discovery; explicit Kit skills and opt-in workspace discovery remain. |
| `WithWorkspaceSkills(policy WorkspaceSkills)` | Enables bounded, recursive sandbox repository discovery before model requests. Repeated options replace the copied policy. Off by default; cannot use `WithAgentFactory`. |
| `WithContextFiles(dir string)` | Selects seed files copied into each new run. An empty directory value disables copying, not sandboxing. |
| `WithTools(tools ...kit.Tool)` | Adds tools beside generated tree tools and the managed standard tools. Repeated calls append. |
| `WithoutHumanInput()` | Omits BONNIE's `ask_human` and `request_approval`. Custom tools can still suspend; response text can still ask questions. Sandbox permissions do not change. |
| `WithKit(opts ...kit.Option)` | Appends public Kit options for settings BONNIE does not name. BONNIE still owns its durable session and managed tool boundary. |

A skill can be a `.md` or `.txt` file with YAML frontmatter, or a subdirectory with `SKILL.md`. Kit adds the skill name and description to the prompt. The body arrives when the model calls `activate_skill`. Bundled scripts, references, and assets remain on the host. Put required instructions in the body and files the model must open in `context/`.

Generated `bonnie_gen.go` calls `Register(Tree)` from `init`. `Tree` holds static `Tools`, a scoped `ToolFactory`, embedded `Instructions`, `Skills`, and `ContextFiles`. Zero-argument tree tools are constructed during `init`; scoped tree tools are constructed for each Start or Resume. Disk data takes precedence when present; embedded data is the fallback for a deployed binary. `Registered()` returns that tree. Call it from `main` or later, not from a package-level variable initialized before generated `init` runs.

Custom Go tool callbacks run on the host. Registration does not confine them to the sandbox. See [Tools and Skills](/guides/tools-and-skills).

`WorkspaceSkills` has `Exclude []string`, `MaxDepth int`, `MaxSkills int`,
`MaxSkillBytes int64`, and `MaxMetadataBytes int64`. Zero defaults are 12,
100, 128 KiB and 32 KiB. Negative limits and invalid directory exclusion globs
fail startup. Only `.agents/skills/<bundle>/SKILL.md` and
`.kit/skills/<bundle>/SKILL.md` qualify. Tree skills remain separate.
Discovery opens sandbox compute at startup and permits untrusted repository
metadata in context; activation is not authorization. Saved bodies are durable,
but bundled assets are live sandbox files. See [Workspace skills](/guides/tools-and-skills#workspace-skills)
for exact ignore rules, defensive ceilings, diagnostics, guest helper packaging
and resume timing.

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
| `WithToolFactory(factory ToolFactory)` | Appends factories that build tools with the current Kit instance and run scope. Runs after discovered factories and before setup callbacks on each Start or Resume. |
| `WithNamedToolFactory(name string, factory ToolFactory)` | As `WithToolFactory`, but a later option with the same name replaces the factory in its place. An empty name is the same as `WithToolFactory`. |
| `WithKitSetup(setup KitSetup)` | Appends setup callbacks in registration order. Runs after BONNIE installs its session and hooks, before the first prompt, on each Start or Resume. |
| `WithCompletionHook(policy CompletionPolicy)` | Installs one policy that checks a normal model response before BONNIE publishes a final outcome. |

`ToolFactory` has signature `func(context.Context, ToolScope) ([]kit.Tool, error)`. `ToolScope` embeds `RunScope` and adds `Kit *kit.Kit`. Factories run before the first prompt, after BONNIE installs its session and checkpoint hooks. A nil factory, a returned error, or a nil returned tool stops construction. A nil or empty tool slice is permitted. Closure state is local to the execution, not durable. Do not retain the scope after execution ends. Use `Kit.Subagent`, not `Kit.Prompt`, for calls from an active tool. Child calls are not durable BONNIE runs. See [Tools and Skills](/guides/tools-and-skills#call-a-model-from-a-tool) for examples.

`KitSetup` has signature `func(context.Context, *kit.Kit, RunScope) error`. `RunScope` provides `RunID string`, `Session *runtime.Session`, `Exec func(context.Context, sandbox.Command) (*sandbox.Result, error)`, and `Decide func(context.Context, DecisionRequest) (*kit.DecisionResult, error)`. `Decide` is nil unless a decision model is configured; see [Decision models](#decision-models). `Session.Caller()` returns the `*runtime.Caller` that sent the current turn's input, or nil. The channel passes it with the input, so a later message cannot change it. Its `Verified` field is true only when the channel proved the identity; an identity in the body of an unauthenticated HTTP request is not verified. `Exec` opens the sandbox lazily and shares the tools' handle. BONNIE owns cleanup. Do not retain `Exec` after execution ends. Callback code itself runs on the host.

`CompletionPolicy` has:

- `NewHook CompletionHookFactory`, with signature `func(context.Context, RunScope) (CompletionHook, error)`.
- `MaxContinuations int`, the maximum additional model turns. Zero permits no additional turns.

`CompletionHook` accepts a `CompletionCandidate` and returns `CompletionFeedback` and an error. The candidate contains `Response` and `ContinuationsUsed`. Empty `ContinueWith` accepts it; non-empty `ContinueWith` requests another model prompt. Suspension and recovery do not reset the saved budget. A request beyond the limit fails with `ErrContinuationLimit`; test it with `errors.Is`.

The hook is skipped on suspension and model failure. Hook errors fail the run. A second policy, nil factory, negative limit, nil setup callback, or factory returning a nil hook is invalid. Setup and completion factory errors prevent execution. Closure state is not durable. Interrupted callbacks can repeat, so external effects must be safe to retry. See [Runtime](/reference/runtime) for recovery limits.

## Decision models

| Exact API | Behavior |
| --- | --- |
| `WithDecisionModel(model string)` | Sets the decision model, in `provider/model` format. `RunScope.Decide` and `Agent.Decide` use it when a request names no model. `Agent.Run` refuses a model that Kit cannot call as a decision model; it does not check credentials. |
| `WithDecider(d Decider)` | Replaces the provider call with `d`, for tests or a model Kit cannot call. Calls in a run are still journalled. The model is not checked against the catalog. |
| `WithDecisionTool()` | Gives the model the `decide` tool. Needs `WithDecisionModel` or `WithDecider`. Cannot be combined with `WithAgentFactory`. |

`Decider` has signature `func(context.Context, kit.DecisionRequest) (*kit.DecisionResult, error)`. `DecisionRequest` has `Model`, `State`, `Questions`, and `ProviderOptions`, as `kit.DecisionRequest`, and `Key string`.

`RunScope.Decide` writes a `decision` journal record for each call that reaches the decision model or the `Decider`, also for a failed call. A call that does not reach it writes no record: a key answered from the journal, a key used again with different input (`runtime.ErrDecisionKeyReused`), or input that cannot be encoded. A request with a `Key` is idempotent in the run: a successful answer for the same key and input (model, state, questions, and provider options) is returned from the journal, also after a restart, and the model is not asked again. A key used again with different input returns `runtime.ErrDecisionKeyReused`. If the record cannot be written, `Decide` returns an error and no result.

`Agent.Decide(ctx, req)` asks the model outside a run, for example in a schedule's `Prepare`. It writes no record and ignores `Key`. It uses Kit's default settings, not the transport from `WithKit`. It works beside `WithAgentFactory`.

A probability is not an authorization. Enforce access rules in host code. See [Decisions](/guides/decisions).

## Recovery policy

`WithReplaySafeTools(names ...string)` adds names to the permitted recovery set;
repeated calls form a union. The default is none. Both saved and current policy
must permit replay. Tools must enforce their own authorization and external
idempotency: recovery calls the implementation directly, not interactive Kit
approval hooks. Do not mark shell or payment tools safe by default. This managed
setup option cannot be combined with `WithAgentFactory`. See [Runtime](/reference/runtime).

## Custom agent factory

`WithAgentFactory(f runtime.AgentFactory)` replaces the managed model agent. The factory receives the restored session and builds a `runtime.Agent` for each execution.

It cannot be combined with `WithModel`, `WithSystemPrompt`, `WithSandbox`, `WithSandboxes`, `WithSandboxEnv`, `WithNetwork`, `WithTools`, `WithToolFactory`, `WithKit`, `WithoutHumanInput`, `WithKitSetup`, `WithCompletionHook`, `WithDecisionTool`, `WithWorkspaceSkills`, or `WithRunSandboxCleanup`. BONNIE refuses agent settings that it would otherwise ignore.

Tree instructions, skills, and generated tools do not automatically reach the custom factory. Use `Registered()` if the host needs them. BONNIE still owns journal storage, working files, channels, and shutdown. The host owns its agent's isolation.

## Command configuration

These scalar options replace earlier values. All five commands are enabled by
default in `Serve`; they do not affect `Run`.

| Exact API | Behavior |
| --- | --- |
| `WithUpdateCommand(enabled bool)` | Controls the default-enabled `update models [source]` group. Does not restrict filesystem access or shared cache reads. Disable before adding a custom `update` command with `WithCommand`. Later options win. |
| `WithAuthCommand(enabled bool)` | Controls provider `auth login/status/logout`. Does not prevent stored credential loading. |
| `WithChatCommand(enabled bool)` | Controls terminal `chat` against a running HTTP server. |
| `WithRunsCommand(enabled bool)` | Controls local `runs list/show/inspect`. Does not restrict journal access. |
| `WithSchedulesCommand(enabled bool)` | Controls HTTP schedule operator commands, not schedule routes. |
| `WithVersionCommand(enabled bool)` | Controls the explicit build-information `version` command. |

Disabling commands does not remove their dependencies, limit underlying API or
file access, or guarantee smaller binaries. Disable a built-in before registering
a replacement with the same name.

`WithCommand(fn func(*cobra.Command, *Agent))` appends a root command configuration callback. Callbacks run in registration order after BONNIE adds its built-in and host Go flags, but before argument parsing. They run only in `Serve`, not in `Run`. Add custom flags, subcommands, and Cobra hooks here.

Use `Agent.Configure(opts ...Option)` from a pre-run hook to apply parsed values. It keeps the option replacement, additive, and validation rules. It opens no resources. Do not call it concurrently or to change a live agent. Explicit built-in serving flags override matching options before `Run` starts.

Use `cmd.Context()` for blocking operations. A context set in a pre-run hook reaches `Agent.Run`. Setting a hook replaces the existing hook unless you explicitly chain it. Help runs registration callbacks, but not pre-run hooks.

See [Agent Commands](/guides/agent-commands) for a complete example and scoped journal and runner access.

## HTTP and process hosting

| Exact API | Behavior |
| --- | --- |
| `WithAddr(addr string)` | Replaces the listen address when non-empty. |
| `WithWebUI(enabled bool)` | Enables `/web` on the HTTP listener; false by default. Replaces the earlier setting; does not open a browser. |
| `WithHTTPListenerDisabled()` | Disables TCP API and schedule routes. Rejects web UI, injected listeners, and channels with TCP routes. See [NATS API](/channels/nats-api#host-and-client). |
| `WithJournal(dir string)` | Replaces the journal directory when non-empty. Root hosting uses SQLite. |
| `WithListener(ln net.Listener)` | Serves on an already-bound listener instead of binding the configured address. Useful for tests with port zero. |
| `WithShutdownTimeout(d time.Duration)` | Replaces the 30-second shutdown timeout when positive. Shutdown phases use this bound, including the wait for each interrupted turn to record its state. |
| `WithHTTPAuthenticator(fn bonniehttp.Authenticator)` | Verifies callers of the framework HTTP API and supplies their principal. Health remains unauthenticated. |
| `Quiet()` | Suppresses the startup banner, not authentication or isolation checks. |
| `WithActivityLogger(logger runtime.ActivityLogger)` | Enables live activity logging across channels. Nil disables it. Replay is not logged again. |
| `WithTracing(tp trace.TracerProvider, opts ...observe.Option)` | Records each turn as OpenTelemetry spans. Metadata only unless `observe.WithCapture` allows content. Nil disables it. See [Observability](/guides/observability). |

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
| `WithIroh(cfg irohchannel.Config)` | None; supply endpoint options and approved peer IDs in code |
| `WithNATSAPI(cfg natsapi.Config)` | None; appends an API channel with an explicit agent-specific Core NATS subject and required verifier. Broker authentication is not API authentication. See [NATS API](/channels/nats-api). |
| `WithNATS(cfg natschannel.Config)` | `NATS_URL`, `NATS_NKEY_SEED`, `NATS_TOKEN`, `NATS_USERNAME`, `NATS_PASSWORD` |

Explicit non-empty fields win. Missing required platform verification credentials fail startup. GitHub webhook events carry installation identity; hand-offs need a configured installation ID. A non-numeric `GITHUB_INSTALLATION_ID` fails validation. The GitHub bot name is a code setting, not an environment fallback.

`WithIroh` serves the HTTP-compatible API on Iroh streams, beside TCP HTTP.
An empty `Peers` list denies all Iroh requests, including health. Pairing grants
operator access to all runs; it does not authenticate TCP HTTP. See
[Iroh](/channels/iroh) for stable keys, tickets, and process-local pairing.

For NATS, select only one authentication method, including environment fallbacks. NKey seeds are values, not file paths. A supplied `Conn` disables connection environment fallback; the caller owns that connection. `Stream` or `RootSubject` enables JetStream. Core NATS does not retain tasks or retry result delivery. See [Channels](/channels/overview) for transport selection and delivery limits.

## Agent presence

`WithPresence(cfg PresenceConfig)` enables optional agent registration during `Agent.Run` or `Agent.Serve`. Without it, the host does not register. It adds no CLI flags or HTTP discovery routes.

| `PresenceConfig` field | Type | Default and contract |
| --- | --- | --- |
| `Registry` | `presence.Registry` | Required. Provides `Register(context.Context, presence.Record) error` and `Unregister(context.Context, presence.Identity) error`. The host does not close it. |
| `AgentID` | `string` | Required stable agent name. Must match any mounted channel's non-empty `channel.AgentIdentity()`. |
| `InstanceID` | `string` | Random 16-byte value encoded as hex when omitted. Distinguishes process instances; do not reuse it across simultaneous instances. |
| `Labels` | `map[string]string` | Optional discovery metadata. Copied when the option is set; not authorization claims. |
| `RefreshInterval` | `time.Duration` | Zero selects 10 seconds. Negative values fail. Must be less than the registry TTL when it implements `presence.TTLStore`. |
| `Endpoints` | `[]presence.Endpoint` | Omitted: collect from mounted `channel.PresenceProvider` implementations, including HTTP. Explicit entries replace the full list and must have non-empty addresses. Copied when the option is set. |

Endpoint fields are `Channel string`, `Address string`, and `Ready`, `Input`, `Delivery bool`. For overrides, the host sets `Ready` from its state. Provider endpoints retain their reported readiness while the host is ready; all endpoints become not ready while draining. Supply externally reachable URLs explicitly, without credentials.

Initial registration occurs after channel startup. Registration and refresh failures stop the host. Shutdown attempts a draining update and unregister with the bounded shutdown context. Presence is advisory: expiry does not prove execution stopped or permit automatic task reassignment.

See [Deployment](/guides/deployment#advertise-agent-presence) for a NATS KV registry example, discovery watches, ownership conflicts, and memory-store limits.

## Schedules

| Exact API | Behavior |
| --- | --- |
| `WithSchedule(definitions ...schedule.Definition)` | Appends durable time-based agent dispatch or host callback definitions. |
| `WithScheduleClock(enabled bool)` | Enables or disables cron evaluation. Production default is true; `bonnie dev` passes false unless enabled. |
| `WithScheduleTriggerAuthorizer(fn func(*http.Request) error)` | Mounts and authorizes external/manual schedule triggers. This is separate from the normal HTTP authenticator. |

Definitions and revisions are code. Each definition requires exactly one of `Prompt`, `Prepare`, or `Run`. `Run` has signature `func(context.Context, schedule.Fire) error` and starts no agent dispatches or channel deliveries. A nil error sets the occurrence to `Completed`; an error sets it to `Failed`. A saved failure is not retried. An empty `Prepare` result with no error sets it to `Skipped`.

`Prepare` and `Run` execute on the host, outside the agent sandbox. Both must respect context cancellation and be safe to repeat if their results were not saved. `Fire.ID` stays stable across retries; use it to prevent repeated external effects. BONNIE does not guarantee exactly-once execution.

Prepared dispatches and delivery progress are durable, but delivery is at least once. The scheduler takes a journal-directory lock; this is not a general distributed run lock. See [Scheduling](/guides/scheduling).
