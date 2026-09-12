# Upstream asks for Kit

These are the changes BONNIE needs from `github.com/mark3labs/kit/pkg/kit`.
Each one is written so you can paste it into an issue on `mark3labs/kit`.

BONNIE is Kit's first serious external consumer of the public SDK. Every gap
here is a gap that a real SDK user hits.

File status: **drafted, not yet filed.** Put the issue link next to each title
when you file it, and copy the links into `docs/SPEC.md` §6.

Line numbers are from Kit `v0.105.0`, the version pinned in `go.mod`. Check
them again before you file.

---

## 1. Batch append on `SessionManager`

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
