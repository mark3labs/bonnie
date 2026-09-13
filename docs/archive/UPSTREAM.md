# Upstream asks for Kit — archived record

**Archived 2026-09-13.** This is the historical record of the four asks the
`v0.1.0` design raised against Kit. All four were answered by `v0.106.0`
before anything was filed, so nothing here is actionable — it stays as the
ledger of what was asked, why, and what each ask became. Future asks belong
in [`docs/UPSTREAM.md`](../UPSTREAM.md), the forward page.

# Upstream asks for Kit

**Status: ANSWERED. All four landed in Kit `v0.106.0`** — PR
`mark3labs/kit#135`, "feat(sdk): durability seams for external SessionManager
implementations", merged 2026-09-12. The release notes credit BONNIE as the
first external consumer of the public SDK and say the seams were verified
non-breaking against BONNIE's own suite.

Nothing was filed as issues; the release landed before the filing. This file
stays as the record of what was asked, why, and what each ask became. The
citations below are from Kit `v0.105.0`, the version the asks were drafted
against; `docs/SPEC.md` §3 carries the re-verified `v0.106.0` citations.

| Ask | Became | Where in v0.106.0 |
|---|---|---|
| 1. Batch append | `kit.StepAppender` optional interface, dispatched at every multi-message site | `pkg/kit/session.go:181-231`, `pkg/kit/kit.go:2996,3141,3205` |
| 2. Per-step tools | `PrepareStepResult.Tools []Tool`, nil-vs-empty documented | `pkg/kit/hooks.go` |
| 3. Halt/FinalValue promise | stated as contract in the godoc | `pkg/kit/tools.go` |
| 4. SessionManager policy | frozen for `v0.x`; capability via optional interfaces | `pkg/kit/session.go` |

BONNIE adopted ask 1 in `runtime/session.go` (`Session.AppendStep`) and
mirrored the optional-interface pattern on its own `Journal` seam
(`runtime.StepJournal`), so a host journal is not forced to implement the
batch method either. See `docs/SPEC.md` §4.2 for what the adoption closed
and why the torn-write repair stays.

---

## 1. Batch append on `SessionManager`

**ANSWERED in v0.106.0.** Landed almost exactly as drafted — as the optional
`kit.StepAppender` interface rather than a widening of `SessionManager`,
dispatched at all three persistence sites, with the fallback preserved.
One improvement over the draft: the shipped `AppendStep` takes a `context.Context`,
and its godoc documents that the context may already be cancelled, because
Kit persists a completed step before it checks for cancellation. BONNIE's
`Session.AppendStep` honours that with `context.WithoutCancel`.

**Priority: highest.** Without this, no external `SessionManager` can be
crash-safe for a tool-calling step.

### The problem

`kit.SessionManager.AppendMessage` takes one message. Its doc comment
(`pkg/kit/session.go:30-35`) promises that an assistant message and its tool
result "are appended together as a pair", so "the session never contains an
orphaned tool call without its result, which would break subsequent LLM
requests".

Kit keeps that promise inside the process. It cannot keep it across a crash,
because it calls the method once per message. There are **three** such loops in
`pkg/kit/kit.go`, and all three have the same hazard:

```go
// kit.go:2990-2994 — per-step persistence during generation
OnStepMessages: func(stepMessages []fantasy.Message) {
    for _, msg := range stepMessages {
        _, _ = m.session.AppendMessage(msg)
    }
},

// kit.go:3128-3130 — persistGenerationRemainder, the tail of a finished or
// overflow-retried turn. This one is the most dangerous: the remainder it
// writes can contain a complete assistant + tool pair.
for _, msg := range newMessages[result.PersistedMessageCount:] {
    _, _ = m.session.AppendMessage(msg)
}

// kit.go:3194-3196 — pre-generation messages. Lower risk, because these are
// user-role, but it is the same unbatched write.
for _, msg := range preMessages {
    _, _ = m.session.AppendMessage(msg)
}
```

An implementation that writes to durable storage therefore produces two
independent writes for one step:

```
write N    : assistant message that carries tool_use   <- the process dies here
write N+1  : tool message that carries tool_result
```

A process that dies between them leaves storage holding an assistant message
whose `tool_use` has no `tool_result`. Anthropic and OpenAI both reject that
conversation. The run is then permanently unresumable.

**Fixing only the first loop is not enough.** `persistGenerationRemainder` runs
at the end of every turn and again on the context-overflow retry path, and it
writes a multi-message remainder. A batch hook has to be used at every site
that writes more than one message.

### Evidence

BONNIE hits this. The repair is
[`runtime/repair.go`](../runtime/repair.go), and the test that reproduces the
torn write is `TestFileJournalCrashResumeIsProviderValid` in
[`runtime/filejournal_test.go`](../runtime/filejournal_test.go). BONNIE must
drop the incomplete step on restore, which throws away work the agent had
already done.

### The ask

Add the batch write as an **optional interface**, so the change breaks nobody:

```go
// StepAppender is an optional interface a SessionManager may implement. When
// it does, Kit appends the messages of one agent step in a single call instead
// of looping over AppendMessage. An implementation that writes to durable
// storage should make that write atomic, so a crash never leaves a tool call
// without its result.
type StepAppender interface {
    AppendStep(msgs []LLMMessage) (entryIDs []string, err error)
}
```

Kit type-asserts at each of the three sites above and falls back to the
existing loop:

```go
if sa, ok := m.session.(StepAppender); ok {
    _, _ = sa.AppendStep(msgs)
} else {
    for _, msg := range msgs {
        _, _ = m.session.AppendMessage(msg)
    }
}
```

This lets an implementation write one step as one `fsync`, or one database
transaction. Nothing in the public API can do that today.

**Note on the alternative.** Adding `AppendStep` directly to `SessionManager`
would be a breaking change: Go interfaces have no default implementations, so
every external implementer fails to compile with no deprecation window. The
optional interface avoids that entirely, and it sets the precedent asked for in
item 4 below.

---

## 2. `PrepareStepResult.Tools []Tool`

**ANSWERED in v0.106.0.** `Tools []Tool` landed on `PrepareStepResult` with
the nil-versus-empty distinction documented: nil keeps the live tool set, so
runtime `AddTools`/`RemoveTools` still reach the model mid-turn; an empty
non-nil slice offers no tools and forces a text response. Not yet used by
BONNIE; L2 discovery is the consumer.

### The problem

The doc comment on `PrepareStepHook` in `pkg/kit/hooks.go` advertises "dynamic
tool filtering per step". `PrepareStepResult` cannot express it: the struct has
no field for the tool set.

A host that wants to give the model a different tool set on a later step — a
common pattern for a multi-phase agent — has no public way to do it.

### The ask

Add a `Tools []Tool` field to `PrepareStepResult`, with nil meaning "leave the
tool set alone", to match the other fields on that struct.

### Why BONNIE cares

L2 will want per-phase capabilities. This is not blocking for `v0.1.0`, so it
is second in the list, not first.

---

## 3. A stability promise for `ToolOutput.Halt` and `FinalValue`

**ANSWERED in v0.106.0.** The godoc now states that `Halt` plus `FinalValue`
is a supported suspension mechanism, that a halted tool call still emits a
well-formed tool result, and that `FinalValue` is propagated by dynamic type,
unmodified. No behaviour change — the implementation already did this;
BONNIE's suspension protocol is contract now, not coincidence.

### The problem

`ToolOutput.Halt` with `ToolOutput.FinalValue` reads today as a convenience
for a tool that wants to stop the loop early.

BONNIE's whole suspension protocol rests on it. `AskTool` and `ApprovalTool`
([`runtime/suspend.go`](../runtime/suspend.go)) return
`ToolOutput{Halt: true, FinalValue: SuspendRequest{...}}`, and the runner
recognises a parked run by the dynamic type of `TurnResult.FinalValue`.

The behaviour is correct and well designed. `recordHalt` runs before
`toolOutputToResponse` returns (`pkg/kit/tools.go:265-275`), so a halted turn
still emits a well-formed assistant message and tool result. That is exactly
what a resume needs.

### The ask

State in the doc comment that `Halt` plus `FinalValue` is a supported
suspension mechanism, and that the pairing behaviour above is part of the
contract rather than an implementation detail. A sentence in the godoc is
enough.

---

## 4. A stability policy for `kit.SessionManager`

**ANSWERED in v0.106.0.** The interface is frozen for the `v0.x` line; new
capability arrives through optional interfaces that Kit type-asserts for.
`StepAppender` is the first instance and sets the precedent, exactly as the
ask predicted. An implementer can now tell which world it lives in.

### The problem

`kit.SessionManager` has 21 methods. Adding one breaks every external
implementer at compile time, with no deprecation window.

BONNIE's `Session` implements all 21. The line

```go
var _ kit.SessionManager = (*Session)(nil)
```

in [`runtime/session.go`](../runtime/session.go) is a deliberate tripwire: a
widened interface breaks BONNIE's build there first.

### The ask

Say in the godoc how the interface will change, and pick one:

- Freeze the interface for the `v0.x` line, and add capability through
  optional interfaces that Kit type-asserts for.
- Or document that the interface may grow in a minor release, so an
  implementer knows to pin.

Either answer is workable. No answer is not, because an implementer cannot
tell which world they live in.

Ask 1 is the first instance of this question, and it is already written in the
optional-interface form: `StepAppender` adds capability without touching
`SessionManager`. Accepting ask 1 as written therefore answers this ask by
precedent — new capability arrives through optional interfaces that Kit
type-asserts for, and the 21-method core stays frozen.

---

## What BONNIE does NOT need

Recorded so that nobody adds these on BONNIE's account:

- **A durable-session interface.** `SessionManager` is the right seam. BONNIE
  needs no new abstraction, only the atomic write in ask 1.
- **A suspension type in Kit.** `ToolOutput.FinalValue` carrying a host-defined
  type is the correct design. BONNIE defines its own `SuspendRequest`.
- **Event persistence.** The journal is the durable record. Kit's event bus is
  a live view, and that is the right split.
