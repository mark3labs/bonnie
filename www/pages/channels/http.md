---
title: HTTP channel
description: Start, inspect, answer, and control durable runs through the BONNIE v1 HTTP API.
---

# HTTP channel

HTTP is the default channel for agent trees and the `bonnie serve` command. It accepts JSON and streams newline-delimited JSON (NDJSON), not server-sent events.

```sh
bonnie serve --addr 127.0.0.1:8080 --journal .bonnie \
  --model anthropic/claude-sonnet-4-5
curl -s http://127.0.0.1:8080/bonnie/v1/health
# {"ok":true,"status":"ready"}
```

Configure the provider key before starting. Health is liveness only: it does not read the journal or check model access.

## Routes

All paths below start with `/bonnie/v1`.

| Method | Path | Effect |
| --- | --- | --- |
| GET | `/health` | Public liveness |
| GET | `/info` | `agent` (optional), `version`, and mounted `channels` |
| POST | `/runs` | New run, or message through an address |
| GET | `/addresses/{address}` | Look up a binding; unknown address is 404 |
| POST | `/addresses/{address}` | Ensure a binding and pending run, without a model turn |
| GET | `/runs/{id}` | Durable state and latest boundary result |
| GET | `/runs/{id}/snapshot` | Conversation, state, and cursor from one replay |
| POST | `/runs/{id}` | Message to an exact existing run |
| POST | `/runs/{id}/respond` | Answer a waiting run |
| POST | `/runs/{id}/cancel` | Durable cancellation request |
| POST | `/runs/{id}/reset` | Retire the run and free its HTTP addresses |
| POST | `/runs/{id}/clear` | Remove model conversation context, keep run and files |
| POST | `/runs/{id}/compact` | Summarize older model context |
| GET | `/runs/{id}/stream` | NDJSON events; optional `?cursor=N` |

ID-addressed message, read, and answer routes never create a run. Reserved internal runs are not addressable. Cancel on an unknown or idle run is a harmless `not_active` result. Reset, clear, and compact return 204 on success.

## Request fields

`POST /runs` accepts:

| JSON field | Type | Meaning |
| --- | --- | --- |
| `text` | string | User input |
| `address` | string, optional | HTTP-local conversation key; create on first use |
| `operation_id` | string, optional | Authenticated start idempotency key; cannot accompany `address` |
| `title` | string, optional | First-turn listing title |
| `kind` | string, optional | First-turn surface kind, such as `dm` or `thread` |
| `context` | array of strings, optional | Input for this turn only, separate from history |
| `turn_policy` | string, optional | `steer` (default) or `queue` |
| `auth` | principal object, optional | Self-asserted identity only when no authenticator is configured |

`POST /runs/{id}` accepts only `text`, `context`, `turn_policy`, and `auth` from this table. Neither request type has a file-upload field. JSON body decoding is limited to 1 MiB. The run API decoder is not a strict schema validator: do not use acceptance of an unknown field as proof that it has an effect.

`channel.Principal` has no JSON tags. Its Go JSON field names are `Authenticator`, `Kind`, `ID`, and `Attributes`. Do not confuse these with the snake_case fields on run requests. A body principal is not proof of identity.

```sh
curl -s http://127.0.0.1:8080/bonnie/v1/runs \
  -H 'Content-Type: application/json' \
  -d '{"address":"browser-42","text":"Summarize the deployment plan.","context":["Use staging only."],"turn_policy":"queue"}'
```

The normal response contains `run_id`, `state`, and optional `turn_id`, `cursor`, `response`, `suspend`, and `usage`. Usage contains `input_tokens` and `output_tokens` when available. Address ensure and lookup replies contain the run ID and optional cursor, not a model result.

States are `pending`, `running`, `waiting`, `completed`, `failed`, `cancelled`, and `retired`. A completed run can receive another turn. Retirement is permanent.

## Answer a waiting run

A `suspend` object has `kind`, `prompt`, and optional `options`, `tool_call_id`, and `turn_id`. Use `/respond`, not the message route, to answer it:

```sh
curl -s http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID/respond \
  -H 'Content-Type: application/json' \
  -d '{"responses":[{"turn_id":"TURN_ID_FROM_SUSPEND","text":"eu-west-1"}]}'
```

Each response accepts `text`, optional `turn_id`, and optional `approved`. For an approval, send an explicit Boolean verdict:

```json
{"responses":[{"turn_id":"TURN_ID_FROM_SUSPEND","text":"Use staging only.","approved":true}]}
```

`approved: false` is rejection. An absent `approved` field is not rejection; it is a text-only answer. Include the current suspension's turn ID to protect against a delayed answer for an older turn.

## Cancel and reset

```sh
curl -s http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID/cancel \
  -H 'Content-Type: application/json' -d '{"turn_id":"OBSERVED_TURN_ID"}'
```

The optional `turn_id` protects a later turn from a stale command. A cancellation reply has `status` and optional `run_id` and `turn_id`. Status is `requested`, `not_active`, or `stale`. `requested` returns 202; no-op replies return 200. This confirms the command was saved, not that work stopped. Wait for `cancelled`. Cancellation can withdraw a parked turn and cannot undo external actions.

Reset accepts an optional `{"reason":"Start a separate task"}` body. It retires the exact run and frees bindings in the HTTP namespace only. Clear keeps working files and the journal; it is not secure deletion. Compact requires agent compaction support.

## Subscribe before the first turn

Live model deltas cannot be recovered from conversation history. Ensure an address before sending input:

```sh
curl -s -X POST http://127.0.0.1:8080/bonnie/v1/addresses/browser-42
# Copy run_id and cursor from this reply.
curl -sN 'http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID/stream?cursor=CURSOR'
# In another terminal:
curl -s http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID \
  -H 'Content-Type: application/json' -d '{"text":"Review the plan."}'
```

Encode an address containing `/` as one URL path segment, for example `browser%2F42`. Do not add a channel prefix.

Streams use `application/x-ndjson` and `Cache-Control: no-store`. Events contain `run_id`, `seq`, `type`, `time`, and optional `turn_id`, `text`, `state`, or `data`. Runtime types include `run_turn`, `run_cancel_requested`, `run_state`, `run_suspend`, `run_resume`, and `run_response`. Other types come from Kit lifecycle events.

A cursor is the last journal position seen, not a unique counter for every live delta. Use a non-negative integer. Durable events can be replayed after a restart. Live-only reasoning and tool deltas cannot. Keep the stream open across waiting states; disable proxy buffering and set suitable timeouts.

For a conversation view, GET `/snapshot` first, then stream after its cursor. The snapshot adds `messages`, with complete Kit message content parts, to the run response. It excludes live reasoning and tool activity. Compaction does not remove older messages from display history; clear, branch selection, and torn-step repair do affect that view. Snapshot replies are `no-store`.

## Authentication and authorization

There is no default bearer-token environment variable or built-in token policy. Configure a verifier in Go:

```go
// Imports: net/http, os, github.com/mark3labs/bonnie,
// github.com/mark3labs/bonnie/channel, and
// bonniehttp "github.com/mark3labs/bonnie/channel/http".
bonnie.New(
    bonnie.WithHTTPAuthenticator(func(r *http.Request) (*channel.Principal, error) {
        token := os.Getenv("AGENT_HTTP_TOKEN") // Host-defined variable.
        if token == "" || r.Header.Get("Authorization") != "Bearer "+token {
            return nil, bonniehttp.ErrUnauthenticated
        }
        return &channel.Principal{
            Authenticator: "host-token", Kind: "app", ID: "operations",
        }, nil
    }),
).Serve()
```

This is a simple single-caller example, not a multi-tenant access policy. Use TLS. For OIDC or mutual TLS, validate the assertion or certificate and return the identity it proves. Returning `ErrUnauthenticated` produces 401. Other verifier errors produce 500. A nil principal with nil error permits an unattributed request.

The verifier covers every HTTP channel route except health, including routes mounted on a host's own mux. With it configured, body `auth` is ignored, not merged. Without it, body identity is unverified. The API does not automatically restrict exact run IDs to their original caller. Add access controls around routes and host tools before a multi-user deployment.

Low-level servers can build `bonniehttp.New(runner, bonniehttp.WithAuthenticator(fn))` and mount `ch.Handler()`. `HandlerWithOutbound` also supplies a registry for hand-offs. Custom channels cannot mount routes under the reserved `/bonnie/` namespace.

## Idempotent starts

`operation_id` requires a non-nil verified principal from the channel authenticator. A reverse proxy alone does not make the field valid unless the channel verifier returns a proven principal.

```sh
curl -s http://127.0.0.1:8080/bonnie/v1/runs \
  -H "Authorization: Bearer $AGENT_HTTP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"operation_id":"deploy-request-42","text":"Prepare a staging deployment."}'
```

The same key under the same `Authenticator`, `Kind`, and `ID` returns saved state without another turn. Changed text on a retry does not change the admitted operation. This also applies after failure, cancellation, retirement, or a restart. Use a new key for new work. A crash before execution can leave `pending`; recovery is explicit through the run API. This is not distributed exactly-once execution: keep one live owner for the journal's runs.

## Errors

Non-2xx channel replies use `{"error":"human-readable text","code":"stable_code"}`. Branch on `code`, not the text.

| Status | Representative codes |
| --- | --- |
| 400 | `bad_request`, `invalid_run_id`, `unknown_turn_policy` |
| 401 | `unauthenticated` |
| 404 | `run_not_found` |
| 405 | `method_not_allowed` |
| 409 | `run_waiting`, `run_not_waiting`, `run_active`, `run_not_active`, `run_retired`, `run_owned_elsewhere`, `conversation_corrupt` |
| 413 | `too_large` |
| 499 | `client_closed` |
| 501 | `files_unsupported`, `compaction_unsupported` |
| 500 | `internal` |

Unexpected internal details go to server logs, not the response. A transport timeout does not prove an external action did not occur.

## Schedule routes

When schedules are configured, GET `/bonnie/v1/schedules` lists them and GET `/bonnie/v1/schedules/{name}` returns definition and history. POST `/{name}/trigger` is mounted only with `WithScheduleTriggerAuthorizer`. This callback authorizes a trigger separately from the normal HTTP authenticator; configure both as needed.

Manual triggers may send `{}`. External triggers require a stable `id`, RFC3339 `scheduled_at`, and `kind: "external"`. Retry with the same ID and scheduled time. Unlike run request decoding, trigger decoding rejects unknown fields and extra JSON values. See [scheduled channel work](/channels/overview#scheduled-channel-work) for delivery limits.

## Source and tests

[HTTP godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel/http). Verified against `channel/http/http.go`, `snapshot.go`, `runtime/events.go`, `runtime/suspend.go`, and `runtime/cancellation.go`. Tests include `auth_test.go`, `ensure_test.go`, `snapshot_test.go`, `operation_test.go`, `cancel_test.go`, and `limits_test.go` under `channel/http`.
