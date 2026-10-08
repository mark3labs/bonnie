# Releasing BONNIE

The procedure every tag follows. The code is the spec; this file is the one
procedure that is not in the code. Open work is ad hoc or a GitHub issue.

The three claims a release must state: **survives process death**, **parks
indefinitely**, **reachable over HTTP**. State the limits from the current
`README.md` just as plainly.

## The checklist

1. Run `task release-check` (`goreleaser check`) and `task release-snapshot`
   (`goreleaser build --snapshot --clean`).
2. Confirm every CI job is green on `master` at the commit you will tag.
   That means `test`, `examples`, and `lint`. There is no `boundary` job any more —
   `depguard` in `lint` is the authority.
3. Write the `CHANGELOG.md` section for the version **before** you push the
   tag. State the three claims — survives process death, parks indefinitely,
   reachable over HTTP — and state the limits from the **current**
   `README.md` just as plainly. `release.yml` slices this section out with
   `scripts/release-notes.sh` and gives it to `goreleaser --release-notes`;
   a tag with no matching section fails the workflow.
4. Tag the version and push the tag. `release.yml` fires on `v*`.
5. Check the published artifacts: the downloaded binary must print the
   injected version, not `dev`, its checksum must verify, and it must be
   statically linked.
6. Pin the examples to the new tag, in the commit after the tag:
   `task examples-pin TAG=vX.Y.Z`, then commit `chore: pin examples to
   vX.Y.Z`. Each example is an agent tree whose `go.mod` pins a release, so
   this cannot happen before the tag is on the proxy.
   `TestExamplesGoModIsAUserGoMod` allows a pin one release behind for this
   window, and fails at the next release if the step was forgotten.

### Every box must be confirmed

- [ ] `goreleaser check` passes; the snapshot builds on every target and the
      binary prints the injected version
- [ ] No `replace` directive is committed in `go.mod`
- [ ] `test`, `examples`, and `lint` green on `master` at the tagged commit
- [ ] `CHANGELOG.md` carries the version's section in Keep-a-Changelog shape
- [ ] The release notes state both the claims and the limits
- [ ] Tag pushed and artifacts published
- [ ] A downloaded binary prints the injected version
- [ ] The examples are pinned to the new tag and `task examples` passes

## Watch for

- **Notes written mid-release must be re-checked against the code at tag
  time.** A later commit in the same release can make an earlier entry false,
  and nothing in CI notices.
- **Re-read the commit list, not the section.** No test knows what a release
  forgot to say, so a feature that never reached `CHANGELOG.md` ships unnamed.
  Read `git log <latest-tag>..HEAD` against the notes; `v0.7.0` was 22 commits
  and four of them had added a public surface the section never mentioned.
- **A limit can go stale inside one release.** At `v0.7.0` the README limit
  "the HTTP channel does not verify auth" was falsified by a commit of that
  same increment. Check each limit against the code you are tagging, not
  against the README you remember.
- **Prove a behaviour from the downloaded artifact.** That it builds and
  prints its version says nothing about the claim the release is *for*. At
  `v0.7.0` the published binary was made to refuse `--sandbox none` by name.
- **Do not copy the previous release's limits forward.** That is how a stale
  limit gets published. **Do not drop one silently either:** a previous limit
  that is missing from the README must be proven false before it is left out.
  At `v0.8.0` one had left the README while it still held.
- **A guard pinned to a fixed version is worse than no guard: it reports
  success.** `TestRepositoryChangelogStatesClaimsAndLimits` derives the
  version from the newest released heading for this reason.
- **Raise the golangci-lint pin whenever the `go` line in `go.mod` moves.**
  golangci-lint refuses to load its config when its own toolchain is older
  than the target, and exits 3.
- The sandbox conformance suite **skips** any backend whose runtime is not on
  the machine, and a bare CI runner has neither Docker nor microsandbox. CI
  green does not mean those adapters were exercised. When a backend matters,
  check that it *ran*, not that the suite was green.

## History

`v0.1.0` was tagged at `15e1727` and published on 2026-09-12; `release.yml`
completed successfully. Verified post-publish, not assumed: a downloaded
`linux_amd64` artifact prints `bonnie 0.1.0` (the injected version, not
`dev`), and the GitHub release notes lead with the three claims and the
limits — the auto-generated notes had only the commit list, so the notes were
edited to the required form.

| Version | Commit | Date |
|---|---|---|
| `v0.1.0` | `15e1727` | 2026-09-12 |
| `v0.2.0` | `b1fff6d` | 2026-09-13 |
| `v0.3.0` | `38a511d` | 2026-09-13 |
| `v0.4.0` | `9ccb959` | 2026-09-14 |
| `v0.5.0` | `f9794d1` | 2026-09-15 |
| `v0.6.0` | `09f5293` | 2026-09-15 |
| `v0.7.0` | `4b6fa58` | 2026-09-16 |
| `v0.8.0` | `c03de52` | 2026-09-23 |
| `v0.9.0` | `fb23879` | 2026-09-25 |
| `v0.9.1` | `779289a` | 2026-09-29 |
| `v0.10.0` | `4540d6f` | 2026-10-05 |
| `v0.11.0` | `6ec5953` | 2026-10-06 |
| `v0.12.0` | `dc1ee27` | 2026-10-06 |
| `v0.13.0` | `a5eaa8a` | 2026-10-06 |
| `v0.14.0` | `98b9620` | 2026-10-06 |
| `v0.15.0` | `cd37924` | 2026-10-07 |
| `v0.16.0` | `eac5456` | 2026-10-07 |
| `v0.17.0` | `60b3f7d` | 2026-10-07 |
| `v0.18.0` | `6be6b44` | 2026-10-08 |
| `v0.19.0` | `408c712` | 2026-10-08 |

### `v0.19.0`, 2026-10-08 — publication verified

- [x] `task release-check` and `task release-snapshot` pass at tagged commit
      `408c712`. Both Linux archive checksums verify. The amd64 snapshot
      prints `bonnie 0.18.0-SNAPSHOT-408c712`, not `dev`.
- [x] No `replace` directive in the framework or example modules.
- [x] `test`, `examples`, and `lint` pass on `master` at tagged commit `408c712`
      (CI run `37848180398`). CI excludes the two browser tests by name.
- [x] The dated `[0.19.0]` section groups changes under Added, Changed, and
      Fixed, and states the claims and current limits. Note extraction passes.
      Local `task check` and `task ci` pass with the prepared notes.
- [x] Prepared notes committed and pushed before the tag; CI confirms that commit.
      The `[Unreleased]` heading is removed.
- [x] Annotated tag pushed after human confirmation. Release run `37848921736`
      succeeded. Published notes match the changelog section.
- [x] Three artifacts published. Both downloaded archive checksums verify;
      the statically linked amd64 binary prints `bonnie 0.19.0`.
- [x] Both examples pin v0.19.0 after publication; `task examples` passes.

**Version choice.** MINOR: public agent-command and scoped-runtime APIs,
scheduled host callbacks, tool recovery policies, durable submissions and child
runs, new HTTP routes, and the web interface are added.

**Validation limits.** Local quality checks used cached results for many packages.
CI green does not prove the excluded Chromium live-state and end-to-end flows.
The downloaded amd64 binary exposes `runs inspect` and refuses `--sandbox none`,
naming Landlock and Local as alternatives. The arm64 binary and live-model
behaviour were not executed during release verification. External effects are
not exactly once; use one submission scheduler and process per journal.

### `v0.18.0`, 2026-10-08 — publication verified

- [x] `task release-check` and `task release-snapshot` pass through the Nix shell.
      Both Linux archive checksums verify; the amd64 snapshot prints
      `bonnie 0.17.0-SNAPSHOT-6be6b44`, not `dev`.
- [x] No `replace` directive in the framework or example modules.
- [x] `test`, `examples`, and `lint` pass at tagged commit `6be6b44`
      (CI run `37739773030`). Local `task ci` also passes before the tag.
- [x] The dated `[0.18.0]` changelog section states the three claims, current
      limits, and breaking-change migration. The `[Unreleased]` heading is removed.
- [x] Annotated tag pushed after human confirmation. Release run `37742693557`
      succeeded. Published notes match the changelog section.
- [x] Three artifacts published. Both downloaded archive checksums verify.
      The downloaded amd64 binary prints `bonnie 0.18.0` and is statically linked.
- [x] Both examples pin v0.18.0; `task examples` passes.

**Version choice.** MINOR: public runtime-installation APIs are added, deprecated
workspace APIs are removed, and authored `workspace/` seed trees must move to
`context/`. Storage paths and journal record values stay unchanged.

**Local validation found an old runtime pin.** The Nix shell used microsandbox
v0.6.18, which refused the database migrated by a newer runtime. It now uses
v0.7.7, matching the installer. A test keeps versions and archive hashes aligned.
The database was not reset or deleted; the microsandbox tests then passed.
The arm64 artifact and live-model behaviour were not executed during release
verification. External effects are not exactly once.

### `v0.17.0`, 2026-10-07 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — linux amd64 and arm64; both archive checksums
      verify. The statically linked amd64 binary prints
      `bonnie 0.16.0-SNAPSHOT-60b3f7d`, not `dev`
- [x] No `replace` directive in the framework or example modules
- [x] All CI jobs green on `master` at tagged commit `60b3f7d`:
      `test`, `examples`, and `lint` (run `37675572119`)
- [x] `CHANGELOG.md` has a dated `[0.17.0]` section in Keep-a-Changelog shape;
      note extraction passes and the `[Unreleased]` heading is removed
- [x] Published notes state the three claims and current README limits, including
      schedule delivery and cooperative cancellation. They match the changelog
      section except for trailing blank lines
- [x] Annotated tag pushed after human confirmation; release run `37676166786`
      succeeded
- [x] Three artifacts published; both downloaded archive checksums verify.
      The statically linked downloaded linux amd64 binary prints `bonnie 0.17.0`
- [x] Both examples pin `v0.17.0`; `task examples` and `task check` pass

**Version choice.** MINOR: the clearer context-file, shared-directory, and
sandbox-cleanup names add public APIs. The old names remain supported and
deprecated. Legacy `workspace/` trees remain supported when `context/` is absent.
Generated registration uses `Tree.Workspace` so the current CLI can build trees
that still pin v0.16.0. A nonempty `Tree.ContextFiles` takes precedence at runtime.

**CI blocked the first candidate.** Run `37671203857` failed because generated
`Tree.ContextFiles` wiring did not compile against the examples' v0.16.0 pin.
Compatibility fields, wrappers, constants, and legacy layout fallback restored
that build without a premature pin, a committed replace, or a weaker CI check.

**Verified from the artifact.** The downloaded CLI refuses `--sandbox none` and
names Landlock and Local as alternatives. Compatibility and seeding have automated
tests; this release did not prove them with a live model from the artifact, run
integration-tag live-model tests, or execute the arm64 binary. External effects
and delivery are not exactly once; cancellation is cooperative, not rollback.

### `v0.16.0`, 2026-10-07 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — linux amd64 and arm64, archives and checksums;
      both checksums verify. The statically linked amd64 binary prints
      `bonnie 0.15.0-SNAPSHOT-eac5456`, not `dev`
- [x] No `replace` directive in the framework or example modules
- [x] All CI jobs green on `master` at the tagged commit `eac5456`:
      `test`, `examples`, and `lint` (run `37644593184`)
- [x] `CHANGELOG.md` has a dated `[0.16.0]` section in Keep-a-Changelog shape;
      release-note extraction passes, and the `[Unreleased]` heading is removed
- [x] Published notes state the three claims and current README limits, plus
      schedule delivery and cooperative cancellation limits. The notes match
      the changelog section except for trailing blank lines
- [x] Annotated tag pushed after human confirmation; `release.yml` run
      `37645611447` succeeded
- [x] Three artifacts published; both downloaded archive checksums verify.
      The downloaded linux amd64 binary prints `bonnie 0.16.0` and is
      statically linked
- [x] Both examples pin `v0.16.0`; `task examples` and `task check` pass.
      The pin is committed after the tag as `43458ab`

**Version choice.** MINOR: new public schedule and cancellation APIs, the
`channel.SessionRef.RequestCancel` interface addition, and changed HTTP
cancellation responses. Compiled-agent sandbox selection was already released
in v0.15.0; the notes identify its later documentation, not a new API.

**CI blocked the first candidate.** Run `37642674467` failed the schedule HTTP
restart test. Newly idle keep-alive connections can delay Go HTTP shutdown for
five seconds, which raced the test's five-second deadline. The test now closes
its client's idle connections before shutdown and cleans up the restarted
context on assertion failure. Thirty consecutive race-enabled runs pass. No
runtime shutdown change was needed.

**Verified from the artifact.** The downloaded CLI exposes `schedules` with
`list`, `show`, `history`, and `trigger`, and refuses `--sandbox none` by name,
with Landlock and Local as the alternatives. Schedule recovery and turn-scoped
cancellation have automated tests; this release did not prove them with a live
model from the artifact. It did not run integration-tag live-model tests or
execute the arm64 binary. Delivery and external effects are not exactly once;
cancellation is cooperative, not a rollback or distributed coordination.

### `v0.15.0`, 2026-10-07 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — linux amd64 and arm64, archives and checksums;
      both checksums verify. The statically linked amd64 binary prints
      `bonnie 0.14.0-SNAPSHOT-cd37924`, not `dev`
- [x] No `replace` directive in the framework or example modules
- [x] All CI jobs green on `master` at the tagged commit `cd37924`:
      `test`, `examples`, and `lint` (run `37617863724`)
- [x] `CHANGELOG.md` has a dated `[0.15.0]` section in Keep-a-Changelog shape;
      release-note extraction passes, and the `[Unreleased]` heading is removed
- [x] Published notes state the three claims and the current README limits,
      including targeted delivery and chat retry. The notes match the changelog
      section except for trailing blank lines
- [x] Annotated tag pushed after human confirmation; `release.yml` run
      `37618809204` succeeded
- [x] Three artifacts published; both downloaded archive checksums verify.
      The downloaded linux amd64 binary prints `bonnie 0.15.0` and is
      statically linked
- [x] Both examples pin `v0.15.0`; `task examples` and `task check` pass.
      The pin is committed after the tag as `e8c8f21`

**Version choice.** MINOR: new public NATS configuration fields, `SubmitTo`,
route validation, and chat features. The model-facing sandbox tool changes
from `bash` to `shell`; prompts and scripted calls must use the new name.

**Verified from the artifact.** The downloaded CLI refuses `--sandbox none`
by name and identifies Landlock and Local as the alternatives. Targeted task
routing, cache keys, shell selection, and chat behaviour have automated tests;
this release did not prove them with a live model from the artifact. It did
not run integration-tag live-model tests or execute the arm64 binary. Targeted
tasks have no fallback and remain only within stream retention limits.
External effects are not exactly once, and chat retry can repeat them.

### `v0.14.0`, 2026-10-06 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — linux amd64 and arm64, archives and checksums;
      both checksums verify. The statically linked amd64 binary prints
      `bonnie 0.13.0-SNAPSHOT-98b9620`, not `dev`
- [x] No `replace` directive in the framework or example modules; Kit stays
      at `v0.120.0`
- [x] All CI jobs green on `master` at the tagged commit `98b9620`:
      `test`, `examples`, and `lint` (run `37491892717`)
- [x] `CHANGELOG.md` has a dated `[0.14.0]` section in Keep-a-Changelog shape;
      release-note extraction passes, and the `[Unreleased]` heading is removed
- [x] Published notes state the three claims and the current README limits,
      including status delivery, worker controls, and independent stream ordering.
      The notes match the changelog section except for trailing blank lines
- [x] Annotated tag pushed after human confirmation; `release.yml` run
      `37492722109` succeeded
- [x] Three artifacts published; both downloaded archive checksums verify.
      The downloaded linux amd64 binary prints `bonnie 0.14.0` and is
      statically linked
- [x] Both examples pin `v0.14.0`; `task examples` and `task check` pass.
      The pin is committed after the tag as `bb92c30`

**Version choice.** MINOR: the `feat:` commit adds public NATS channel and
client configuration fields, status and control types, subject and stream
helpers, and client methods for durable statuses, queries, and cancellation.
Existing explicit-subject configurations remain supported.

**Verified from the artifact.** The downloaded CLI refuses `--sandbox none`
by name and identifies Landlock and Local as the alternatives. NATS status and
control behaviour has automated tests; this release did not prove it with a
live model from the artifact. It did not run integration-tag live-model tests
or execute the arm64 binary. Status delivery is at least once. Queries and
cancellation are request/reply, not durable queued commands. Workers do not
share run state, and cancellation does not undo external effects.

### `v0.13.0`, 2026-10-06 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — linux amd64 and arm64, archives and checksums;
      both checksums verify. The statically linked amd64 binary prints
      `bonnie 0.12.0-SNAPSHOT-a5eaa8a`, not `dev`
- [x] No `replace` directive in the framework or example modules; Kit stays
      at `v0.120.0`
- [x] All CI jobs green on `master` at the tagged commit `a5eaa8a`:
      `test`, `examples`, and `lint` (run `37472122552`)
- [x] `CHANGELOG.md` has a dated `[0.13.0]` section in Keep-a-Changelog shape;
      release-note extraction passes, and the `[Unreleased]` heading is removed
- [x] Published notes state the three claims and the current README limits,
      including cleanup, shared workspaces, completion recovery, and logging.
      The notes match the changelog section except for trailing blank lines
- [x] Annotated tag pushed after human confirmation; `release.yml` run
      `37472846758` succeeded
- [x] Three artifacts published; both downloaded archive checksums verify.
      The downloaded linux amd64 binary prints `bonnie 0.13.0` and is
      statically linked
- [x] Both examples pin `v0.13.0`; `task examples` and `task check` pass.
      The pin is committed after the tag as `2d43f5a`

**Version choice.** MINOR: five `feat:` commits add managed Kit setup and
completion checks, image reads, workspace policies, optional human-input tools,
and activity logging. Public additions reach the root, runtime, and sandbox
packages. The README's manual-only cleanup limit was corrected before the tag.

**Verified from the artifact.** The downloaded CLI refuses `--sandbox none`
by name and identifies Landlock and Local as the alternatives. Image replay,
completion recovery, workspace cleanup, and shared workspace behaviour have
automated tests; this release did not prove them with a live model from the
artifact. It did not run integration-tag live-model tests or execute the arm64
binary. Completion checks can repeat after interruption, and cleanup and shared
workspace locks do not coordinate separate processes. External effects are not
exactly once.

### `v0.12.0`, 2026-10-06 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — linux amd64 and arm64, archives and checksums;
      both checksums verify, and the statically linked amd64 binary prints
      `bonnie 0.11.0-SNAPSHOT-dc1ee27`, not `dev`
- [x] No `replace` directive in the framework or example modules; Kit stays
      at `v0.120.0`
- [x] All CI jobs green on `master` at the tagged commit `dc1ee27`:
      `test`, `examples`, and `lint` (run `37451169148`)
- [x] `CHANGELOG.md` has a dated `[0.12.0]` section in Keep-a-Changelog shape;
      release-note extraction passes. The published 0.11.0 section matches
      its tagged text; the later authentication entries belong to 0.12.0
- [x] Published notes state the three claims and the current README limits,
      including authentication, NATS delivery, and worker-state limits.
      The notes match the changelog section except for trailing blank lines
- [x] Annotated tag pushed after human confirmation; `release.yml` run
      `37451844000` succeeded
- [x] Three artifacts published; both downloaded archive checksums verify.
      The downloaded linux amd64 binary prints `bonnie 0.12.0` and is
      statically linked
- [x] Both examples pin `v0.12.0`; `task examples` and `task check` pass.
      The pin is committed after the tag as `8cf6eb3`

**Version choice.** MINOR: two `feat:` commits add the public NATS configuration
fields `NKeySeed`, `Token`, `Username`, and `Password`, with environment
fallbacks in `bonnie.WithNATS`.

**Verified from the artifact.** The downloaded CLI refuses `--sandbox none`
by name and identifies Landlock and Local as the alternatives. Authentication
is covered by automated real-broker tests, not a live-model run from the
artifact. This release did not run the integration-tag live-model tests or
execute the arm64 binary. Authentication does not make Core NATS durable or
JetStream execution exactly once, and workers still do not share run state.

### `v0.11.0`, 2026-10-06 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — linux amd64 and arm64, archives and checksums;
      the amd64 binary prints `bonnie 0.10.0-SNAPSHOT-6ec5953`, not `dev`
- [x] No `replace` directive in the framework or example modules; Kit stays
      at `v0.120.0`
- [x] All CI jobs green on `master` at the tagged commit `6ec5953`:
      `test`, `examples`, and `lint` (run `37444905068`)
- [x] `CHANGELOG.md` has a dated `[0.11.0]` section in Keep-a-Changelog shape;
      `task release-notes TAG=v0.11.0` passes
- [x] Published notes state the three claims and the current README limits,
      including NATS delivery and worker-state limits. The notes match the
      changelog section except for trailing blank lines
- [x] Annotated tag pushed; `release.yml` run `37445426894` succeeded
- [x] Three artifacts published; both downloaded archive checksums verify.
      The downloaded linux amd64 binary prints `bonnie 0.11.0` and is
      statically linked
- [x] Both examples pin `v0.11.0`; `task examples` and `task check` pass.
      The pin is committed after the tag as `38b6ebb`

**Version choice.** MINOR: two `feat:` commits add Core NATS and JetStream
channels, `bonnie.WithNATS`, `channel.Lifecycle`, public task and result
wire types, a typed `client/nats`, and stable consumer-name helpers.

**Verified from the artifact.** The downloaded CLI refuses `--sandbox none`
by name and identifies Landlock and Local as the alternatives. The real-broker
NATS tests ran in the local quality checks. This release did not run the
integration-tag live-model tests or prove NATS execution from the downloaded
CLI. JetStream delivery is at least once, not exactly once; another worker
can execute a task again, and a waiting run needs its original worker state.

### `v0.10.0`, 2026-10-05 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — linux amd64 and arm64, archives and checksums;
      the amd64 binary prints `bonnie 0.9.1-SNAPSHOT-4540d6f`, not `dev`
- [x] No `replace` directive in the framework or example modules; Kit is
      `v0.120.0` and SQLite is `v1.60.1`
- [x] All CI jobs green on `master` at the tagged commit `4540d6f`:
      `test`, `examples`, and `lint` (run `37321446620`)
- [x] `CHANGELOG.md` has a dated `[0.10.0]` section in Keep-a-Changelog shape
- [x] Published notes state the three claims and the current README limits,
      with no human edit; they also state the experimental status and do not
      promise exactly-once external side effects
- [x] Annotated tag pushed; `release.yml` run `37322130456` succeeded
- [x] Three artifacts published; both archive checksums verify. The downloaded
      linux amd64 binary prints `bonnie 0.10.0` and is statically linked
- [x] Both examples pin `v0.10.0`; `task examples` and `task check` pass.
      The pin is committed after the tag as `6a983b0`

**Version choice.** MINOR despite the `fix:` prefix: the release adds the HTTP
snapshot endpoint, `http.SnapshotResponse`, `client.Snapshot` and
`Client.Snapshot`, `runtime.FileAgent` and two public errors, and
`channel.AddressMap.UnbindRun`. The notes name these public additions.

**Verified from the artifact.** The downloaded CLI refuses `--sandbox none`
by name and identifies Landlock and Local as the alternatives. The published
notes match the changelog section except for trailing blank lines. Snapshot
and recovery behaviour is covered by the automated tests; this release did
not use a live model to prove those behaviours from the downloaded CLI.

### `v0.9.1`, 2026-09-29 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — two targets (linux amd64, arm64), archives
      and checksums; the snapshot binary prints its injected version
- [x] No `replace` directive in `go.mod`; Kit moves to `v0.114.0`
- [x] All CI jobs green on `master` at the tagged commit `779289a`:
      `test`, `examples`, and `lint` (run `36572444674`)
- [x] `CHANGELOG.md` carries a `[0.9.1]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits, published
      with no human edit
- [x] Tag pushed; `release.yml` run `36572985707` succeeded
- [x] Three artifacts published; the downloaded `linux_amd64` binary prints
      `bonnie 0.9.1`, its checksum verifies, and it is statically linked
- [x] The examples are pinned to `v0.9.1` and `task examples` passes

**The section written across the increment had no claims and no limits, for
the fourth release in a row**, and no entry for the Kit `v0.114.0` floor that
the dependency update imposes on every user. Both were added. Each limit was
checked against Kit `v0.114.0`, not carried forward: the `ToolOutput.Halt`
godoc still says a sibling call in the halting step runs, the skill
activation still gives the host `BaseDir`, and `kit.SessionManager` still has
20 methods. The README limits and the `0.9.0` limits match in both
directions.

**Verified the docker fix where it runs.** CI has no Docker, so
`TestDockerStopIsPrompt` skips there. It was run on a host with Docker
29.8.0 before the tag: `Stop` took 0.49 s, against the 10 s grace period
before. From the artifact: the downloaded binary still refuses
`--sandbox none` by name, and `install.sh` from `master` resolved `v0.9.1` as
the latest release, verified the checksum, and installed a binary that
prints `bonnie 0.9.1`.

**Version choice.** PATCH: one `fix:` and one `chore:`, and no exported
signature changed. The Kit floor moves every user's module up, which at
`v0.9.0` was part of the case for a MINOR; there it came with a behaviour
break in `bonnie.New()`. Here Kit `v0.114.0` changes no BONNIE behaviour a
host relies on — the `read` tool's media result is kept whole by the
journal (`TestReplayPreservesMediaToolResults`) — so the floor alone did not
force a MINOR.

### `v0.9.0`, 2026-09-25 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — two targets (linux amd64, arm64), archives
      and checksums; the snapshot binary prints its injected version
- [x] No `replace` directive in `go.mod`; Kit moves to `v0.113.3`
- [x] All CI jobs green on `master` at the tagged commit `fb23879`:
      `test`, `examples`, and `lint` (run `36138369651`)
- [x] `CHANGELOG.md` carries a `[0.9.0]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits, published
      with no human edit
- [x] Tag pushed; `release.yml` run `36138865116` succeeded
- [x] Three artifacts published; the downloaded `linux_amd64` binary prints
      `bonnie 0.9.0`, its checksum verifies, and it is statically linked
- [x] The examples are pinned to `v0.9.0` and `task examples` passes

**The section written across the increment again had no claims and no
limits.** Reading `git log v0.8.0..HEAD` against it found two more gaps: the
`task examples-pin` fix had no *Fixed* entry, and the change to Kit's
discovery defaults was under *Security* only. That change breaks a host that
relied on `AGENTS.md`, named agents, or extensions, and nothing fails to
compile, so it also got a **Breaking** entry under *Changed*, with the Kit
`v0.113.3` minimum.

**A limit was in the README but not in its Limits section.** The approval
section said that a tool called in the same step as a halting tool still
runs. That is a limit of the release's headline fix, so it went into
`README.md` **Limits** and into the notes. No BONNIE test proves it; Kit's
`ToolOutput.Halt` godoc does. The skill bundled-files limit was checked
against Kit `v0.113.3` directly (the activation still gives the host
`BaseDir`) and still holds.

**Verified from the artifact.** The headline fixes live in `bonnie.New()` and
`sandbox.Agent`, and to prove them from the binary needs a live model. Their
proof is `runtime/kit_seams_test.go` and `sandbox/kit_discovery_test.go`,
which drive a real `*kit.Kit` through `internal/fakemodel`, and both halt
tests fail on Kit `v0.113.1`. From the artifact: the downloaded binary still
refuses `--sandbox none` by name, and `install.sh` from `master` resolved
`v0.9.0` as the latest release, verified the checksum, and installed a binary
that prints `bonnie 0.9.0`.

**Version choice.** MINOR, although every commit is `fix:`, `docs:`, or
`chore:` and no exported signature changed. The default of `bonnie.New()`
changed in a way that a host sees only at run time, and the Kit floor moves
every user's module up. A behaviour break with no compile error is the worst
kind to put in a PATCH.

### `v0.8.0`, 2026-09-23 — every box confirmed

- [x] `task release-check` — 1 config validated
- [x] `task release-snapshot` — two targets (linux amd64, arm64), archives
      and checksums; the snapshot binary prints its injected version
- [x] No `replace` directive in `go.mod`; Kit moves to `v0.110.0`
- [x] All CI jobs green on `master` at the tagged commit `c03de52`:
      `test`, `examples`, and `lint` (run `35858048401`)
- [x] `CHANGELOG.md` carries a `[0.8.0]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits, published
      with no human edit
- [x] Tag pushed; `release.yml` run `35858568031` succeeded
- [x] Three artifacts published; the downloaded `linux_amd64` binary prints
      `bonnie 0.8.0`, its checksum verifies, and it is statically linked
- [x] The examples are pinned to `v0.8.0` and `task examples` passes

**The section written across the increment had no claims, no limits, and a
non-standard `### Breaking changes` heading**, folded into *Changed*. Reading
`git log v0.7.0..HEAD` against it found three public surfaces never named —
`chat.Delivery` with `chat.BearerHeader`, `channel.ErrUnverifiedWebhook`, and
the wire code `conversation_corrupt` — and two removals with no *Removed*
entry: `go run ./examples/...` and `bonnie` on `aarch64-darwin` in the flake.

**A limit can drop out as well as go stale.** "A skill's bundled files stay on
the host" was in the `0.7.0` notes, had left `README.md`, and still holds (the
`WithSkills` godoc says so). Taking the limits from the current README alone
would have published without it. It was restored to `README.md` first. So
compare the previous release's limits with the current README in both
directions: a limit in the README must still be true, and a previous limit
that is not in the README must be false.

**Verified from the artifact.** The headline change, the chat adapter's
refusal to build without a webhook credential, lives in a tree's `WithSlack`
and friends, which the CLI binary does not mount — so it cannot be proven from
the artifact, and `TestNewRefuses*` in each adapter is its proof. The release's
new artifact-facing surface is `install.sh`, so it was run from `master`
against the published release: it verified the checksum, installed, and the
installed binary prints `bonnie 0.8.0`. The downloaded binary still refuses
`--sandbox none` by name.

**Version choice.** MINOR: `fix!` changed the exported signatures of
`slack.New` and `telegram.New`, and `feat:` added `install.sh`.

**`task examples-pin` had never run, and it could not pass.** It changed
`go.mod` and `go.sum` and then called `examples`, whose `git diff
--exit-code -- examples/` then failed on the pin itself. The pin was correct and
the trees built. The task now stages the pin before it calls `examples`, so the
check sees only what `bonnie build` changes.

### `v0.7.0`, 2026-09-16 — every box confirmed

- [x] `task release-check` — goreleaser 2.17.1, 1 config validated
- [x] `task release-snapshot` — **two** targets, archives and checksums. Two
      and not four is correct here: this is the release that makes BONNIE
      Linux-only
- [x] No `replace` directive in `go.mod`; Kit stays `v0.106.0`
- [x] All CI jobs green on `master` at the tagged commit `4b6fa58`:
      `test` and `lint` (run `35110883200`)
- [x] `CHANGELOG.md` carries a `[0.7.0]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits, published
      with no human edit (the third tag with automatic notes)
- [x] Tag pushed; `release.yml` run `35111993043` succeeded
- [x] Three artifacts published; the downloaded `linux_amd64` binary prints
      `bonnie 0.7.0`, its checksum verifies, and it is statically linked

**Re-checking the notes at tag time found six defects**, the largest count so
far, and the reason is worth recording: this increment ran to 22 commits. Four
features were in the tree and not in the notes — the chat activity indicator
(`chat.WithActivity`, Slack `Config.Activity`), `sandbox.EnvInjected` with
`bonnie.WithSandboxEnv`, `github.Config.OnComment`, and the label-triggered
coding run with its checkout descriptor, which also **changes behaviour** by
firing `OnIssue` and `OnPullRequest` on every action. The
`fix!: authenticate in Routes, not in the mux wrapper` entry was missing from
*Security* and the example swap from *Removed*. The section also carried two
separate `### Added` headings and no claims at all.

**The guard catches one defect of the six.** `TestRepositoryChangelogStates
ClaimsAndLimits` would have failed on the missing claims and passed over the
four unrecorded features, because no test knows what a release forgot to say.
The longer the increment, the more the notes depend on reading
`git log <tag>..HEAD` against the notes line by line. **A long increment needs
the commit list read, not the section re-read.**

**Two stale limits were corrected rather than published.** `README.md` still
said "The HTTP channel does not verify auth" after `http.WithAuthenticator`
landed *in this same increment* — the limit went stale between two commits of
one release, which is faster than the warning above anticipates — and
`AGENTS.md` still listed `examples/minimal` and `examples/hitl-restart`, both
deleted. The `0.7.0` limits were then taken from the corrected `README.md`.

**Verified from the artifact, not from the source tree.** The headline of this
release is that a tool call cannot escape its workspace, so the downloaded
binary was made to prove a *behaviour*: `bonnie serve --sandbox none` is
refused by name, with the message naming `--sandbox landlock` and
`--sandbox local`. A snapshot that builds says nothing about whether the
sandbox floor holds.

**Version choice.** MINOR, and not a close call: three commits are `feat!` or
`fix!`, and exported signatures moved in all of `runtime/`, `channel/`, and
`sandbox/` — `InputResponse.Approved` became `*bool`, every `sandbox.Provider`
consumer now gets a backend it did not ask for, and `channel.RouteHandler` is
new.

### `v0.6.0`, 2026-09-15 — every box confirmed

- [x] `task release-check` — goreleaser 2.17.1, 1 config validated
- [x] `task release-snapshot` — four targets, archives and checksums
- [x] All CI jobs green on `master` at the tagged commit `09f5293`:
      `test` and `lint` (run `34972788215`)
- [x] `CHANGELOG.md` carries a `[0.6.0]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits, published
      with no human edit (the second tag with automatic notes)
- [x] Tag pushed; `release.yml` run `34973928983` succeeded
- [x] Five artifacts published; the downloaded `linux_amd64` binary prints
      `bonnie 0.6.0`, its checksum verifies, and it is statically linked

**The guard added at `v0.5.0` was itself broken, and this release caught it.**
`TestRepositoryChangelogStatesClaimsAndLimits` named `v0.5.0` literally, so it
would have inspected an already-shipped section forever and passed while a new
release went out with no claims and no limits — the exact failure the
automatic notes exist to prevent. It now derives the version from the newest
released heading in
`CHANGELOG.md`. **A guard pinned to a fixed version is worse than no guard: it
reports success.** Verified by removing one claim from the `0.6.0` section and
watching it fail by name.

**Version choice.** Two commits, prefixed `fix:` and `docs:`, which reads as a
PATCH. It was cut as a MINOR because the release **adds a public endpoint**,
`POST /bonnie/v1/addresses/{address}`: the wire API under `/bonnie/v1` is a
public contract, and a changelog with an *Added* section is not a patch. The
exported `cmd/bonnie/tui.Client` interface also gained a method, which breaks
any out-of-tree implementer. No exported signature in `runtime/`, `channel/`,
or `sandbox/` changed, so the rule in the checklist did not force this — the
public addition did.

### `v0.5.0`, 2026-09-15 — every box confirmed

- [x] `task release-check` — goreleaser 2.17.1, 1 config validated
- [x] `task release-snapshot` — four targets, archives and checksums
- [x] All CI jobs green on `master` at the tagged commit `f9794d1`:
      `test` and `lint` (run `34968133708`). There is no `boundary` job to
      confirm any more — see the note above
- [x] `CHANGELOG.md` carries a `[0.5.0]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits — in the tag
      annotation and, **for the first time with no human edit**, on the
      GitHub release, because the notes extractor landed in this release
- [x] Tag pushed; `release.yml` run `34968699731` succeeded
- [x] Five artifacts published; the downloaded `linux_amd64` binary prints
      `bonnie 0.5.0` (the injected version, not `dev`), its checksum
      verifies, and it is statically linked

**Re-checking the notes at tag time caught three defects**, exactly as the
`v0.4.0` entry below warns. The section written across the increment had two
separate `### Changed` headings, omitted `bonnie.WithName` from *Added*
though it is a new exported option, and carried neither the claims nor the
limits. The limits were then taken from the current `README.md` rather than
copied from the `0.4.0` section, which still describes the `flock` ownership
model that the SQLite journal replaced. **Copying the previous release's
limits forward is how a stale limit gets published.**

### `v0.4.0`, 2026-09-14 — every box confirmed

- [x] `task release-check` — goreleaser 2.17.1, 1 config validated
- [x] `task release-snapshot` — four targets, archives and checksums, 2m16s
- [x] All CI jobs green on `master` at the tagged commit, including
      `boundary` (run `34862857167`)
- [x] `CHANGELOG.md` carries a `[0.4.0]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits — in the tag
      annotation and, after an edit, on the GitHub release
- [x] Tag pushed; `release.yml` run `34863448503` succeeded in 5m20s
- [x] Five artifacts published; a downloaded binary prints `bonnie 0.4.0`,
      its checksum verifies, and it serves

**`0.4.0` had been written into `CHANGELOG.md` and never tagged.** Four more
commits then accumulated under `[Unreleased]`, so the two sections were merged
into one `0.4.0` rather than tagging `v0.5.0` and leaving a changelog heading
no tag would ever match. Merging found three claims that intra-release churn
had made false — `Manifest.WorkspaceDir` under *Added* after the manifest was
deleted, the seeding fix described through a manifest key that no longer
exists, and two new exported options missing from the list. **Notes written
mid-release must be re-checked against the code at tag time**; a later commit
in the same release can invalidate an earlier entry, and nothing in CI
notices.

**`goreleaser` does not read `CHANGELOG.md` — fixed in `v0.5.0`.**
`.goreleaser.yaml` builds the body from commit subjects, so the published
notes were a commit list until someone replaced them. This cost an edit at
`v0.1.0` and again at `v0.4.0`. It is closed now: `release.yml` slices the
tag's section out of `CHANGELOG.md` with `scripts/release-notes.sh` and passes
it to `goreleaser --release-notes`, and a tag with no matching section fails
the workflow instead of publishing an empty body. **Step 3 above is no longer
manual** — but the section must exist before the tag is pushed.

**The `boundary` CI job no longer exists.** `depguard` in
the `lint` job is the authority and denies both forbidden paths by prefix.
