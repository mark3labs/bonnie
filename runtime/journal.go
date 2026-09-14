package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// RunState describes where a run is in its lifecycle. A run is durable: it
// survives process death and can sit in [RunWaiting] indefinitely without
// holding compute.
type RunState string

const (
	// RunPending means the run is recorded but has not executed a step yet.
	RunPending RunState = "pending"
	// RunRunning means a turn is executing now.
	RunRunning RunState = "running"
	// RunWaiting means the run suspended and needs external input to continue.
	RunWaiting RunState = "waiting"
	// RunCompleted means the run finished normally.
	RunCompleted RunState = "completed"
	// RunFailed means the run ended with an error.
	RunFailed RunState = "failed"
	// RunCancelled means an operator stopped the turn before it ended.
	// Completed steps stay in the journal, so a cancelled run is resumable
	// with [Runner.Start] — unlike [RunFailed], which records a turn that
	// the agent itself could not finish.
	RunCancelled RunState = "cancelled"
)

// IsTerminal reports whether the state admits no further transitions on its
// own. A terminal run can still be revived by [Runner.Start], which appends a
// new turn to the replayed conversation.
func (s RunState) IsTerminal() bool {
	return s == RunCompleted || s == RunFailed || s == RunCancelled
}

// RecordKind classifies a [Record] in the journal.
type RecordKind string

const (
	// RecordMessage is a conversation message appended to the session tree.
	RecordMessage RecordKind = "message"
	// RecordStep marks the end of one agent step. It is the checkpoint
	// boundary: on replay, completed steps are not re-executed.
	RecordStep RecordKind = "step"
	// RecordSuspend records that a tool halted the turn to await input.
	RecordSuspend RecordKind = "suspend"
	// RecordResume records the input that released a suspension.
	RecordResume RecordKind = "resume"
	// RecordCompaction records a context compaction.
	RecordCompaction RecordKind = "compaction"
	// RecordExtensionData records a branch-aware extension data entry.
	RecordExtensionData RecordKind = "extension_data"
	// RecordModelChange records a switch of provider or model mid-run.
	RecordModelChange RecordKind = "model_change"
	// RecordBranchSummary records a summary of an abandoned branch.
	RecordBranchSummary RecordKind = "branch_summary"
	// RecordState records a run-state transition.
	RecordState RecordKind = "state"
	// RecordRepair records that a restore dropped an incomplete trailing
	// tool-calling step. See docs/SPEC.md §4.2.
	RecordRepair RecordKind = "repair"
	// RecordSandbox records that a sandbox opened for a run: which backend,
	// which sandbox. It is the only record of a run's workspace, so a
	// resumed run can tell a vanished workspace from a live one, and a
	// reconciler can find the sandboxes of terminal runs. See
	// docs/SPEC.md §4.10.
	RecordSandbox RecordKind = "sandbox"
)

// Record is one durable entry in a run's journal. Records are append-only and
// ordered by Seq within a run.
//
// Text holds a flattened, human-readable rendering of the entry and is for
// display only. Payload holds the lossless encoding that a replay needs, and
// its shape depends on Kind:
//
//   - [RecordMessage]: the full JSON-encoded kit.LLMMessage, including tool
//     calls, tool results, files, and reasoning. Text keeps only the text
//     parts, so a replay that reads Text alone silently drops the rest.
//   - [RecordCompaction]: the compaction metadata.
//   - [RecordModelChange]: the provider and model.
//   - [RecordSuspend] and [RecordStep]: the [SuspendRequest].
//   - [RecordRepair]: the entry IDs a torn-write repair dropped.
//   - [RecordSandbox]: the backend and sandbox ID, and whether the sandbox
//     was seen to be gone.
//
// Every record that carries an EntryID takes part in the conversation tree, so
// each one must be journalled. An entry that reaches the tree but not the
// journal orphans everything appended after it.
type Record struct {
	Seq       int             `json:"seq"`
	RunID     string          `json:"run_id"`
	Kind      RecordKind      `json:"kind"`
	Timestamp time.Time       `json:"timestamp"`
	EntryID   string          `json:"entry_id,omitempty"`
	ParentID  string          `json:"parent_id,omitempty"`
	Role      string          `json:"role,omitempty"`
	ExtType   string          `json:"ext_type,omitempty"`
	Text      string          `json:"text,omitempty"`
	State     RunState        `json:"state,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// ErrRunNotFound is returned when a run ID is unknown to the journal.
var ErrRunNotFound = errors.New("bonnie: run not found")

// ErrRunOwnedElsewhere is part of the [Journal] contract for an
// implementation that admits only one writer per run: a write to a run some
// other owner holds is refused with this error rather than allowed to
// interleave records. A transport maps it to a conflict — `channel/http`
// answers 409 — because no retry can fix it.
//
// The built-in [SQLiteJournal] never returns it. SQLite serialises write
// transactions across processes, and the journal's (run_id, seq) primary key
// makes a reused sequence number a constraint violation, so concurrent
// writers are safe rather than forbidden. BONNIE's original JSONL journal
// needed a lock file, and this error, to reach the same place. It stays
// exported for a journal backed by a store that cannot make the same
// promise.
var ErrRunOwnedElsewhere = errors.New("bonnie: run is owned by another process")

// ReservedRunPrefix marks run IDs that belong to BONNIE itself rather than to
// a conversation. A transport that needs durable bookkeeping — an HTTP
// channel's address-to-run map, for example — writes it to a reserved run, so
// it inherits the journal's durability without inventing a second store.
//
// Operator-facing listings hide reserved runs. Use [IsReservedRun] to filter.
const ReservedRunPrefix = "bonnie."

// IsReservedRun reports whether a run ID belongs to BONNIE's own bookkeeping.
func IsReservedRun(runID string) bool {
	return strings.HasPrefix(runID, ReservedRunPrefix)
}

// Journal is the durability seam. Implement it to back BONNIE runs with any
// store: memory, files, Postgres, S3. It is the only interface a host must
// provide to get crash-resumable agent turns.
//
// Implementations must be safe for concurrent use.
type Journal interface {
	// Append writes a record and returns its assigned sequence number.
	// Implementations assign Seq; callers may leave it zero.
	Append(ctx context.Context, rec Record) (seq int, err error)

	// Replay returns every record for a run in sequence order. It returns
	// [ErrRunNotFound] when the run is unknown.
	Replay(ctx context.Context, runID string) ([]Record, error)

	// Checkpoint records a run-state transition.
	Checkpoint(ctx context.Context, runID string, state RunState) error

	// State returns the current state of a run. It returns [ErrRunNotFound]
	// when the run is unknown.
	State(ctx context.Context, runID string) (RunState, error)

	// Runs lists known run IDs that are currently in the given state. Pass an
	// empty state to list every run.
	Runs(ctx context.Context, state RunState) ([]string, error)

	// Persisted reports whether records outlive the process. Hosts use it to
	// decide whether a run can be resumed after a restart, and [Session]
	// reports it to Kit through IsPersisted.
	Persisted() bool

	// Close releases any resources held by the journal.
	Close() error
}

// Positioner is an optional interface a [Journal] may implement to report
// how many records a run has without replaying them.
//
// The event stream uses it as its anchor: every [Event] carries the journal
// position the event belongs to, so a client that reconnects can be served
// from the journal itself when the in-memory backlog has moved past its
// cursor. [SQLiteJournal] and [MemoryJournal] implement it. Without it the
// stream degrades to the live backlog only.
type Positioner interface {
	// Position returns the number of records a run has. It reports 0 with a
	// nil error for a run that does not exist.
	Position(ctx context.Context, runID string) (int, error)
}

// StepJournal is an optional interface a [Journal] may implement to commit a
// whole agent step as one unit.
//
// It exists for the same reason Kit's [kit.StepAppender] does: a tool-calling
// step is two messages, and writing them one at a time leaves a crash window
// between them. A journal that implements this interface commits every record
// of the step as one unit, so the step is either fully durable or not present
// at all.
//
// The contract mirrors [kit.StepAppender]: a partial write must not be
// reported as success, and cancellation must not stop a completed step from
// being written — see [Session.AppendStep] for why. [SQLiteJournal] and
// [MemoryJournal] both implement it. A journal that does not keeps working:
// [Session] falls back to one [Journal.Append] per record, and the torn-write
// repair in [Restore] still covers the crash window that leaves.
//
// This is an optional interface rather than a widening of [Journal] for the
// same reason Kit made [kit.StepAppender] optional: Go interfaces have no
// default implementations, so adding a method to [Journal] would break every
// host implementation at compile time, with no deprecation window.
type StepJournal interface {
	// AppendStep writes every record of one step and returns the sequence
	// number assigned to each, in input order. Implementations assign Seq;
	// callers may leave it zero.
	AppendStep(ctx context.Context, recs []Record) (seqs []int, err error)
}
