# Handover

Where the project stands, what is sharp, and what I would do next.

## State

`master` is green: `task ci` passes (build, vet, vet with the integration
tag, `go test -race ./...`, `golangci-lint`). The last increment was T-024,
which removed the manifest and made configuration code.

| Layer | State |
|---|---|
| L0 `kit/pkg/kit` | upstream, unmodified; pinned `v0.106.0`. No open asks (`docs/UPSTREAM.md`) |
| L1 `runtime/` | done and hardened. Journal, replay, suspend/resume, cancel, steer, events |
| L2 `agent/` + root `bonnie` | done. Default layout, scaffold, codegen, `dev`, `build`. Evals (T-019) are the gap |
| L3 `channel/` | HTTP, Slack, Discord, Telegram. `channeltest` conformance suite |
| L4 CLI | `init`, `dev`, `build`, `serve`, `chat`, `runs`, `sandbox`. Evals planned |

The public entry point is the root package: `bonnie.New(opts...).Serve()` is
a complete agent. A scaffolded `main.go` is that one call.

## The rules that are not obvious

- **Public Kit SDK only** (`AGENTS.md`). The module path makes
  `kit/internal/...` a compile error; the real trap is `charm.land/fantasy`,
  which must stay `// indirect`.
- **One place per setting** (SPEC §8, invariant 14). A setting is a file at a
  fixed path or a Go option, never both. The default paths are constants in
  the root package. The manifest existed to be the second place and was
  removed for it; do not add a config file back. `docs/L2.md` §12 records the
  intended answer to "`main.go` is getting long": slot discovery, not YAML.
- **Refuse what you cannot honour** (invariant 13). A network policy with no
  sandbox, an image on a backend that runs none, an agent option beside
  `WithAgentFactory` — all startup errors naming both sides.
- **An `Option` is a reusable value.** Anything its closure captures is
  read-only; copy before mutating. This bit once already (SPEC §4.13).
- **A guard test's file name is not its scope.** Deleting a feature's test
  file can delete an unrelated invariant's only guard. Before removing a test
  file, grep the docs for the test names inside it (SPEC §4.13).

## Pitfalls that cost time

- **`bonnie.Registered()` at package scope reads an empty tree.** The
  generated file registers from `init`, and Go initialises package-level
  variables first. Read it inside `main` or later.
- **`bonnie dev` links the tree's pinned release, not your working tree.** To
  test a local change through a scaffolded tree, put a `replace` in that
  tree's `go.mod` — `internal/treetest` does exactly this for the tests.
- **No `go.work` anywhere.** `go.mod` pins Kit and the build tests write a
  `replace` into the temporary tree they compile. A workspace file above the
  checkout silently covers every module and can make a green local run lie;
  CI runs `GOWORK=off` to catch it.
- **The dev loop must never watch the workspace.** It is where the model
  writes, so watching it makes the agent restart itself mid-turn (SPEC
  §4.9.1).

## What I would do next

1. **T-019, evals.** The only named gap in L2. Write the spec before any
   code: what a case is, where cases live, how a run is scored, what CI runs
   without a provider key.
2. **Slot discovery for sandbox and channels** (`docs/L2.md` §12). The
   `Register` seam was built so this is additive: `sandbox/sandbox.go`
   exporting `func Sandbox() sandbox.Provider`, `channels/<name>/`, both
   walked by the generator that already walks `tools/`. It is the release
   valve for `main.go` growth, and it keeps configuration as code.
3. **Tag `v0.2.0`.** The `[Unreleased]` section of `CHANGELOG.md` is a
   breaking release: the manifest is gone and `bonnie.New().Serve()` is the
   API. The migration table is written.

## Repository hygiene

The repository root is a Go library, not an agent tree. A `bonnie init .` run
here leaves `instructions.md`, `skills/`, and `workspace/` behind; they are
in `.gitignore` and must not be committed. Neither must `agent.yaml` — that
file no longer means anything (T-024) — and `.kit.yml` is a local tool
configuration.
