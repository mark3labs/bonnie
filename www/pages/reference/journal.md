---
title: Journal
description: SQLite storage, journal interfaces, lossless records, atomic steps, legacy import, and safe retention.
---

# Journal

The journal is the durable record of a run. Events and terminal output are not substitutes for it. Root hosting uses one SQLite database at `<journal-directory>/journal.db`; the default is `.bonnie/journal.db`.

See [Runtime](/reference/runtime) for execution and recovery, [Events](/reference/events) for cursor replay, and [Deployment](/guides/deployment) for storage ownership.

## Built-in stores

| API | Persistence | Purpose |
| --- | --- | --- |
| `OpenSQLiteJournal(dir, opts...)` | Yes | Production-shaped local storage; empty directory selects `.bonnie` |
| `NewMemoryJournal()` | No | Tests and deliberately temporary execution |

Both implement `Journal`, `Positioner`, and `StepJournal`. Root `WithJournal(dir)` selects a directory, not a custom journal implementation. Use the low-level Runner to supply another store.

SQLite uses `modernc.org/sqlite`, a pure-Go driver. BONNIE must build with `CGO_ENABLED=0`. Do not substitute a driver that requires C.

The database uses WAL. SQLite can create `journal.db-wal` and `journal.db-shm` beside the database. Connection settings are in the DSN so they apply to each pooled connection. The pool is bounded to 16 open and 4 idle connections, with a one-minute idle timeout.

Records are append-only and keyed by `(run_id, seq)`. The runs table stores current state. State and its supporting record are updated in the same transaction. Strict tables reject inappropriate value types. A store from a newer unsupported schema fails open with `ErrJournalSchema`, rather than being read under an older shape.

## Flush policy

Use the runtime journal option, not a root option:

```go
journal, err := runtime.OpenSQLiteJournal(dir,
    runtime.WithFsync(runtime.FsyncAlways),
)
if err != nil {
    return fmt.Errorf("bonnie: open journal: %w", err)
}
defer journal.Close()
```

| Policy | SQLite setting | Contract |
| --- | --- | --- |
| `FsyncAlways` (`always`, default) | `synchronous=FULL` | Flush commits before acknowledging writes; acknowledged records survive a power cut under SQLite's storage assumptions |
| `FsyncRelaxed` (`relaxed`) | `synchronous=NORMAL` | Trades recent acknowledged commits for throughput; WAL remains consistent but recent data can be lost on power failure |

Software cannot compensate for a storage device or filesystem that does not honor locking or flush requests. SQLite journal integrity is not coordination of model execution.

## `Journal` contract

Implementations must be safe for concurrent use.

| Method | Contract |
| --- | --- |
| `Append(ctx, rec) (int, error)` | Assign and return the record's per-run sequence number |
| `Replay(ctx, runID) ([]Record, error)` | Return all records in sequence order; unknown run returns `ErrRunNotFound` |
| `Checkpoint(ctx, runID, state) error` | Record a durable state transition |
| `State(ctx, runID) (RunState, error)` | Return current state; unknown run returns `ErrRunNotFound` |
| `Runs(ctx, state) ([]string, error)` | List current-state matches; empty state means all known runs |
| `Persisted() bool` | State whether records outlive the process |
| `Close() error` | Release storage resources |

An implementation with single-writer ownership can return `ErrRunOwnedElsewhere`. HTTP maps this to a conflict. Built-in SQLite does not return that sentinel: it serializes transactions across processes and rejects duplicate sequence keys.

Optional interfaces preserve compatibility with custom stores:

- `Positioner.Position(ctx, runID) (int, error)` reports the record count without full replay. An unknown run returns zero with no error. The runner uses it to anchor live events. Without it, the event stream has reduced replay guarantees and relies on the live backlog for unanchored activity.
- `StepJournal.AppendStep(ctx, recs) ([]int, error)` commits all records of one step as a unit and returns assigned sequences in input order. Partial writes must not be reported as success. Cancellation must not prevent saving an already-completed step.

For a store without `StepJournal`, Session falls back to individual Append calls. Restore's tail repair remains necessary for that crash window. Add a custom store to `runtime/journal_conformance_test.go` and test reopen behavior.

## Records

`Record` JSON has these fields:

| Field | Meaning |
| --- | --- |
| `seq` | Ordered position within this run, assigned by the journal |
| `run_id` | Owning run |
| `kind` | Record classification |
| `timestamp` | Record time |
| `entry_id`, `parent_id` | Conversation-tree entry and parent, when applicable |
| `role` | Message role |
| `ext_type` | Extension-data type |
| `text` | Human-readable display projection |
| `state` | State transition value |
| `payload` | Lossless JSON data required for restore |

**Do not rebuild messages from `text`.** Message payloads contain full `kit.LLMMessage` data, including tool IDs, inputs, results, files, media, and reasoning. Flattened text loses those parts. Legacy text-only records have a compatibility fallback; invalid non-empty JSON payloads are errors, not permission to discard structured content.

| Constant | Stored kind | Purpose |
| --- | --- | --- |
| `RecordTurn` | `turn` | Durable turn identity |
| `RecordCancel` | `cancel` | Turn-scoped cancellation command |
| `RecordMessage` | `message` | Lossless conversation message |
| `RecordStep` | `step` | Finished-step checkpoint |
| `RecordSuspend` | `suspend` | Saved request for external input |
| `RecordResume` | `resume` | Input that released suspension |
| `RecordCompaction` | `compaction` | Saved context compaction |
| `RecordExtensionData` | `extension_data` | Branch-aware extension metadata |
| `RecordModelChange` | `model_change` | Provider/model selection change |
| `RecordBranch` | `branch` | Selected branch tip; empty parent selects root |
| `RecordBranchSummary` | `branch_summary` | Summary of an abandoned branch |
| `RecordState` | `state` | Lifecycle transition |
| `RecordRepair` | `repair` | IDs excluded by torn-tail repair |
| `RecordSandbox` | `sandbox` | Backend, sandbox identity, and availability information |
| `RecordSandboxDeleted` | `workspace_deleted` | Successful working-file cleanup |
| `RecordTrigger` | `trigger` | Structured dispatch provenance |
| `RecordContext` | `context` | Per-turn context strings, separate from history |
| `RecordClear` | `clear` | Prior messages leave model context; journal remains |
| `RecordCompletion` | `completion` | Completion orchestration phases and budget |

Every tree entry with an EntryID must reach the journal. Branches, compaction, and metadata are needed to restore the active model context, not only user and assistant text. Clear is append-only forgetting: it records a new root selection rather than deleting prior records.

Run IDs starting with `ReservedRunPrefix` (`bonnie.`) hold framework bookkeeping, such as address mappings. Use `IsReservedRun` to filter operator listings. Do not use that namespace for application conversations.

## Atomic steps and repair

`Session` implements `kit.SessionManager` and `kit.StepAppender`. A completed tool step's assistant call and tool results are written in one SQLite transaction. Either the batch is present or it is not. Finished data is persisted before cancellation stops the next work.

Legacy or third-party journals can still contain incomplete tails. Restore removes an incomplete trailing tool step from the active conversation and appends `RecordRepair`; it does not erase original records. If only some sibling tool calls have results, the whole trailing step is excluded. A missing result in the middle of history returns `ErrCorruptConversation`, because silent repair would hide damage.

Atomic journal writes do not make external effects atomic. An external action can succeed before the process saves its tool result. Make such actions safe to retry.

## Legacy JSONL import

On open, SQLite detects `<root>/runs/<run-id>.jsonl` from the original format.

- Imports preserve sequence numbers so existing event cursors retain their meaning.
- A run already present in SQLite is skipped, making a crash between import and rename safe to retry.
- Imported source files are renamed to `<run-id>.jsonl.imported` and retained.
- Obsolete JSONL lock files are removed.
- Invalid run IDs or records fail open with an error. A damaged file is not silently skipped.

Opening the database can therefore change legacy storage even when called by `bonnie runs`. Back up old data before migration and inspect errors before removing any source files.

## Ownership, backup, and retention

SQLite protects records from conflicting write transactions; it does not stop two agents from executing the same run. Use one execution owner per run and one owner for cleanup. Network filesystems without reliable POSIX locking are unsafe. Do not treat shared SQLite storage as a multi-host coordinator.

For a simple consistent backup, stop the owner cleanly and preserve the journal directory and run working files together. Do not copy only a live `journal.db` while ignoring its WAL. Use a SQLite-aware backup method if the server must remain live. Journal backup alone does not preserve sandbox output or remote container/VM state.

There is no built-in transcript retention or record-deletion command. `bonnie sandbox prune` and `WithRunSandboxCleanup` delete eligible working files only. They retain history, state, and channel addresses. Successful cleanup writes `workspace_deleted`; another terminal checkpoint permits a later cleanup cycle. Waiting, running, and pending runs retain their files.

Logs, records, and backups can contain private data and tool output. Restrict file access, encrypt storage as needed, and do not publish raw JSON diagnostics. See [CLI](/reference/cli) for inspection and [Troubleshooting](/guides/troubleshooting) for recovery checks.
