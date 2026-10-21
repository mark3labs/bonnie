package wire

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
	// RunInterrupted means the host stopped execution to recover later. It is
	// distinct from an explicit operator cancellation. It is not terminal:
	// Runner.Continue (and Runner.Recover, which a served agent calls at
	// startup) continues it, and so does StartRetry.
	RunInterrupted RunState = "interrupted"
	// RunCancelled means an operator stopped the turn before it ended.
	// Completed steps stay in the journal, so a cancelled run is resumable
	// with Runner.Start — unlike [RunFailed], which records a turn that
	// the agent itself could not finish.
	RunCancelled RunState = "cancelled"
	// RunRetired means the run was reset: a person or a channel asked for
	// a fresh conversation, and this one is closed for good. It keeps its
	// history and stays readable, but Runner.Start and Runner.Resume
	// refuse it with runtime.ErrRunRetired. It is the only terminal state a run
	// cannot leave.
	RunRetired RunState = "retired"
)

// IsTerminal reports whether the state admits no further transitions on its
// own. A terminal run other than [RunRetired] can still be revived by
// Runner.Start, which appends a new turn to the replayed conversation.
//
// [RunInterrupted] is not terminal: the turn has no stop reason, and the host
// continues it after a restart. Treating it as terminal let the scheduler
// mark interrupted work failed and let sandbox cleanup delete the working
// files the continued turn needs.
func (s RunState) IsTerminal() bool {
	return s == RunCompleted || s == RunFailed || s == RunCancelled || s == RunRetired
}
