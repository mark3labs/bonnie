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
- [ ] `test` and `lint` green on `master` at the tagged commit
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
