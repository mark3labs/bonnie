---
title: Testing
description: BONNIE contributor commands, hermetic model tests, process-boundary recovery tests, conformance suites, and CI rules.
---

# Testing

Tests must prove the behavior claimed by godoc. Read the code, API comments, and existing tests before changing an interface or recovery path. Correct an inaccurate comment with the code change. Add or update tests even when a task does not explicitly request them.

The standard suite must not call a live model. Use deterministic agents or a real Kit with a scripted provider. See [Runtime](/reference/runtime), [Journal](/reference/journal), and [Events](/reference/events) for the contracts to test.

## Contributor setup

Use Linux and Go 1.27. Work against the Kit version pinned in `go.mod`. Nix users can run `direnv allow` or `nix develop` for the project tools. Use `task` commands from `Taskfile.yml`.

```bash
task build
task install
task check
```

`task build` writes `output/bonnie`; `task install` installs the CLI in the Go binary directory. `task dev -- serve --journal .bonnie` builds and runs the CLI; it is distinct from the CLI's `bonnie dev` tree watcher.

A local Kit HEAD experiment can use a temporary replacement:

```bash
go mod edit -replace github.com/mark3labs/kit=../kit
# Run the relevant local tests.
go mod edit -dropreplace github.com/mark3labs/kit
```

Never commit a replace directive. Final checks must use the pinned dependency a user will receive. `task tidy` updates the root module; inspect its changes.

## Quality commands

| Command | Purpose |
| --- | --- |
| `task check` | Format check, lint, vet, and race tests |
| `task ci` | Full local CI checks, including tagged vet, CGO-free build, lint, and example builds |
| `go build ./...` | Build root-module packages |
| `go test -race ./...` | Standard tests with race detection |
| `go test -race ./runtime -run TestResumeAcrossProcessBoundary` | Focused durability test |
| `go vet ./...` | Static checks |
| `go vet -tags integration ./...` | Check integration-tagged code without model calls |
| `golangci-lint run ./...` | Lint, including dependency boundary checks |
| `go fmt ./...` | Format root-module Go packages |
| `CGO_ENABLED=0 go build ./...` | Prove the pure-Go/static build boundary |
| `task test-cover` | Write coverage data and HTML under `output/` |

`task test-pkg -- ./runtime` runs a package with verbose race tests. `task test-run -- TestResumeAcrossProcessBoundary` selects a test name across packages. Correct test, lint, and type errors before completing a change.

Run `task check` before each commit and `task ci` before pushing. CI has test, lint, and examples jobs. The test job builds, vets, runs race tests, and builds again without CGO. The examples job builds each tree against its released pin. Live providers are not required for ordinary CI.

## Avoid the module cache in file checks

`.envrc` places GOPATH under `.direnv/`, inside the checkout. Do not run a raw recursive file walk such as `gofmt -l .`, bare `find`, or `grep -r` over the whole repository. That includes downloaded modules.

Go's `./...` skips dot directories. For other file operations, ask Git:

```bash
git ls-files --cached --others --exclude-standard '*.go'
```

This includes new unstaged files and respects `.gitignore`. Filter deleted paths before passing the list to a formatter. The `task fmt-check` task already does this.

## Executor tests without Kit

Use `fakeAgent` and the factories in `runtime/runner_test.go` to test state transitions, controls, failures, and journal behavior. These doubles implement the small `runtime.Agent` interface and do not need credentials.

Use `t.Parallel()` by default, `t.TempDir()` for storage, and `t.Cleanup()` to release resources. Coordinate concurrency with channels rather than relying on sleep timing. Give blocking assertions a deadline so a defect cannot hang the suite. Do not mutate process-wide state in parallel tests.

Test both the returned outcome and durable records. A correct response with the wrong journal is not a successful durability test. Test sentinel errors with `errors.Is` rather than error text alone.

## Real Kit, scripted model

Use `internal/fakemodel` when the test must prove what Kit actually does with options, hooks, tool calls, or provider requests.

```go
model := fakemodel.New(
    fakemodel.Call("ask_human", `{"question":"Which region?"}`),
    fakemodel.Say("Use eu-west-1."),
)
providerOption := model.Option()
```

The option registers a real Kit provider whose model answers from a script. Recorded requests let tests inspect the exact restored context sent to a provider. Exhausting the script returns `ErrScriptExhausted`, which can expose an unexpected extra model call.

Make the Kit hermetic unless discovery is the behavior under test. Disable user configuration, context files, automatic skills, extensions, agents, and host core tools as appropriate. The `hermetic()` helper in `runtime/kit_seams_test.go` sets `SkipConfig`, `NoContextFiles`, `NoSkills`, `NoExtensions`, `NoAgents`, `DisableCoreTools`, and `Quiet`. Do not let a contributor's home config or nearby `AGENTS.md` alter a standard test.

Useful test files:

| File | What it proves |
| --- | --- |
| `runtime/kit_seams_test.go` | Real Kit suspension, atomic steps, context injection, and approval sequencing |
| `runtime/kit_setup_test.go` | Managed setup on a real Kit |
| `runtime/completion_seams_test.go` | Completion behavior at the Kit boundary |
| `sandbox/kit_discovery_test.go` | Managed discovery and sandbox tool selection |
| `runtime/replay_fidelity_test.go` | Typed calls, results, media, and other message content survive restore |

`TestKitApprovalBlocksTheActionUntilAnswered` and `TestKitSuspendAndResumeAcrossProcessBoundary` require exactly one model call in the halting turn. They protect the Kit v0.113.3 halt behavior. Do not weaken them or lower the pin to hide a regression.

## Prove recovery across an ownership boundary

Every durability claim needs a second Runner that shares only journal data. Prefer closing the first SQLite handle and reopening the same directory for the second execution. Do not preserve the first session, agent instance, active map, event backlog, or callback closure to make recovery pass.

A useful sequence is:

1. Start work through Runner A and save a completed step, suspension, or interrupted orchestration phase.
2. Close the first journal and discard local executor state.
3. Open the same directory and construct Runner B with a fresh factory.
4. Resume or Start through B, as appropriate.
5. Inspect the result, restored provider request, and records. Assert that committed tool IDs/results survived and incomplete work is handled explicitly.

`TestResumeAcrossProcessBoundary` is the basic pattern. `TestKitSuspendAndResumeAcrossProcessBoundary` proves that a real provider request retains matching tool call and result IDs. Some lifecycle and dev tests use child processes to test the actual serving boundary.

Cover failure intervals, not only clean restarts:

- Atomic step batches must be all-or-nothing and survive cancellation after the work finishes.
- Plain journals without `StepJournal` still need tail repair; mid-history corruption must fail.
- Cancellation must be saved before acknowledgement. A saved command must withdraw input after restart.
- Completion feedback, continuation charges, and accepted candidates must recover without resetting the budget. Hook effects can repeat.
- Event recovery must work after backlog eviction and restart. Unsubscribe must release blocked goroutines.
- Cleanup must retain waiting runs, record successful deletion, and retry deletion/write failures.

Do not claim exactly-once effects from a test that proves only lossless replay. A tool can perform an external effect before the journal records its result.

## Conformance suites

| Area | Suite | Extension rule |
| --- | --- | --- |
| Journals | `runtime/journal_conformance_test.go` | Add a journal factory and reopen function; test ordering, unknown runs, state, persistence, and concurrency |
| Channels | `channeltest.RunConformance` | Supply a fresh fixture with a journal and scripted agent |
| Sandboxes | Tests in `sandbox/` | Follow existing backend and capability conformance patterns |

The channel suite tests the inbound contract without opening a port or using platform credentials: address resolution, attachment, origin, turn policies, suspension, and cancellation. Declare genuinely unsupported optional capabilities in `Fixture.Unsupported`. Do not skip mandatory behavior to make a backend pass. See [Channels](/channels/overview).

For HTTP host tests, bind a listener on `:0` and supply `WithListener`; learn the assigned port rather than assuming 8080 is free. Use fake platform HTTP endpoints for signature and delivery tests.

## Live-model tests

```bash
task test-live
# Equivalent:
go test -race -tags integration ./runtime ./sandbox
```

Live tests require a provider key such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or `GEMINI_API_KEY`. They must skip cleanly without a supported key, not fail. With a key they can make billable requests. Keep credentials out of source, test output, journals shared publicly, and sandbox environment unless deliberately required.

Integration tags can also include backend or broker prerequisites. Read the test's skip conditions. A skipped live test does not replace an offline regression test for deterministic runtime behavior.

## Example agent trees

Each directory under `examples/` is an independent tree with its own module, instructions, main, generated wiring, skills, context files, and released BONNIE pin. `./...` does not directly traverse these nested modules.

`examples/examples_test.go` checks their structure and compiles them against the checkout. `task examples` separately builds them as a user does, with `bonnie build`, `GOWORK=off`, and released dependencies, then checks for generated-file changes.

Use `task examples-gen` when generated wiring must change. After a release is tagged and available, use `task examples-pin TAG=vX.Y.Z` as part of the release procedure. These tasks change files; inspect and commit the intended changes. Do not weaken example tests, add committed replacements, or move prompt/skill/context configuration into `main.go` to bypass the tree contract.

For an authored tree, run its own `go test ./...`, then `bonnie build --dry-run` and `bonnie build`. See [Agent Trees](/guides/agent-trees) and [Tools and Skills](/guides/tools-and-skills).

## Public SDK boundary

Shipped BONNIE code imports Kit only through `github.com/mark3labs/kit/pkg/kit`. Never import `kit/internal/...`, including in tests. Never vendor Kit internals or copy their implementation.

Test-only `_test.go` files and `internal/fakemodel` may import `charm.land/fantasy` to implement a scripted language model. Prefer fakemodel so test APIs name Kit types. Shipped code must not import fantasy or fakemodel.

The compiler and checkout extension provide additional checks, but `depguard` in `.golangci.yml` is the authority. Its rules check every change, including edits outside Kit. Keep them. If a public helper is missing, first check the SDK again, then request the export upstream. Only then add a justified workaround marked `TODO(kit)`.

## Contribution checklist

- Branch from `master` and keep the change focused.
- Read existing godoc and test comments; update inaccurate comments with the change.
- Use standard, third-party, then local import groups; add godoc for every exported symbol.
- Check errors and wrap them with useful `bonnie:` context. Use context as the first blocking-operation argument.
- Run `task check` before a commit and `task ci` before push. Inspect the diff for generated output, accidental secrets, and module replacements.
- Use conventional commits such as `feat:`, `fix:`, `docs:`, `test:`, `chore:`, or `refactor:`; use `!` for breaking changes. Explain what changed and why.
- Keep `CHANGELOG.md` current when the change needs a release note and complete the pull-request template.

For deployment checks beyond unit tests, see [Deployment](/guides/deployment) and [Troubleshooting](/guides/troubleshooting).
