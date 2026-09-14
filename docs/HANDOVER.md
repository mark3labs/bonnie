# Handover — built-in TUI (`bonnie dev` / `bonnie chat`)

T-021 is complete and committed on `master`:

- `9ccfe1f feat(cli): add built-in terminal chat`
- `79329ec fix(tui): isolate terminal input`

All checks and live verification are complete. The only remaining working-tree
items are unrelated root agent files, which are intentionally untracked.

## Delivered

- `bonnie dev` starts the hot-reload child and opens a minimal scrollback TUI.
- `bonnie chat --addr --run` connects the same TUI to any HTTP channel.
- The TUI uses the HTTP wire only: start, send, respond, cancel, and NDJSON
  stream routes.
- Automatic addresses walk `127.0.0.1:8080`, `:8081`, `:8082`, and so on.
  An explicit `--addr` is used as-is.
- A dropped stream reopens from the last durable event cursor.
- Before a dev hot reload stops its child, the TUI closes its HTTP stream. This
  prevents graceful shutdown from waiting on the connection that will reconnect.
- `ctrl+w` calls `POST /runs/{id}/cancel`.
- The textarea is focused in `New`. The old `Init` code focused a copied model,
  which caused the tmux typing defect.
- In TUI mode, the child writes through a non-TTY writer. This prevents child
  terminal-capability queries from sending replies into the TUI input.
- The textarea uses Bubble Tea's real cursor pattern and one visible row, so it
  renders one input prompt.
- A durable final response replaces its streamed draft, so reconnect does not
  add a second answer.

## Verified

- `go test -race ./cmd/bonnie/...` passes.
- Live tmux typing inserted text and submitted a provider-backed turn.
- A clean live turn rendered one answer, one input prompt, and no
  terminal-control text.
- A watched-file change restarted the child without a forced-kill timeout.
- The TUI reconnected and completed a second turn on the same durable run.

## Repository hygiene

The repository root is a Go library, not an agent tree. A `bonnie init .` run
here leaves `instructions.md`, `skills/`, and `workspace/` behind; they are
now in `.gitignore` and must not be committed. Neither must `agent.yaml` —
that file no longer means anything (T-024), and `.kit.yml` is a local tool
configuration.
