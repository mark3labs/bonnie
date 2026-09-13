# Handover — built-in TUI (`bonnie dev` / `bonnie chat`)

T-021 is complete. The work is in the working tree and is ready for final
checks and commit.

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
- Leaked OSC color-query replies are not inserted into the textarea.
- A durable final response replaces its streamed draft, so reconnect does not
  add a second answer.

## Verified

- `go test -race ./cmd/bonnie/...` passes.
- Live tmux typing inserted text and submitted a provider-backed turn.
- A clean live turn rendered one answer and no terminal-control text.
- A watched-file change restarted the child without a forced-kill timeout.
- The TUI reconnected and completed a second turn on the same durable run.

## Repository hygiene

Do not commit the untracked root files `agent.yaml`, `instructions.md`,
`skills/`, `workspace/`, or `.kit.yml`. They are not part of T-021.
