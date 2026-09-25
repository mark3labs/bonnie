# Changelog

All notable changes to BONNIE are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.9.0] — 2026-09-25

**Security: `request_approval` now holds the action back until a person
answers, and a sandboxed agent no longer takes configuration from files Kit
finds on the host.** Upgrade from any earlier release.

### The claims

Still what BONNIE is for. All three hold as they did at `0.8.0`; this release
makes the second one mean what it says:

- **A run survives process death.** The conversation is journalled as it
  happens, so another process — after a crash, on another machine — resumes
  the run with the whole history, including which tools it already called, so
  a side effect is not repeated. A tool-calling step commits as one SQLite
  transaction: whole, or absent. Kit moves to v0.113.3 and the compile-time
  tripwire on `kit.SessionManager` held (20 methods). The four Kit seams are
  now also proven against a real `*kit.Kit` in the standard test run, not
  only behind the `integration` tag.
- **A run parks indefinitely.** A run waiting on a person holds no process and
  no compute — the sandbox opens lazily, so a parked run costs a row in a
  database. Exit the process and answer tomorrow. **New in this release:**
  the turn ends at the halting tool, so the model is not asked again, and
  cannot act, until the run resumes with the answer.
- **A run is reachable over HTTP.** `channel/http` mounts its routes under
  `/bonnie/v1`, with an NDJSON event stream that survives a reconnect and a
  restart; the Slack, Discord, Telegram, and GitHub adapters carry the same
  durable run into a thread, and each refuses to start without the
  credential that verifies its webhook.

The limits are under **Known limits** below, stated as plainly. A framework
that hides its limits gets deployed into situations it cannot handle.

### Security

- **`request_approval` now holds the action back until the operator
  answers.** Before Kit v0.113.3, Kit did not end the agent loop on
  `ToolOutput.Halt`, in any release BONNIE has used: it asked the model
  again, with every tool, in the same turn. The run still parked, so this was
  invisible from the outside, but a model that asked for approval and then
  went ahead ran the action before anyone approved it. Kit v0.113.3 ends the
  turn at a halting tool
  ([mark3labs/kit#147](https://github.com/mark3labs/kit/issues/147)), and
  BONNIE now requires it. This covers a host's own halting tool too. A tool
  called in the same step as the halting tool still runs.
- **A sandboxed agent no longer takes configuration from files Kit finds on
  the host.** `sandbox.Agent`, and thus `bonnie.New()`, turns off Kit's
  context files, named agent definitions and extensions. Kit read
  `AGENTS.md` from the sandbox's working directory, which on the default
  landlock backend the model can write, so a model could put instructions
  into its own next system prompt. Extensions from `.kit/extensions`,
  `~/.config/kit/extensions` and `/usr/share/kit/extensions` ran in the
  BONNIE process, outside the sandbox. A host that wants one back passes
  `WithKit(kit.WithContextFiles())`, `kit.WithAgents()` or
  `kit.WithExtensions()`. `~/.kit.yml` is still read.

### Added

- `internal/fakemodel`, a scripted model for tests that drive a real Kit
  through `kit.WithProvider`, with no network and no key. New tests use it to
  prove the four Kit seams and the sandbox's options against a real
  `*kit.Kit`; before, only the live tests behind the `integration` tag did.
  It reproduced both defects above, and guards against their return.

### Changed

- **Breaking (behaviour):** `sandbox.Agent`, and thus `bonnie.New()`, no
  longer loads Kit's context files (`AGENTS.md`), named agent definitions, or
  extensions. See *Security* for why. Nothing fails to compile: a host that
  relied on one of them loses it silently, and gets it back with
  `WithKit(kit.WithContextFiles())`, `kit.WithAgents()`, or
  `kit.WithExtensions()`. The tree's `instructions.md` and skills are
  unaffected.
- **Breaking (dependency):** BONNIE now requires Kit v0.113.3 or later. A
  module that pinned an older Kit is moved up by Go's minimum version
  selection. This is deliberate: an older Kit does not end the turn on
  `Halt`.
- **Test code may import `charm.land/fantasy`.** A `_test.go` file and
  `internal/fakemodel` may; shipped code still may not, and may not import
  `internal/fakemodel` either. depguard enforces both with per-file rules, and
  the `kit-boundary` extension follows. `fantasy` is now a direct requirement
  in `go.mod`; users get the same version as before.

- Dependencies updated. Kit moves to v0.113.3, the first release that ends
  the turn on `Halt`, and `charm.land/bubbletea/v2` to v2.0.10;
  `anthropic-sdk-go`, `mcp-go`, `gax-go` and the AWS and Google clients move
  transitively. `charm.land/fantasy` moves to v0.45.2, Kit's own version. The
  examples take the same set and stay pinned to bonnie v0.8.0.
  `kit.SessionManager` still has exactly 20 methods, and the CGO-free build
  still passes. From Kit v0.113.0, an agent that BONNIE builds no longer
  writes a default `~/.kit.yml` into the home directory of the host that
  serves it.

### Fixed

- **`task examples-pin` passes on its own pin.** It changed each example's
  `go.mod` and `go.sum` and then ran `task examples`, whose
  `git diff --exit-code -- examples/` failed on the pin itself, so the
  post-release step in `docs/RELEASE.md` could never pass. The task now
  stages the pin first, so the check sees only what `bonnie build` changed.

### Known limits

Stated as plainly as the claims, and taken from the current
[`README.md`](README.md#limits) rather than carried forward. Each was checked
against the code at this tag, including the Kit it now requires.

- **Linux only.** The floor is the Landlock LSM, so a release builds
  `linux/amd64` and `linux/arm64` and nothing else. macOS and Windows are not
  supported. A kernel older than 5.13, or one booted with Landlock disabled,
  has no default sandbox: BONNIE refuses to start rather than run a tool
  unconfined — use `--sandbox docker` there.
- **The default sandbox is containment, not isolation.** Landlock confines the
  filesystem and keeps host credentials away from a command. It does **not**
  confine the network, and it shares the host kernel.
- **Do not run BONNIE as a user in the `docker` group.** Landlock mediates
  opening a file, not connecting to a socket, so a tool call reaches
  `/var/run/docker.sock` whenever the process can — a full host escape. No
  path setting closes it. Use an unprivileged user, or microsandbox.
- **Docker is namespaces, not a kernel.** Use microsandbox for hostile code.
  microsandbox is verified on Linux with KVM; its network policy is fixed when
  the sandbox is made, and reattaching under a different one fails with
  `ErrPolicyMismatch`.
- **Sandbox egress is open** until a policy is set, and the default backend
  cannot set one — it refuses the policy instead of ignoring it.
- **A halt stops the turn, not the step.** **New in this release.** Kit runs
  the tool calls of one step together, so a tool the model calls in the
  **same** step as `request_approval` (or any halting tool) still runs
  before the run parks. Approval gates the next step, not a sibling call.
  Put the action that needs approval behind the answer, not beside the
  question.
- **A skill's bundled files stay on the host.** Kit v0.113.3 still names a
  skill's `scripts/`, `references/`, and `assets/` files in the activation
  text with a host path the sandbox does not have, so the model is told about
  a file it cannot open. Put what it must read in the skill body, and a file
  it must open in `workspace/`.
- **The HTTP channel verifies a caller only when you configure one.**
  `http.WithAuthenticator` (or `bonnie.WithHTTPAuthenticator`) checks every
  route but `GET /bonnie/v1/health`. Without one the channel carries a
  `Principal` it does not examine, so authenticate in front of it, and
  `operation_id` is refused. The chat channels each verify their platform's
  signature and refuse to be built without the credential. That verifies the
  platform and not the person: a user ID in a verified Slack event is Slack's
  word.
- **Run ownership is per host, and the journal does not refuse a second
  writer.** SQLite serialises write transactions and rejects a reused sequence
  number, so two processes that write one run cannot corrupt it. That is
  journal integrity and not turn coordination: two servers that both execute
  the same run still interleave the conversation. SQLite locking needs POSIX
  locks that work, so a journal on a network filesystem is unsafe.
- **Events are journal-anchored.** A reconnect — also after a restart — is
  served from the journal past the in-memory backlog, so the stream has no
  gap. Live-only deltas are the exception, and they are marked.
- **Sandbox lifecycle is journalled, and reclamation is manual.**
  `bonnie sandbox prune` deletes the sandboxes of finished runs; `serve` does
  not sweep them.
- **The mark3labs modules are publicly fetchable.** A scaffolded module
  resolves `bonnie` and `kit` from the proxy; no `GOPRIVATE`. Authoring an
  agent needs Go on your machine. The binary that `bonnie build` makes, and
  the one `install.sh` installs, need nothing on the host.

## [0.8.0] — 2026-09-23

**Breaking: a chat adapter will not build without the credential that proves
a delivery came from the platform.**

### The claims

Still what BONNIE is for. All three hold as they did at `0.7.0`; this release
makes the third one harder to misconfigure:

- **A run survives process death.** The conversation is journalled as it
  happens, so another process — after a crash, on another machine — resumes
  the run with the whole history, including which tools it already called, so
  a side effect is not repeated. A tool-calling step commits as one SQLite
  transaction: whole, or absent. Kit moves to v0.110.0 and the compile-time
  tripwire on `kit.SessionManager` held, so the seams the journal rests on did
  not move.
- **A run parks indefinitely.** A run waiting on a person holds no process and
  no compute — the sandbox opens lazily, so a parked run costs a row in a
  database. Exit the process and answer tomorrow.
- **A run is reachable over HTTP.** `channel/http` mounts its routes under
  `/bonnie/v1`, with an NDJSON event stream that survives a reconnect and a
  restart; the Slack, Discord, Telegram, and GitHub adapters carry the same
  durable run into a thread. **Every chat adapter now refuses to start without
  the credential that verifies its webhook**, so a reachable adapter is never
  one that believes whoever calls it.

The limits are under **Known limits** below, stated as plainly. A framework
that hides its limits gets deployed into situations it cannot handle.

### Added

- **`install.sh`, an install script for the CLI.**
  `curl -fsSL https://raw.githubusercontent.com/mark3labs/bonnie/master/install.sh | bash`
  downloads the release archive for the host, verifies it against the
  release's SHA-256 checksum file, and installs `bonnie`. It refuses a
  binary that it cannot verify, and it refuses macOS and other platforms
  before it downloads. It warns when the host has no Landlock, no provider
  key, or no Go. `scripts/install_test.go` runs it against a fake release,
  and fails when `.goreleaser.yaml` changes the asset names that the script
  expects.
- **`chat.Delivery` and `chat.BearerHeader`**, the delivery helper the four
  chat adapters now share (see *Changed*). `Delivery.PostJSON` marshals,
  posts, reads a bounded body, closes it, and logs a failure under the
  adapter's prefix; any 2xx counts as delivered. They are exported so an
  out-of-tree adapter gets the same behaviour.
- **`channel.ErrUnverifiedWebhook`**, the sentinel a chat adapter's `New`
  returns when it has no verification credential. Test it with `errors.Is`.
- **The wire error code `conversation_corrupt`** (HTTP 409). See *Fixed*.
- **`task examples`, `task examples-gen`, and `task examples-pin TAG=…`**, and
  a CI `examples` job that builds each example tree against its pinned
  release.

### Changed

- **Breaking:** `slack.New` and `telegram.New` now return
  `(*Channel, error)`. Both refuse a config with no verification credential,
  as `discord.New` and `github.New` — which already returned an error — now
  do too. The new sentinel is `channel.ErrUnverifiedWebhook`.

  A host that builds an adapter through `bonnie.WithSlack` and friends is
  unaffected: those options already required the credential from the
  environment and refused a mount without it. A host that calls the adapter
  package directly gets a compile error, then a startup error naming the
  variable to set.

  Why it is a refusal and not a warning: every webhook handler MINTS a
  `channel.Principal` from what its signature check proved, and the run is
  journalled under that identity. An adapter with no credential therefore
  did not degrade to "anonymous", it degraded to "whoever can reach the URL
  is whoever they say they are" — while `channel/http`'s own doc claimed
  each adapter "MINTS the principal from what the check proved". The
  sandbox layer has always refused a control it cannot enforce; the channel
  layer now does the same.

  A bot TOKEN stays optional. An adapter without one receives messages and
  runs turns but cannot write back, which is what the conformance suite
  drives.
- The four chat adapters share one delivery helper, `chat.Delivery`. It
  replaces the same twenty lines copied into each: marshal, build, set the
  content type, authorise, send, log the transport error, close the body,
  check the status, log that too. A 2xx now counts as delivered — `201` and
  `204` were previously logged as failures by a bare `!= 200`.
- `runtime.Record.Payload`'s doc link for the torn-write repair points at
  `Session.repairTail`, the production path, rather than the pure helper.
  `repairTrailingOrphan`'s own doc no longer claims Kit v0.106.0 appends a
  step as two calls — it appends it as one, which `Session.AppendStep`
  already documented. The repair stays for the journals BONNIE inherited.
- Dependencies updated. Kit moves to v0.110.0 and `modernc.org/sqlite` to
  v1.59.0; the transitive set moves with them. No BONNIE source changed:
  `kit.SessionManager` still has exactly 20 methods, so the compile-time
  tripwire in `runtime/session.go` held, and the CGO-free build still
  passes. `charm.land/fantasy` stays `// indirect`, and its OpenAI provider
  now pulls `github.com/charmbracelet/openai-go` in place of
  `github.com/openai/openai-go/v3` — both transitive, neither named by
  BONNIE.
- **`examples/github-bot` and `examples/slack-bot` are agent trees.** Each
  was made with `bonnie init`, is its own Go module pinned to a released
  bonnie, and is run with `bonnie dev` and shipped with `bonnie build` — the
  way a user runs their own agent. They were packages in BONNIE's module,
  run with `go run`, and each README told the reader to rebuild the example
  as a tree by hand. The prompt is now `instructions.md`, not a
  `WithSystemPrompt` constant. `examples/examples_test.go` refuses a drift
  back to the old shape and compiles every tree against the checkout, and a
  new CI `examples` job (`task examples`) builds each tree against its pin.
  After each release, `task examples-pin TAG=…` moves the pins.

### Removed

- **`go run ./examples/github-bot` and `go run ./examples/slack-bot`.** The
  examples are no longer packages in BONNIE's module; each is its own agent
  tree with its own `go.mod`. Run one with `bonnie dev` from its directory.
  `go build ./...` in BONNIE no longer builds them.
- **The Nix flake no longer offers `bonnie` on `aarch64-darwin`.** The
  release has been Linux-only since `0.7.0`; the flake now agrees.
  `aarch64-darwin` keeps `microsandbox` and the dev shell.

### Fixed

- **`bonnie_gen.go` is gofmt-clean.** A tree with no tools generated its
  empty `Tools` literal across two lines, so every such tree failed a
  `gofmt -l` check on a file its author is told never to edit. The
  generator now formats its output with `go/format`. The examples, now
  committed trees, found it.
- **The Nix flake builds again.** `nix/bonnie.nix` had a stale
  `vendorHash`, so `nix profile add github:mark3labs/bonnie` and `nix run`
  failed on `master` and at `v0.7.0`. The hash is refreshed, and the
  package now reports its release version instead of `0.1.0`. Two
  `cmd/bonnie` tests that run `go build` on a scaffolded module need the
  network, so the Nix build skips them. The flake now gives `bonnie` on
  Linux only, as the release does. `aarch64-darwin` keeps `microsandbox`
  and the dev shell.
- **A data race on the event anchor.** `Session.AppendMessage` wrote the
  journal sequence onto a tree entry outside the session lock, while
  `Session.LastMessageSeq` read it under the read lock.
  `Session.AppendStep` already took the lock for the same write.
- **The event bus no longer holds its lock across a journal read.**
  `EventBus.Publish` resolved an event's anchor — for `SQLiteJournal`, a
  real query — while holding `b.mu`. Every live Kit event takes that path,
  one per streamed delta, so a streaming turn serialised thousands of
  database reads against every other publisher and against `Subscribe` and
  its unsubscribe: an HTTP client connecting or disconnecting waited behind
  a disk read.
- **`runtime.ErrCorruptConversation` reaches the wire as 409, not 500.** A
  journal that `Restore` refuses to rewrite fell through to an opaque
  `"internal"` 500, which invites a retry that cannot help. It now carries
  the stable code `conversation_corrupt`, the same treatment
  `ErrRunOwnedElsewhere` gets and for the same reason. A repairable torn
  write is unaffected: it is still repaired and the turn still runs.
- **`Seeded` and `EnvInjected` forward `sandbox.Imaged`.** Both wrappers
  dropped it, and `bonnie.Run` applies both to the default backend before
  the agent sees it, so a host asking which image was in force was told the
  backend runs none. The capability is mirrored, not invented: a wrapper
  over a backend that runs no image still does not claim `Imaged`.
- **A sandbox CLI that never ran is reported as itself.** `DeleteRun`,
  container start, and container create collapsed "the CLI failed to run"
  with "the CLI ran and refused" into one branch that quoted stderr — so a
  missing `docker` binary was reported as `rm bonnie-x: no output`, and the
  cause could not be reached with `errors.Is`.

### Security

- Each adapter's signature check now fails CLOSED. A `Channel` holding no
  credential refuses every delivery instead of accepting every delivery.
  `New` makes that state unreachable, so this is defence in depth: the guard
  that remains if the constructor's is ever lost.
- Telegram compares its webhook secret in constant time. A plain `==` on a
  secret leaks its prefix through timing to anyone who can POST repeatedly,
  which a public webhook URL invites by definition.

### Known limits

Stated as plainly as the claims, and taken from the current
[`README.md`](README.md#limits) rather than carried forward.

- **Linux only.** The floor is the Landlock LSM, so a release builds
  `linux/amd64` and `linux/arm64` and nothing else. macOS and Windows are not
  supported. A kernel older than 5.13, or one booted with Landlock disabled,
  has no default sandbox: BONNIE refuses to start rather than run a tool
  unconfined — use `--sandbox docker` there. `install.sh` refuses any other
  platform before it downloads.
- **The default sandbox is containment, not isolation.** Landlock confines the
  filesystem and keeps host credentials away from a command. It does **not**
  confine the network, and it shares the host kernel.
- **Do not run BONNIE as a user in the `docker` group.** Landlock mediates
  opening a file, not connecting to a socket, so a tool call reaches
  `/var/run/docker.sock` whenever the process can — a full host escape. No
  path setting closes it. Use an unprivileged user, or microsandbox.
- **Docker is namespaces, not a kernel.** Use microsandbox for hostile code.
  microsandbox is verified on Linux with KVM; its network policy is fixed when
  the sandbox is made, and reattaching under a different one fails with
  `ErrPolicyMismatch`.
- **Sandbox egress is open** until a policy is set, and the default backend
  cannot set one — it refuses the policy instead of ignoring it.
- **A skill's bundled files stay on the host.** Kit names a skill's
  `scripts/`, `references/`, and `assets/` files in the activation text with a
  host path the sandbox does not have, so the model is told about a file it
  cannot open. Put what it must read in the skill body, and a file it must
  open in `workspace/`. (This limit had dropped out of `README.md` though it
  still holds; it is restored there in this release.)
- **The HTTP channel verifies a caller only when you configure one.**
  `http.WithAuthenticator` (or `bonnie.WithHTTPAuthenticator`) checks every
  route but `GET /bonnie/v1/health`. Without one the channel carries a
  `Principal` it does not examine, so authenticate in front of it, and
  `operation_id` is refused. The chat channels are different: each verifies
  its platform's signature, and — **new in this release, enforced by `New`** —
  a chat channel with no credential refuses to be built. That verifies the
  platform and not the person: a user ID in a verified Slack event is Slack's
  word.
- **Run ownership is per host, and the journal does not refuse a second
  writer.** SQLite serialises write transactions and rejects a reused sequence
  number, so two processes that write one run cannot corrupt it. That is
  journal integrity and not turn coordination: two servers that both execute
  the same run still interleave the conversation. SQLite locking needs POSIX
  locks that work, so a journal on a network filesystem is unsafe.
- **Events are journal-anchored.** A reconnect — also after a restart — is
  served from the journal past the in-memory backlog, so the stream has no
  gap. Live-only deltas are the exception, and they are marked: a client that
  is not subscribed when a turn runs cannot recover them.
- **Sandbox lifecycle is journalled, and reclamation is manual.**
  `bonnie sandbox prune` deletes the sandboxes of finished runs; `serve` does
  not sweep them.
- **The mark3labs modules are publicly fetchable.** A scaffolded module
  resolves `bonnie` and `kit` from the proxy; no `GOPRIVATE`. Authoring an
  agent needs Go on your machine. The binary that `bonnie build` makes, and
  the one `install.sh` installs, need nothing on the host.

## [0.7.0] — 2026-09-16

**Breaking: every tool call now runs in a sandbox, BONNIE is Linux-only, and a
turn no longer dies with the caller that started it.**

### The claims

Still what BONNIE is for. Two of the three moved this release:

- **A run survives process death.** The conversation is journalled as it
  happens, so another process — after a crash, on another machine — resumes
  the run with the whole history, including which tools it already called, so
  a side effect is not repeated. A tool-calling step commits as one SQLite
  transaction: whole, or absent. A turn now survives the **caller** going away
  as well: an HTTP client that hangs up no longer stops the run, and the
  mandatory `channeltest` case *"a turn survives the caller going away"* holds
  that for all five transports.
- **A run parks indefinitely.** A run waiting on a person holds no process and
  no compute — the sandbox opens lazily, so a parked run costs a row in a
  database. Exit the process and answer tomorrow. The answer now carries the
  verdict: a run parked on an approval resumes knowing what the human decided,
  and on Discord the person answers by pressing a button.
- **A run is reachable over HTTP.** `channel/http` mounts its routes under
  `/bonnie/v1` — health, info, idempotent start, stable error codes, session
  controls — and an NDJSON event stream that survives a reconnect and a
  restart; the Slack, Discord, Telegram, and GitHub adapters carry the same
  durable run into a thread. A host can now put an authenticator in front of
  every route, and `client` speaks the whole contract from Go.

The limits are under **Known limits** below, stated as plainly. A framework
that hides its limits gets deployed into situations it cannot handle.

### Added

- **`sandbox.Landlock()`** — the floor backend, and the default. It confines
  tool calls to the run's own workspace with the Linux Landlock LSM and needs
  nothing installed: no daemon, no image, no KVM, no root.

  Because a Landlock domain is irreversible and process-wide, BONNIE cannot
  restrict its own server — that would take away the journal. Each command
  runs in a child that re-executes BONNIE's binary, restricts itself, and only
  then becomes the command. The restriction survives `execve` and is inherited,
  so **a subshell cannot escape it**. It also builds the child's environment
  from nothing, so a provider API key in the server's environment never reaches
  a model-chosen command.

  It passes all 18 conformance cases. One new dependency,
  `github.com/landlock-lsm/go-landlock`, which issues the syscalls with no cgo
  — `CGO_ENABLED=0` still produces a single static binary.

- `sandbox.ErrOutsideWorkspace`, returned by the file tools when a path would
  leave the workspace, including through a symlink the agent created.

- `sandbox.WorkingDirReporter`, implemented by a backend whose commands do not
  run at `/workspace`. It is how the system prompt learns the real working
  directory; `sandbox.Seeded` and `sandbox.EnvInjected` forward it.

- **`sandbox.EnvInjected` and `bonnie.WithSandboxEnv`** put a fixed set of
  `KEY=value` pairs into every command a sandbox runs, so a run gets a
  credential or a setting **the model must not choose**. The wrapper injects
  over the `Command.Env` seam every backend already honours, so one
  implementation covers Local, Docker, microsandbox, and Landlock alike.
  Injected values are appended last, so an operator-fixed value wins over a
  per-command variable of the same name and cannot be clobbered. It conflicts
  with `WithAgentFactory`, which owns the agent and would otherwise accept the
  setting and ignore it.

- **The tree's `skills/` directory is loaded.** It was embedded by codegen
  from the first release and read by nothing, which `bonnie.Tree.Skills`
  admitted in its own doc comment. `Agent.Run` now resolves it and hands it to
  Kit as `Options.SkillsDir`: each skill's name and description reach the
  system prompt, and the body arrives when the model calls `activate_skill`.

  The path is resolved the way the instructions file is — the tree on disk
  first, the copy codegen embedded second. Kit takes a path, so a built binary
  on a bare host unpacks the embedded skills beside its journal, into
  `<journal>/skills`. That copy is replaced on each start rather than kept: a
  skill is authored data the model never writes, so a skill withdrawn from the
  tree must not survive in the prompt.

- **`bonnie.WithSkills(dir)`**, the option that replaces the tree's skills
  directory, beside `WithInstructions` and `WithWorkspace`. `WithSkills("")`
  is a host with no tree, and is what `bonnie serve` passes.

  A tree with no skills now says so to Kit (`Options.NoSkills`) instead of
  leaving auto-discovery on. Kit would otherwise load `~/.agents/skills` — the
  operator's own editor skills — and the `.agents/skills` under
  `Options.SessionDir`, which `sandbox.Agent` points at the sandbox root. A
  served agent must not take instructions from a directory it merely sits
  beside. Skills a host configured itself through `WithKit` are untouched.

- **A `.env` in the working directory is loaded at startup.** `Agent.Run`
  reads `.env` before it builds the agent or mounts a channel, so a provider
  key (`ANTHROPIC_API_KEY`) or a channel credential (`SLACK_SIGNING_SECRET`)
  can live in a file instead of a shell `export`. An exported variable still
  wins over the file — the file fills a gap, the same rule the channel
  credentials already follow — and a missing `.env` is not an error, while a
  file that cannot be parsed is, named at startup rather than surfacing later
  as an agent with no credentials. The startup banner names the file when it
  loaded one. It is not a manifest: it sets no BONNIE setting, only the
  environment BONNIE already reads.

- **`client` — a public Go client for the wire API.** It speaks the whole
  `/bonnie/v1` contract: health, info, address lookup and bind, start, send,
  respond, get, cancel, reset, clear, compact, and the NDJSON event stream
  with cursor resume. It is terminal-free and holds no conversation state.

  BONNIE's own TUI is now one of its callers rather than a privileged path
  into the server, which is what keeps the client honest: a capability the
  wire cannot express is one the TUI cannot show.

- **`runtime.Approve` and `runtime.Reject`** build an approval answer without
  making a caller take the address of a bool literal, and
  **`client.Client.RespondWith`** puts one on the wire.

- **`http.WithAuthenticator`** verifies every request except
  `GET /bonnie/v1/health` and makes the principal it returns the run's
  identity. It is the HTTP channel's equivalent of the signature check every
  webhook adapter already runs — Slack's HMAC, Discord's Ed25519, GitHub's
  HMAC — for a transport that carries no platform signature. With one
  configured the body's `auth` field is **ignored rather than merged**, so a
  caller cannot add claims to a verified identity. `ErrUnauthenticated`
  refuses a caller with 401; any other error from a verifier is a 500,
  because a verifier that broke has not proved the caller is an impostor.

- **`bonnie.WithHTTPAuthenticator`** passes that verifier from an agent tree.
  Without it the option was unreachable for the way most hosts run BONNIE:
  `Serve` built the framework's HTTP channel with a fixed option list.

- **`channel.RouteHandler`** names the type a `channel.Route` carries, so an
  adapter can wrap one — an authentication check, a rate limit, a trace span
  — without spelling the signature out inline.

- **Chat surfaces gain the controls HTTP already had.** `/cancel`, `/clear`,
  `/compact` and `/help` join `/new`, and every adapter that dispatches
  through `chat.Dispatch` — Slack, Discord, Telegram, GitHub — honours them,
  so a control means the same thing wherever a person is standing.

  Until now a chat user could start a conversation over but could not stop a
  turn, drop the context, or compact it, all three of which an HTTP client
  reached as routes. `/cancel` in particular was unreachable by any means: the
  default turn policy steers a mid-turn message into the running turn, so
  anything typed while the agent worked was read by the model rather than
  acted on. A control is now matched before the policy is consulted.

  The match is the whole trimmed message and is case-insensitive, so "should I
  use /new?" is still a question for the model and "/New" from a phone
  keyboard is still a reset.

- **A parked run can be answered by pressing, not only by typing.**
  `chat.Choices` says what controls a suspension offers and `chat.Answer`
  resolves a press back into the responses that resume the run — one
  vocabulary, so a press means the same thing on every surface.
  `chat.DispatchAnswer` is the inbound counterpart of `chat.Dispatch` for an
  answer that is already resolved.

  **Discord renders them.** An approval arrives with Approve and Reject
  buttons, a question with options gets one button per option, and a press
  lands on the adapter's existing interactions route — no new endpoint and no
  change in the Developer Portal. An approval answers with a *verdict*, never
  with the word on the button, so the agent that asked "may I?" is not left
  interpreting prose.

  A token carries the halting tool call, so a button in an old message cannot
  answer the question the run is parked on today. Pressing replaces the
  message without its buttons, which is the double-press guard. Slack and
  Telegram still answer in text; the shared layer is what they will use.

- **A chat thread shows what the agent is doing while a turn runs.**
  `chat.WithActivity` and `chat.ActivityFunc` report Thinking…, Working…, the
  line of reasoning the model is following, and the tool it called with its
  most telling argument — `read_file runner.go`, `bash go test ./...`, plus
  `+N more` when the model asked for several at once. Slack renders it and
  chooses the surface with `Config.Activity`; the default edits one italic
  placeholder in place and needs no scope the channel does not already have.

  The reducer lives in `channel/chat`, so Discord and Telegram get the same
  statuses when their renderers are written. L1 is untouched: the statuses come
  from Kit's lifecycle events the runner already puts on `Runner.Events`, so an
  indicator costs no extra model work and writes **no journal record**.
  Clearing waits for the watching goroutine to return, because a status still
  in flight would otherwise strand "Working…" above the answer for ever — the
  one failure a thread keeps.

- **The GitHub channel starts a coding run from a label.** `OnIssue` and
  `OnPullRequest` now see every `issues` and `pull_request` action, so the hook
  owns the filter and a maintainer adding `agent-fix` can start a run.
  `IssueCtx` and `PullRequestCtx` gain `State`, `Body`, `Labels`, `Label`,
  `Assignees`, a pull request's `BaseRef`, `HeadRef` and `HeadSHA`, and `Raw` —
  the exact webhook body, for a field the typed layer omits.

  Every issue, pull-request, and comment turn carries a **checkout descriptor**
  in its context — clone URL, default branch, and a pull request's base, head,
  and SHA — so a coding agent clones and branches with the bash tool it has. It
  is public repository metadata and never a token, so it is safe in a
  `RecordContext` that a replay re-injects; a guard test asserts it never lands
  in a `RecordMessage`.

- **`github.Config.OnComment`** replaces the channel's hardwired
  mention-or-bound gate, so a host can admit a comment on its own terms —
  "answer only repository admins" was impossible without forking the channel.
  `CommentCtx` carries the sender, kind, body, and the `Mentioned`/`Bound`
  booleans the default gate uses, so a hook extends the default instead of
  re-deriving it. With no hook the behaviour is unchanged.

- **`channeltest` gains capabilities and a durability case.** An adapter
  declares what it cannot do in `Fixture.Unsupported`, and the suite skips
  exactly those cases with a message naming the capability — so the set of
  skips across the adapters reads as a parity matrix instead of scattered
  `t.Skip` calls. `CapCompaction` is the first, because compaction needs an
  agent that implements `runtime.Compactor`. The new mandatory case, *"a turn
  survives the caller going away"*, holds the durability claim above for every
  transport; all five adapters pass it.

- **`examples/github-bot` and `examples/slack-bot`** — each a runnable agent
  tree in this module, with a README that walks through creating the GitHub App
  or Slack app by hand and live-testing with `bonnie dev`. The scaffold's
  channel menu now lists `bonnie.WithGitHub` beside the others.

### Changed

- **There is no unsandboxed mode.** `bonnie.WithSandbox` now *selects* a
  backend rather than enabling one. A host that configures nothing gets the
  new `sandbox.Landlock` backend instead of this process's filesystem.
  `hostWorkspaceOptions`, which rooted Kit's core tools at the workspace with
  `kit.WithWorkDir`, is deleted.

  The rooting was not enough, and the reason is the whole point: a working
  directory is consulted for **relative** paths only. A live Slack agent ran
  `find /home/<user>/Workspace/my-agent -type f` and read `main.go`,
  `instructions.md`, and `.bonnie/journal.db` — the journal that made its own
  runs durable. Nothing refused it.

- **`--sandbox none` is refused by name**, with a message naming the
  replacement. It was the default, so it lives in scripts and unit files; a
  silent change of behaviour would be worse than a failure. The default for
  `--sandbox` is now `landlock`.

- **The startup banner's no-sandbox warning is gone**, because the condition
  it warned about is gone. The banner names the backend in force instead.

- **The system prompt now names the directory the tools actually use.**
  `sandbox.Agent` sets Kit's `Options.SessionDir` to the directory the chosen
  backend really runs commands at: `/workspace` under Docker or microsandbox,
  the run's host directory under Landlock or Local. Kit's environment block
  otherwise reports the **process** directory, and the model believes the
  prompt over its own `pwd`.

- **Linux only.** Releases build `linux/amd64` and `linux/arm64`. macOS and
  Windows are no longer supported and are off the roadmap. Restoring macOS
  needs a seatbelt backend of equal strength first, not one more build target.

- **A GitHub hook now fires on actions it never saw.** `OnIssue` and
  `OnPullRequest` ran on `opened` only; they run on `labeled`, `edited`,
  `closed`, and the rest as well. A hook written against the old behaviour must
  filter on `Action` itself or it will start runs it did not before.

- **`README.md` was rewritten against the code**, and `AGENTS.md`
  restructured to the agents.md format.

### Deprecated

- `cmd/bonnie/tui.HTTP` and `tui.NewHTTP` remain as aliases for the `client`
  package. Use `client.Client` and `client.New`.

### Removed

- **`examples/minimal` and `examples/hitl-restart`.** The two starter examples
  demonstrated the library rather than a deployment; `github-bot` and
  `slack-bot` replace them, and park-and-resume across a real process kill is
  proven by the live smoke procedure instead of by an example.
- **The spec documents** (`docs/SPEC.md`, `docs/L2.md`, `docs/CHANNELS.md`,
  `docs/SANDBOX.md`, `docs/TASKS.md`, `docs/HANDOVER.md`, `docs/UPSTREAM.md`).
  The code is the spec: the godoc on each exported symbol and the comments on
  the tests carry the reasoning. `docs/RELEASE.md` stays, because the release
  procedure is the one thing that is not in the code.

### Fixed

- **An address now resolves to a run that exists.** `POST
  /bonnie/v1/addresses/{address}` journalled the binding but not the run, so
  the ID it handed back answered 404 on every ID-addressed route until the
  first turn happened to write a record — defeating the route's whole purpose,
  which is to give a client a usable run ID *before* it speaks. A run created
  through the address map is now journalled as `pending` first, so the ID
  works straight away.

  Two visible consequences: such a run carries one extra journal record, and a
  stream read from cursor 0 now opens with a `pending` state event.

### Security

- **A turn is no longer killed by the caller that started it.**
  `runtime.Runner` now derives a turn's context with `context.WithoutCancel`,
  so an HTTP client that hangs up, a webhook handler that returns, or a CLI a
  person interrupts no longer stops a durable run. The journal kept such a run
  at its last checkpoint instead of at an answer. Stopping a turn is
  `Runner.Cancel` and nothing else; a caller's deadline no longer bounds how
  long the agent may think.

- **The authenticator is applied in `Routes`, not in the mux wrapper.** It was
  applied by `HandlerWithOutbound`, but `Routes()` has a second consumer:
  BONNIE's own `Serve` mounts the same routes on a mux of its own, and that
  path never saw the check. An authenticator therefore protected a hand-wired
  host and left the framework's own server wide open — health and info answered
  200 with no credential. Every unit test used `Handler()`, so every unit test
  passed; a live run against a scaffolded tree found it in one request. The
  guard now belongs to the channel's single definition of its own surface, so
  both mounting paths are covered by construction.

- **`operation_id` now requires a principal an authenticator proved.** The
  idempotency key is namespaced by the caller's identity, but the HTTP channel
  derived that identity from the request body, which the caller writes. Any
  caller could name another principal, guess an operation ID, and be handed
  that principal's run. A start that carries `operation_id` without
  `http.WithAuthenticator` configured is now refused with 400 and a message
  naming what is missing.

- **Unmapped errors no longer reach the client verbatim.** `writeError`
  returned `err.Error()` for anything it did not recognise, which sent journal
  paths, driver messages, and SQL to whoever could reach the API. A 500 now
  carries the stable `"internal"` code and nothing else; the detail goes to
  stderr. The mapped sentinels keep their exact wording, which is contract.

- **An approval verdict now reaches the model.** `InputResponse.Approved` was
  declared and read by nothing: only `Text` ever reached the agent, so a run
  parked by `runtime.ApprovalTool` and answered with a bare
  `{"approved": true}` resumed with an **empty message** and the agent had to
  guess what the human decided. Any structured approval — a button, a
  checkbox, an API field — had no way to say yes.

  The field is now `*bool`, because a plain bool cannot say "rejected": false
  is its zero value, so a refusal and an answer that never mentioned approval
  were the same value. An approval resumes the turn as `approved`,
  `rejected`, or the verdict followed by the responder's own words
  (`rejected: that drops production`).

### Known limits

Stated as plainly as the claims, and taken from the current
[`README.md`](README.md#limits) rather than carried forward.

- **Linux only.** The floor is the Landlock LSM, so a release builds
  `linux/amd64` and `linux/arm64` and nothing else. A kernel older than 5.13,
  or one booted with Landlock disabled, has no default sandbox: BONNIE refuses
  to start rather than run a tool unconfined, naming `--sandbox docker`.
- **The default backend is containment, not isolation.** `Landlock` confines
  the filesystem and withholds host credentials. It does **not** confine the
  network and it shares the host kernel, so a local privilege-escalation bug
  is not contained. It therefore does not implement `sandbox.Networked`: a
  network policy given to it is **refused**, naming the backends that can
  enforce one. For untrusted or hostile code use `sandbox.Docker()` or
  `sandbox.Microsandbox()`.
- **Do not run BONNIE as a user in the `docker` group.** Landlock mediates
  opening a file, not connecting to a socket, so a tool call can reach
  `/var/run/docker.sock` whenever the BONNIE process can — and
  `docker run -v /:/host` then defeats the jail completely. No path setting
  closes this; withholding `/var/run` was tried and does not. Use a dedicated
  unprivileged user, or microsandbox. Found by a live model, which named the
  vector itself after every filesystem technique was refused.
- **Docker is namespaces, not a kernel.** Use microsandbox for hostile code.
  microsandbox is verified on Linux with KVM, and its network policy is fixed
  when the sandbox is made: reattaching under a different policy fails with
  `ErrPolicyMismatch`.
- **Sandbox egress is open** until a policy is set, and the default backend
  cannot set one.
- `sandbox.Local()` still exists and still provides **no** containment. It is
  no longer reachable by accident: it has to be named.
- **A skill's bundled files stay on the host.** Kit names a skill's
  `scripts/`, `references/`, and `assets/` files in the text it injects when
  the skill is activated, with a host path. The tools run in a sandbox that
  does not have that path, so the model is told about a file it cannot open.
  Put what the model must read in the skill body, and put a file it must open
  in `workspace/`, which is copied into the sandbox.
- **The HTTP channel verifies a caller only when a host configures one.**
  `http.WithAuthenticator` is new and off by default; without it the body's
  `auth` field is self-asserted, trusted only as far as the deployment's own
  network boundary, and `operation_id` is refused outright. The chat channels
  are different: each verifies its platform's signature, and a channel with no
  credentials refuses to serve. That verifies the platform and not the person:
  a user ID in a verified Slack event is Slack's word.
- **Run ownership is per host, and the journal does not refuse a second
  writer.** SQLite serialises write transactions and rejects a reused sequence
  number, so two processes that write one run cannot corrupt it. That is
  journal integrity and not turn coordination: two servers that both execute
  the same run still interleave the conversation. SQLite locking also needs
  POSIX locks that work, so a journal on a network filesystem is unsafe.
- **Events are journal-anchored, and mid-turn deltas are not.** A reconnect —
  also after a restart — is served from the journal past the in-memory backlog,
  so the stream has no gap. Kit's reasoning and tool deltas stay live-only, and
  the activity indicator above is built from them: a client that is not
  subscribed when a turn runs cannot recover that turn's deltas afterwards.
  Bind the address, open the stream, then send.
- **Sandbox lifecycle is journalled, and reclamation is manual.**
  `bonnie sandbox prune` deletes the sandboxes of finished runs; `serve` does
  not sweep them.
- **The mark3labs modules are publicly fetchable.** A scaffolded module runs
  `go mod tidy` and resolves `bonnie` and `kit` from the proxy; no `GOPRIVATE`.
  Authoring an agent needs Go on your machine. The binary that `bonnie build`
  makes needs nothing on the host.

## [0.6.0] — 2026-09-15

A fix for the first thing a new user sees. `bonnie dev` rendered the first
turn of a fresh agent as a bare answer — no thinking, no tool calls — and
rendered every later turn in full, so the same agent looked broken until you
quit and reopened it. The cause was an ordering mistake, and the cure is a new
endpoint that lets a client subscribe before it speaks.

### The claims

Unchanged by this release, and still what BONNIE is for:

- **A run survives process death.** The conversation is journalled as it
  happens, so another process — after a crash, on another machine — resumes
  the run with the whole history, including which tools it already called, so
  a side effect is not repeated. A tool-calling step commits as one SQLite
  transaction: whole, or absent.
- **A run parks indefinitely.** A run waiting on a person holds no process and
  no compute — the sandbox opens lazily, so a parked run costs a row in a
  database. Exit the process and answer tomorrow.
- **A run is reachable over HTTP.** `channel/http` mounts its routes under
  `/bonnie/v1` — health, info, idempotent start, stable error codes, session
  controls — and an NDJSON event stream that survives a reconnect and a
  restart; the Slack, Discord, Telegram, and GitHub adapters carry the same
  durable run into a thread.

The limits are under **Known limits** below, stated as plainly. A framework
that hides its limits gets deployed into situations it cannot handle.

### Added

- **`POST /bonnie/v1/addresses/{address}`** resolves an address to its run,
  creating and binding one when the address is new, and **runs no turn**. It
  is idempotent: an address that already owns a run returns that run.

  It exists because a turn's reasoning deltas and tool events are live-only —
  the journal keeps the conversation, not the mid-turn deltas — so a client
  that learns its run ID from the *reply* to its first message has already
  missed that turn, and no replay recovers it. Any client that wants a turn's
  live events needs its run ID before it sends the turn. This is how it gets
  one. The read-only `GET` on the same path is unchanged and still creates
  nothing.

### Fixed

- **`bonnie dev` showed no reasoning and no tool calls on the first run of a
  new tree**, then showed both after a quit and restart. The TUI learned its
  run ID from the reply to the first message, so it opened the event stream
  after that turn had already finished. A restart found the address already
  bound, resolved the run at startup, and streamed normally — which is why
  the same agent appeared to work the second time, and why the defect
  survived the earlier fix that only covered reopening.

  The TUI now holds the first message, resolves the run, opens the stream,
  and only then dispatches the turn: **it subscribes before it speaks.** A
  turn is no longer dispatched from a fresh session's send path; it is
  dispatched once the stream is live, so a turn cannot outrun the
  subscription meant to observe it. A stream that fails to open no longer
  strands the message — it is sent anyway and the stream reconnects, because
  a degraded transcript beats a dropped turn.

- **The release-notes guard checked a fixed version.** The check that every
  release states the claims and the limits (added in `0.5.0`) named `0.5.0`
  literally, so it would have passed forever while inspecting a section that
  had already shipped — the exact failure it was written to prevent. It now
  tracks the newest released section in `CHANGELOG.md`.

### Changed

- `cmd/bonnie/tui.Client` gains an `Ensure` method. The package is the CLI's
  own chat surface rather than a framework API, but the interface is
  exported, so an out-of-tree implementation of it needs the new method.

### Known limits

Stated as plainly as the claims. Full detail in [`README.md`](README.md#limits).

- Sandboxing is opt-in. Without it, tool calls run as the host process.
- Docker isolates with namespaces, not a guest kernel. Use microsandbox when
  the threat model includes hostile code.
- microsandbox is verified on Linux with KVM only, not on macOS with Apple
  Silicon, and its network policy is fixed at create time; reattaching under
  a different policy fails with `ErrPolicyMismatch`.
- Sandbox egress is open unless a policy is set.
- **The HTTP channel carries a `Principal` but does not verify it.**
  Authenticate in front of it. The chat channels are different: each verifies
  its platform's signature — Slack, Discord, Telegram, and GitHub's
  `X-Hub-Signature-256` — and a channel without its credentials refuses to
  serve. That verifies the platform, not the person: a user ID inside a
  verified event is the platform's word.
- **Run ownership is per host, and the journal does not refuse a second
  writer.** SQLite serialises write transactions and rejects a reused
  sequence number, so two processes writing one run cannot corrupt it. That
  is journal integrity, not turn coordination: two servers that both execute
  the same run still interleave the conversation. SQLite's locking needs
  working POSIX locks, so a journal on a network filesystem is unsafe.
- **Events are journal-anchored, and mid-turn deltas are not.** A reconnect
  past the in-memory backlog is served from the journal, so the stream has no
  gap. Kit's reasoning and tool deltas stay live-only: a client that is not
  subscribed when a turn runs cannot recover that turn's deltas afterwards.
  Bind the address, open the stream, then send.
- Reclaiming the sandboxes of finished runs is a command
  (`bonnie sandbox prune`), not a background sweep.
- The mark3labs modules are publicly fetchable; authoring an agent needs Go
  on your machine, while the binary `bonnie build` produces needs nothing on
  the host.

## [0.5.0] — 2026-09-15

The channels increment. A run now reaches a person wherever they already are:
a GitHub App joins Slack, Discord, and Telegram, and an agent can **open** a
conversation instead of only answering one. Two breaking changes land with
it — the HTTP channel moves under `/bonnie/v1`, and the adapter-facing `chat`
API takes a normalised turn.

### The claims

Unchanged by this release, and still what BONNIE is for:

- **A run survives process death.** The conversation is journalled as it
  happens, so another process — after a crash, on another machine — resumes
  the run with the whole history, including which tools it already called, so
  a side effect is not repeated. A tool-calling step commits as one SQLite
  transaction: whole, or absent.
- **A run parks indefinitely.** A run waiting on a person holds no process and
  no compute — the sandbox opens lazily, so a parked run costs a row in a
  database. Exit the process and answer tomorrow. A parked run now also parks
  in a GitHub thread, and the reply that wakes it needs no mention.
- **A run is reachable over HTTP.** `channel/http` mounts its routes and an
  NDJSON event stream that survives a reconnect and a restart, now under
  `/bonnie/v1` with health, info, idempotent start, and stable error codes;
  the Slack, Discord, Telegram, and GitHub adapters carry the same durable
  run into a thread.

The limits are under **Known limits** below, stated as plainly. A framework
that hides its limits gets deployed into situations it cannot handle.

### Breaking changes

1. **The HTTP channel lives under `/bonnie/v1`.** `POST /runs` is now
   `POST /bonnie/v1/runs`, and every other route moved the same way.
   `/bonnie/` is the framework's reserved namespace: a channel that mounts a
   route under it is refused at startup with an error naming the channel and
   the path, instead of a mux panic or a silent shadow. `bonnie chat` and the
   TUI follow the new paths. **Update any client that calls the old paths;
   they no longer exist.** (T-030)
2. **Breaking for adapter authors:** `chat.NewCore` takes the channel's name;
   `chat.Dispatch` and `chat.Route` take a `chat.Turn`; the core applies the
   address prefix itself, so adapters pass the bare platform key;
   `channel.SessionRef` gains `Reset`, `Clear`, and `Compact`; route handlers
   take a `channel.Outbound` beside their `Inbound`. An out-of-tree adapter
   must be updated to compile. (T-028, T-032, T-033)

### Added

- **Cross-channel hand-offs and proactive sessions.** Route handlers get
  `channel.Outbound` beside their `Inbound`, and any mounted channel that
  implements `channel.Receiver` can be asked to start a conversation with
  no inbound message. Slack opens a thread and threads the reply; Discord,
  Telegram, and GitHub post where their target says. The address is bound
  before the turn runs, so a reply that arrives mid-turn continues the
  run, and a hand-off to an address that already carries a conversation
  joins it rather than replacing it. The initiating principal is recorded
  on the destination run. `GITHUB_INSTALLATION_ID` is the installation a
  GitHub hand-off posts with. (T-033)
- **GitHub channel.** `channel/github` and `bonnie.WithGitHub`: a GitHub
  App whose webhooks turn comments into turns. A `@<bot>` mention on an
  issue, a PR, or a review thread starts or continues a run bound to that
  thread (a review thread is its own conversation); a reply in an
  already-bound thread continues it with no mention; the PR's title, base,
  head, and changed-file patches reach the model as per-turn context, with
  generated files dropped and the block capped. Delivery is a comment on
  the thread, split at GitHub's limit, with an `eyes` reaction on the
  triggering comment. Deliveries are verified (`X-Hub-Signature-256`) and
  deduplicated (`X-GitHub-Delivery`); the installation token is minted per
  event and never reaches a run. `OnIssue`, `OnPullRequest`, and
  `OnCheckSuite` hooks dispatch the events a maintainer agent wants on its
  own. (T-029)
- **Idempotent start and stable error codes.** `POST /bonnie/v1/runs`
  accepts `operation_id`: the same ID from the same authenticated principal
  returns the run the first call created instead of dispatching again (it
  is an entry in the journalled address map, so it survives a restart).
  Without a principal it is refused. Every error body carries a stable
  `code` beside the message — `run_not_found`, `run_retired`,
  `run_owned_elsewhere`, `compaction_unsupported`, `bad_request`, and
  friends — so a client switches on it instead of parsing prose. (T-031)
- **Session controls.** `channel.SessionRef` gains `Reset`, `Clear`, and
  `Compact`; the runner implements them once (`runtime/controls.go`) and
  every transport shares them. `POST /bonnie/v1/runs/{id}/reset` retires a
  run for good (`retired` is the only terminal state a run cannot leave;
  the address is freed, the history stays readable), `/clear` drops the
  conversation from the model's context and keeps the run, `/compact`
  summarises on demand through Kit's public `Compact`. Chat surfaces get
  the same thing by typing `/new` in the thread. The address map is
  prefixed by the core with the channel's name — two channels cannot bind
  the same key, and an adapter never spells its own prefix. (T-032)
- **Per-turn context and a normalised turn.** `runtime.Input` gains
  `Context` (facts for the model on this turn only — journalled as a
  `RecordContext`, shown in front of the prompt through Kit's
  context-prepare hook, never kept as history), `Title`, and `Origin`
  (channel and kind, recorded once). `chat.Turn` is the one shape every
  adapter normalises a platform event to; `chat.Dispatch` and `chat.Route`
  take it. Slack, Discord, and Telegram set a kind and a title and pass the
  sender as context. The HTTP channel accepts `context` and `kind`.
  `runs list` shows the title. (T-028)
- `GET /bonnie/v1/health` answers `{"ok":true,"status":"ready"}` before any
  run exists and without touching the journal. `GET /bonnie/v1/info` reports
  the agent name, the BONNIE version, and the mounted channels. (T-030)
- **Two options**: `bonnie.WithName` sets the agent name that
  `GET /bonnie/v1/info` reports, and `bonnie.WithGitHub` mounts the GitHub
  channel. (T-029, T-030)

### Changed

- **A tag publishes the release notes, not a commit list.** `release.yml`
  slices the tag's section out of `CHANGELOG.md` (`scripts/release-notes.sh`)
  and passes it to `goreleaser --release-notes`, so the published body states
  the three claims and the limits with no human edit. That edit was done by
  hand at `v0.1.0` and again at `v0.4.0`; a manual step after every tag is
  eventually skipped, and the failure is silent — the release simply stops
  saying what BONNIE cannot do. A tag whose version has no matching section
  now **fails the release workflow**, naming the missing heading, rather than
  publishing an empty body. (T-027)

- **The public-API boundary is a Kit extension, not a CI job.**
  `.kit/extensions/kit-boundary.go` blocks a `write` or `edit` that would add
  `github.com/mark3labs/kit/internal/...` or `charm.land/fantasy` to a `.go`
  file in this repository, and names the import, the file, and the way out in
  the refusal, so the agent corrects itself in the same turn instead of
  learning about it minutes later in CI. It is scoped to this repository: a
  sibling checkout has its own rules, and Kit's own code must import Kit's
  internals. The `boundary` CI job is removed: `depguard` in the `lint` job
  already denies both paths by prefix, whatever the module layout, so the
  rule loses no coverage. `depguard` remains the authority; the extension
  only runs when a person drives Kit in this checkout. (T-034)

### Fixed

- **The microsandbox conformance suite is reliable again** (20/20 runs, was
  ~0/10). Two defects. The suite built a sandbox provider per test case, so
  parallel cases issued ~20 concurrent `msb create` calls and locked msb's
  own SQLite store — a load BONNIE never produces, because a host shares one
  provider whose mutex serialises every create. And `msb ps --all`
  intermittently reports an empty list while sandboxes are running, so the
  adapter thought a live sandbox was absent and tried to recreate it; `Open`
  now adopts a sandbox that already exists, still refusing one whose network
  policy does not match. An `msb` failure also keeps its own cause now
  instead of only the headline. (T-026)

### Known limits

Stated as plainly as the claims. Full detail in [`README.md`](README.md#limits).

- Sandboxing is opt-in. Without it, tool calls run as the host process.
- Docker isolates with namespaces, not a guest kernel. Use microsandbox when
  the threat model includes hostile code.
- microsandbox is verified on Linux with KVM only, not on macOS with Apple
  Silicon, and its network policy is fixed at create time; reattaching under
  a different policy fails with `ErrPolicyMismatch`.
- Sandbox egress is open unless a policy is set.
- **The HTTP channel carries a `Principal` but does not verify it.**
  Authenticate in front of it. The chat channels are different: each verifies
  its platform's signature — Slack, Discord, Telegram, and GitHub's
  `X-Hub-Signature-256` — and a channel without its credentials refuses to
  serve. That verifies the platform, not the person: a user ID inside a
  verified event is the platform's word.
- **Run ownership is per host, and the journal does not refuse a second
  writer.** SQLite serialises write transactions and rejects a reused
  sequence number, so two processes writing one run cannot corrupt it. That
  is journal integrity, not turn coordination: two servers that both execute
  the same run still interleave the conversation. SQLite's locking needs
  working POSIX locks, so a journal on a network filesystem is unsafe.
- Events are journal-anchored: a reconnect past the in-memory backlog is
  served from the journal, and Kit's mid-turn deltas stay live-only.
- Reclaiming the sandboxes of finished runs is a command
  (`bonnie sandbox prune`), not a background sweep.
- The mark3labs modules are publicly fetchable; authoring an agent needs Go
  on your machine, while the binary `bonnie build` produces needs nothing on
  the host.

## [0.4.0] — 2026-09-14

The configuration-is-code increment, and the journal becomes a database.
Three breaking changes land together: the manifest is gone, the root package
`bonnie` owns the serving path, and the journal is SQLite instead of one JSONL
file per run. The TUI renders assistant messages as markdown, the agent's
files are rooted in its workspace, and the defects a read-only audit found are
closed.

### The claims

Unchanged by this release, and still what BONNIE is for:

- **A run survives process death.** The conversation is journalled as it
  happens, so another process — after a crash, on another machine — resumes
  the run with the whole history, including which tools it already called, so
  a side effect is not repeated. A tool-calling step now commits as one SQLite
  transaction: whole, or absent.
- **A run parks indefinitely.** A run waiting on a person holds no process and
  no compute — the sandbox opens lazily, so a parked run costs a row in a
  database. Exit the process and answer tomorrow.
- **A run is reachable over HTTP.** `channel/http` mounts six routes and an
  NDJSON event stream that survives a reconnect and a restart; the Slack,
  Discord, and Telegram adapters carry the same durable run into a chat
  thread.

The limits are under **Known limits** below, stated as plainly. A framework
that hides its limits gets deployed into situations it cannot handle.

### Breaking changes

1. **The manifest is gone. Configuration is code.** `agent.yaml` (and
   `agent.toml`, `agent.json`) no longer exist. A tree's data lives at fixed
   paths and everything else is a Go option on `bonnie.New`, so a setting that
   does not exist is a compile error rather than a key nothing reads. See
   **Migration** below.
2. **`bonnie.New().Serve()` replaces `bonnie.Main()`.** The root package is
   new in this release and owns the serving path.
3. **The journal is SQLite.** `runtime.FileJournal` and
   `runtime.OpenFileJournal` are replaced by `runtime.SQLiteJournal` and
   `runtime.OpenSQLiteJournal`. One `<root>/journal.db` holds every run
   instead of one JSONL file per run plus a lock file per run. The driver is
   pure Go (`modernc.org/sqlite`), so BONNIE still builds and cross-compiles
   with `CGO_ENABLED=0` and `bonnie build` still ships one static binary.

**Your existing runs are migrated, not lost.** A `.bonnie` that still holds
`runs/*.jsonl` is imported the first time the new journal opens it: records
keep their sequence numbers, and each source file is renamed to
`<run>.jsonl.imported` rather than deleted. The import is idempotent.

### Added

- **The root package `github.com/mark3labs/bonnie`.** `bonnie.New().Serve()`
  is a complete agent: it reads `instructions.md`, roots the agent's files in
  `workspace/`, journals to `.bonnie`, serves the HTTP channel, and drains
  in-flight turns on a signal. The scaffolded `main.go` is now one call.

  ```go
  package main

  import "github.com/mark3labs/bonnie"

  func main() { bonnie.New().Serve() }
  ```

- **Options for everything that is not a file**, passed to `New`: `WithModel`,
  `WithSystemPrompt`, `WithSandbox`, `WithNetwork`, `WithTools`, `WithKit`,
  `WithSlack`, `WithDiscord`, `WithTelegram`, `WithChannel`, `WithWorkspace`,
  `WithInstructions`, `WithJournal`, `WithAddr`, `WithListener`,
  `WithShutdownTimeout`, and `WithAgentFactory` for a host that brings its own
  agent.
- **`Agent.Run(ctx) error`** is `Serve` without the process — no flags, no
  signal handler, no exit — for a host that already owns those.
- **The default layout as exported constants**: `DefaultInstructions`,
  `DefaultWorkspace`, `DefaultSkills`, `DefaultJournal`, `DefaultAddr`. The
  scaffold, codegen, the dev loop, and the runtime all read them, so the
  layout is defined once.
- **`bonnie.Register` / `Registered` / `Tree`.** The generated
  `bonnie_gen.go` registers the tree's tools and embedded data from `init`, so
  `main.go` never names a tool or an embed.
- `-addr` and `-model` are operator flags on every serving binary, applied
  after the options, so one binary can move port or model without a rebuild.
- Assistant messages render as **markdown** in `bonnie dev` and `bonnie chat`
  (a T-021 extension): headings, bold, lists, tables, and code blocks render
  through herald-md — the same typography stack upstream Kit's TUI uses — in
  the TUI's existing palette. Streaming text renders live, so the answer
  arrives already shaped; user messages, tool lines, and reasoning stay as
  they were, because typed text is not markdown.
- `sandbox.Imaged`, the optional interface a provider implements to report the
  image it really runs. It is what makes the image testable and the banner
  honest.
- `chat.DeliveryText` and `chat.FirstLine`: the rule for what a person sees at
  the end of a turn, which lived three times, byte for byte, in the Slack,
  Discord, and Telegram adapters.
- `TestCancelledRunContinuesInASecondRunner`: `Runner.Cancel` promises a
  cancelled run can be continued, and every durability claim needs a test that
  crosses a process boundary. This one had only a same-process `Restore`.

### Changed

- **`bonnie init`** scaffolds `main.go`, `instructions.md`, `go.mod`,
  `bonnie_gen.go`, `skills/`, `workspace/`. `--model` writes the option into
  `main.go`. `--format` and `--title` are gone.
- **`bonnie build`** names its output from the module path in `go.mod`.
- **`bonnie serve`** is the flag-only generic host, for running an agent with
  no tree. `--agent` and `--config` are gone: a tree's configuration is Go in
  its own `main.go`, so a tree is served by running it.
- **The startup banner reports the address actually bound**, so `-addr :0`
  names the real port.
- **Chat channels mount through options** rather than manifest keys. Their
  credentials still come only from the environment, and a missing one is still
  a startup error naming the variable.
- **The agent's root is now the workspace.** A tool call's relative path
  resolves inside the tree's `workspace/` (or `bonnie.WithWorkspace`) instead
  of wherever the server was started from. Serving a tree used to drop a
  model's files on the tree itself — beside `instructions.md` and `.bonnie/`,
  the journal a run's durability depends on. A sandboxed run already rooted
  everything at `/workspace`, so the two modes now agree. The banner names the
  resolved workspace. Serving without a tree is unchanged: the process's own
  directory stays the root. Note that this is a root, not a jail — an absolute
  path still escapes, which is what the sandbox is for.
- `gopkg.in/yaml.v3` and `github.com/pelletier/go-toml/v2` return to indirect
  dependencies. `go.sum` is unchanged.

#### The journal

- **A tool-calling step is one transaction.** The "torn single write" window
  the JSONL journal documented is closed, not narrowed: a step commits whole
  or is absent. `Restore`'s torn-write repair stays, because imported runs and
  third-party journals can still carry the shape.
- **Concurrent writers are safe instead of refused.** SQLite serialises write
  transactions across processes and the `(run_id, seq)` primary key makes a
  reused sequence number a constraint violation. The per-run `flock`, the
  `.lock` files, and the refusal they produced are gone.
  `runtime.ErrRunOwnedElsewhere` stays exported for journals backed by a store
  that admits one writer — `channel/http` still maps it to 409 — but the
  built-in journal never returns it. **Journal integrity is not turn
  coordination:** two servers executing the same run still interleave the
  conversation, and `SECURITY.md` says so.
- **A read of an unknown run costs nothing.** There is no per-run handle to
  create, so the growth the previous release had to fix cannot recur.
- **`FsyncInterval` is now `FsyncRelaxed`, and `WithFsyncInterval` is
  removed.** SQLite's `synchronous=NORMAL` fsyncs at write-ahead-log
  checkpoints, not on a clock, so the old name promised something the store
  cannot deliver. `WithFsync(FsyncAlways)` is still the default and still
  means every commit is on the platter.
- **A record payload that is not valid JSON is refused at write time.** The
  JSONL encoder caught this for free; a blob column does not.
- **A store written by a newer BONNIE is refused** with
  `runtime.ErrJournalSchema` rather than read with the wrong shape.
- Inspecting a run is now `sqlite3 .bonnie/journal.db "SELECT ... FROM records
  WHERE run_id = ..."` instead of `jq` over a JSONL file.
- The binary grows about **3.8 MB** stripped (measured: 77.4 → 81.1 MB,
  linux/amd64), and a **cold-cache** `go build ./...` grows about 12%
  (98 → 110 s). The driver is transpiled C, so it adds roughly 1.6 million
  generated lines to the dependency graph. Warm builds are unaffected.

### Fixed

- **A run's event stream no longer leaks goroutines when a client
  disconnects.** `Runner.StreamEvents` forwarded on an unbuffered channel, so
  a client that went away between two events left the forwarder parked on its
  send and the bus subscriber's pump parked behind it. A goroutine blocked in
  a send cannot see the unsubscribe. Every reconnect that raced an event cost
  a long-lived server two goroutines and their queued events, for the life of
  the process. Stopping a stream now releases every send it owns.
- **`--sandbox-image` reaches every backend that runs an image.**
  `--sandbox auto` built its candidates without the image, so an operator who
  named one silently got the default; `--sandbox local` accepted an image it
  cannot run and is now refused. The startup banner names the image in force.
- **Reserved runs are no longer addressable.** BONNIE keeps its address map in
  a run under `runtime.ReservedRunPrefix`. A caller who knew the prefix could
  start a turn on it through any chat transport, and `bonnie sandbox prune`
  listed it to operators. Both now refuse and filter.
- **`channel/http` caps a request body at 1 MiB**, with a 413 that names the
  limit — the webhook adapters always did. A write refused because another
  process owns the run (`ErrRunOwnedElsewhere`) now answers 409, not 500.
- **A chat-channel option can be reused.** `WithSlack`, `WithDiscord`, and
  `WithTelegram` filled their captured `Config` from the environment, and the
  fill only writes an empty field — so the first use left the credentials
  inside the option's closure and every later use skipped the environment. An
  `Option` held in a variable and passed to two agents, or an `Agent.Run`
  called twice, served the credentials read at the first call rather than the
  ones set now. Each option now copies its config per call. Guard test:
  `TestChatChannelOptionsMount`.
- **The workspace really is seeded.** `sandbox.Seeded` was written and tested
  but never called, so the configured workspace was accepted and ignored —
  what invariant 13 exists to forbid. A sandboxed run now receives its seed
  files at `/workspace`.
- `bonnie dev` watched the workspace, so the agent restarted itself for doing
  its job: a model writing a file — the ordinary case now that the workspace
  is the agent's root — tripped a rebuild that SIGTERMed the child still
  serving the turn. The workspace is the loop's output, not its input, and is
  no longer watched. Edits to instructions, tools, and `go.mod` still
  hot-reload; verified live in both directions. Guard tests:
  `TestWorkspaceIsNotWatched`, `TestWorkspaceDirIsTheRuntimeWorkspace`.
- Assistant lines longer than 100 columns were silently truncated by the old
  `MaxWidth` style: every cell past the limit was lost, on every message.
  Assistant prose now wraps at the terminal width and keeps everything —
  verified live against a real model, including a wrapped 300-character
  paragraph and the hot-reload reconnect path.
- The TUI's cursor landed below the footer once the transcript grew taller
  than the terminal: the view reported a frame-relative row, while inline mode
  moves the terminal cursor to that exact screen position, so the terminal
  clamped the move to its bottom row. The view now subtracts the rows the
  screen has scrolled past. Pinned by a test and verified in tmux at three
  window heights.

### Removed

- `runtime.FileJournal` and `runtime.OpenFileJournal`, the per-run `.lock`
  files, and `runtime.WithFsyncInterval`.
- `agent.Manifest`, `agent.Load`, `agent.LoadFile`, `agent.ParseManifestData`,
  `agent.APIVersion`, and every manifest sentinel error.
- `serve --agent`, `serve --config`, `init --format`, `init --title`,
  `build --config`.
- The generated `discoveredTools()`, `embeddedInstructions()`,
  `embeddedManifest()`, `embeddedSkills()`, and `embeddedWorkspace()` symbols,
  replaced by `bonnie.Register`.
- `EventBus.Backlog`, which had no caller in the repository.
  `Runner.StreamEvents` is the supported way to read a run's events, and it
  has been since journal-backed catch-up landed.
- **The zero-Go path.** A host with no Go toolchain can no longer serve a tree
  from data. Authoring an agent now needs Go from the first step, as it did
  from the fifth before. The binary `bonnie build` produces still needs
  nothing on the host, which is the end of the arc that matters.

### Migration

For each key in your `agent.yaml`, write the option in `main.go`:

| Manifest | `main.go` |
|---|---|
| `model:` | `bonnie.WithModel(...)` |
| `instructions:` | `bonnie.WithInstructions(...)` — or rename the file to `instructions.md` |
| `workspace:` | `bonnie.WithWorkspace(...)` — or rename the directory to `workspace/` |
| `sandbox.kind`, `sandbox.image` | `bonnie.WithSandbox(sandbox.Docker(...))` |
| `sandbox.network` | `bonnie.WithNetwork(sandbox.NetworkPolicy{...})` |
| `channels.http.addr` | `bonnie.WithAddr(...)` |
| `channels.slack` / `discord` / `telegram` | `bonnie.WithSlack(...)` / `WithDiscord(...)` / `WithTelegram(...)` |
| `title:` | the directory name — `bonnie build` reads it from `go.mod` |
| `apiVersion:` | nothing; the Go type system replaced it |

Then delete `agent.yaml` and run `bonnie build`. A tree that has no `main.go`
(the old zero-Go scaffold) gets one from `bonnie init .`, which never
overwrites what is already there.

Your journal needs no action: point the new binary at the same `.bonnie` and
the `runs/*.jsonl` files are imported on first open.

### Known limits

Stated as plainly as the claims, because a framework that hides its limits
gets deployed into situations it cannot handle.

- **Early and experimental.** Pre-1.0: the API can change without notice, and
  no release is suitable for workloads whose loss would hurt.
- **Sandboxing is opt-in.** Without it, tool calls run as the host process.
- **Docker is namespaces, not a guest kernel.** Use microsandbox when the
  threat model includes hostile code.
- **Sandbox egress is open** unless a policy is set.
- **microsandbox is verified on Linux with KVM only**, not on macOS with Apple
  Silicon. Every network policy mode is enforced, but the policy is fixed at
  create time; reattaching under a different one fails with
  `ErrPolicyMismatch`.
- **No auth verification on the HTTP channel.** It carries a `Principal`; it
  does not check one. Authenticate in front of it. The chat channels each
  verify their platform's signature, which verifies the platform, not the
  person.
- **Run ownership is per host, and the journal no longer refuses a second
  writer.** SQLite serialises write transactions and rejects a reused sequence
  number, so two processes writing one run cannot corrupt it. That is journal
  integrity, not turn coordination: two servers that both execute the same run
  still interleave the conversation. SQLite's locking also needs working POSIX
  locks, so a journal on a network filesystem is still unsafe.
- **Events are journal-anchored.** The stream replays the journal past the
  in-memory backlog, so a reconnect — even after a restart — has no gap.
  Kit's mid-turn deltas stay live-only, marked as ephemeral.
- **Reclaiming sandboxes is manual.** `bonnie sandbox prune` deletes the
  sandboxes of terminal runs; `serve` does not sweep them on its own yet.
- **Authoring needs Go.** A scaffolded module resolves `bonnie` and `kit` from
  the public proxy; the binary `bonnie build` produces needs nothing on the
  host.

## [0.3.0] — 2026-09-13

The terminal increment: `bonnie dev` and `bonnie chat` open a scrollback TUI
that streams one durable conversation — tool calls render as a spinner that
becomes a check mark with the result on the next line. Two event-stream
defects that swallowed tool calls are fixed.

### Added

- Built-in terminal TUI (T-021): `bonnie dev` opens the TUI against its
  serving child, `bonnie chat` connects the same TUI to any running HTTP
  channel. One durable conversation per address, streamed from the
  journal-cursor; a `dev` hot reload reconnects without losing the
  conversation, and `ctrl+w` cancels the running turn.
- `GET /addresses/{address}` on the HTTP channel: a read-only lookup that
  returns the run bound to an address and its journal cursor, and creates
  nothing on a miss. The TUI uses it to open the stream before the first
  message of a session.
- A tool call renders as one compact entry: a spinner while the tool works, a
  check mark when it completes, and an indented arrow with a one-line,
  Unicode-safe truncated result.

### Fixed

- Live events that share one journal anchor were dropped past the first one.
  A tool-call start, parsed call, execution, and
  result can all land on the same anchor, so the TUI lost tool calls the
  event bus held.
- A TUI that reopened an existing address learned its run ID only after the
  first turn, so tool and reasoning events of that turn never reached it.
  The startup lookup fixes the stream; guard tests cover both.

## [0.2.0] — 2026-09-13

The L2 increment: an agent is a tree, not a hand-wired library. `bonnie init`
scaffolds one; `bonnie serve --agent` serves it with no compile; `bonnie dev`
hot-reloads it; `bonnie build` graduates it into one static binary. The chat
channels move durable runs into Slack, Discord, and Telegram, and every
scaffold is now a Go module that builds against the public modules.

### Added

- Chat channels: Slack, Discord, and Telegram adapters (`channel/slack`,
  `channel/discord`, `channel/telegram`), zero new dependencies. Each
  mounts one verified webhook — Slack's v0 HMAC with a replay window,
  Discord's Ed25519, Telegram's shared secret — acknowledges inside the
  platform's deadline, runs the turn in a goroutine the handler does not
  outlive, and delivers the reply to the thread. A run that parks for human
  input posts its question into the chat, and the next message there
  resumes it. Each adapter joins the `channeltest` conformance suite.
- `channel/chat`, the shared plumbing the adapters are built on: the
  journalled address map (moved from `channel/http`), per-run turn locks,
  the `SessionRef` implementation, the dispatch rule (a reply to a parked
  run resumes it — a chat surface cannot say "this is a resume"), and the
  goroutine delivery model.
- The manifest's `channels:` keys (`channels.slack`, `channels.discord`,
  `channels.telegram`) enable the chat adapters for `bonnie serve --agent`.
  Configuration lives in the manifest; credentials live in the environment
  (`SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET`, `DISCORD_BOT_TOKEN`,
  `DISCORD_PUBLIC_KEY`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_WEBHOOK_SECRET`),
  and a missing one is a startup error that names the variable. Secrets in
  the manifest are refused by construction: the keys do not exist.
- New rule: a chat channel verifies its caller or refuses to serve. The
  per-platform setup, the dispatch and steering rules, and what is
  deliberately not implemented are in the `channel` godoc.

### Changed

- L2 discovery, first increment (`T-017`): the agent tree is discovered
  from a manifest. `bonnie init` scaffolds a tree — `agent.yaml`,
  `instructions.md`, and the `skills/` and `workspace/` seed directories —
  as a Go module with a `main.go` that defines the default agent;
  `--tools` adds a sample tool. `bonnie serve --agent DIR` serves a
  discovered tree with no build and no toolchain: instructions from disk,
  model, sandbox, and channel bindings from the manifest. Flags override
  the manifest, and the startup banner names the source that won. Files under
  the manifest's `workspace:` are mirrored into every run's sandbox on every
  backend, and a file the model already wrote is never overwritten.
- Always a Go module (`Option A`, this release): `bonnie init` no longer
  offers a zero-Go fork. Every scaffold builds out of the box — the authored
  `main.go` reads the manifest for the model, address, and instructions and
  wires the default agent, so `go run .`, `bonnie dev`, and `bonnie build`
  converge on it. `--tools` only adds a sample tool directory; the modules
  are public, so `go mod tidy` resolves them from the proxy.
- The manifest loader (`agent/`) is strict on one code path for `agent.yaml`,
  `agent.toml`, and `agent.json`: an unknown key, an unknown `apiVersion`,
  or two manifests in one root is an error naming what is wrong. `mcp` and
  `skills` are reserved and refused until their loading stories exist.
  `serve --agent` refuses a tree that carries Go tools and names
  `bonnie build` (the codegen increment, T-018) — a partial run is never
  the answer.

### Changed

- The `bonnie` CLI is built on cobra, and its help and errors render with
  [fang](https://github.com/charmbracelet/fang). Commands, flags, and exit
  codes are unchanged; help gains styling, shell completions, and a man
  page (`bonnie man`). `--version` and `bonnie version` print the same
  version. A bare `bonnie` now prints help instead of exiting 2.

### Removed

- The depguard bans on the Charm stack (bubbletea, bubbles, huh, lipgloss,
  fang) and the "headless" invariant behind them. They were a design
  starting position, not a boundary. BONNIE ships no TUI today and none is
  planned, but importing the stack is no longer a lint failure. The
  public-Kit-SDK boundary (`kit/internal`, `charm.land/fantasy`) is
  unchanged and still enforced by `depguard` and the `boundary` CI job.

## [0.1.0] — 2026-09-12

The first release. Tagged at the state of `master` on 2026-09-12; everything
the working tree held is in the tag.

### Added

- Durable run executor (`runtime` package) built on the public Kit SDK only
- Journal-backed `kit.SessionManager` with lossless typed-message replay
- `Session.AppendStep` implements `kit.StepAppender` (Kit `v0.106.0`): a
  tool-calling step reaches the journal as one call, and
  `FileJournal.AppendStep` commits it as one buffered write and one fsync —
  the torn-write window went from "any crash between two fsyncs" to "a torn
  single Write". A cancelled context cannot drop a completed step
- `runtime.StepJournal`: an optional interface on BONNIE's own `Journal`
  seam, mirroring Kit's pattern so a host journal is not forced to implement
  the batch method. `FileJournal` and `MemoryJournal` implement it; anything
  else keeps the per-record fallback, covered by the torn-write repair
- Torn-write repair on `Restore` that drops incomplete trailing tool-calling
  steps
- `FileJournal` with JSONL format (one file per run, fsync policy configurable)
- `Runner.Cancel` to interrupt in-flight turns and `RunCancelled` state
- `Runner.Steer` for external turn control
- Events are journal-anchored and survive a reconnect past the backlog: every
  event carries the journal position it belongs to, the stream replays the
  records when the in-memory backlog has moved past the cursor, and the
  stream survives a process restart. Kit's mid-turn deltas stay live-only,
  marked as ephemeral
- `runtime.RunnerOption` and `runtime.WithEventBuffer` tune the reconnect
  backlog; a smaller buffer costs memory, not correctness
- Cross-process run ownership: the file journal takes an exclusive `flock`
  per run on first write and refuses a second writer with
  `runtime.ErrRunOwnedElsewhere` instead of letting records interleave and
  sequence numbers collide. Reads stay unlocked, so `runs list` and
  `runs show` work from any process. The lock is per host and says nothing on
  a network filesystem
- The `channeltest` package: the conformance suite a channel adapter joins
  instead of writing its own tests. The HTTP adapter is the first member. An
  unknown `TurnPolicy` is refused with `channel.ErrUnknownTurnPolicy`
  (HTTP: 400) rather than silently queueing
- microsandbox: every network policy mode is enforced. `SetNetworkPolicy`
  maps `deny-all` to `msb create --no-net` and an allow-list to
  `--net-rule allow@<host>`, verified with real egress
- `ErrPolicyMismatch`: `Open` returns it when an existing microsandbox
  carries a different network policy than the host configured, because `msb`
  fixes policy at create time and a silent reattach would run under the old
  rules
- The sandbox lifecycle is journalled: a sandbox that opens for a run writes
  a `RecordSandbox` naming the backend and the sandbox. `bonnie runs show`
  prints it in the timeline. A resumed run whose workspace was pruned gets a
  note in its conversation — never an empty workspace in silence.
  `bonnie sandbox prune [--dry-run]` deletes the sandboxes of terminal runs
- Provider capabilities behind optional interfaces, mirroring Kit's pattern:
  `sandbox.ExistenceChecker` and `sandbox.RunDeleter`. All three built-in
  backends implement both
- The live suites default to `opencode/kimi-k2.5` (`BONNIE_TEST_MODEL`
  still overrides)
- Nix flake: `packages.bonnie`, `packages.microsandbox`, `apps.msb`,
  `overlays.default`, and a `devShells.default` with Go 1.27, `golangci-lint`,
  `goreleaser`, and the microsandbox CLI. The `bonnie` Nix package wraps the
  binary so `msb` is on its PATH
- HTTP channel with six routes:
  - `POST /runs` — start a new run
  - `GET /runs/{id}` — fetch run state
  - `POST /runs/{id}` — send a message to a run
  - `POST /runs/{id}/respond` — answer a suspension
  - `POST /runs/{id}/cancel` — cancel a run
  - `GET /runs/{id}/stream` — NDJSON event stream with cursor support
- `bonnie` CLI with:
  - `serve` — mount the HTTP channel
  - `runs list` — show runs filtered by state
  - `runs show` — inspect run timeline, including the sandbox records
  - `sandbox prune` — reclaim the sandboxes of finished runs
- `AskTool` and `ApprovalTool` for human-in-the-loop workflows
- Live-model integration suites (behind the `integration` build tag), for the
  runtime and the sandbox
- Conformance suites a new implementation joins instead of writing its own
  tests: the journal suite (`memory` + `file`), the sandbox suite (18 cases
  per backend), and the channel suite
- `sandbox` package: isolated tool execution with `Local`, `Docker`, and
  `Microsandbox` backends, all CLI-driven and adding no dependency
- `sandbox.Agent` wires a sandboxed tool set into a `runtime.AgentFactory`;
  the sandbox opens lazily, so a parked run holds no compute
- `bonnie serve --sandbox` selects a backend from the CLI

### Known limits

Stated as plainly as the claims, because a framework that hides its limits
gets deployed into situations it cannot handle.

- Sandboxing is opt-in. Without it, tool calls run as the host process.
- Docker isolates with namespaces, not a guest kernel. Use microsandbox when
  the threat model includes hostile code.
- Sandbox egress is open unless a policy is set.
- microsandbox is verified on Linux with KVM only, not on macOS with Apple
  Silicon, and its network policy is fixed at create time.
- The HTTP channel carries a `Principal` but does not verify it.
- Run ownership is enforced per host with a `flock` per run; a shared network
  filesystem or a second writer still needs one owner in front.
- Events are journal-anchored: a reconnect past the in-memory backlog is
  served from the journal, and Kit's mid-turn deltas stay live-only.
- Reclaiming the sandboxes of finished runs is a command, not a background
  sweep.

---

[0.9.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.9.0
[0.8.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.8.0
[0.7.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.7.0
[0.6.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.6.0
[0.5.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.5.0
[0.4.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.4.0
[0.3.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.3.0
[0.2.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.2.0
[0.1.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.1.0
