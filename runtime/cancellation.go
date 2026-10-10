package runtime

import (
	"context"
	"errors"
	"fmt"
)

// CancelResult reports a cancellation command, not execution completion.
// Requested means the command is durable. RunCancelled on the stream confirms
// completion. NotActive and Stale are harmless no-ops.
type CancelResult struct {
	RunID  string `json:"run_id,omitempty"`
	TurnID string `json:"turn_id,omitempty"`
	Status string `json:"status"`
}

// Cancellation outcomes shared by all transports.
const (
	CancelRequested = "requested"
	CancelNotActive = "not_active"
	CancelStale     = "stale"
)

// RequestCancel durably requests cancellation of the current turn, including
// a turn parked for human input, interrupted, or left running by a dead owner.
// expectedTurnID, when set, protects a later
// turn from an old command. Unknown and idle runs are harmless no-ops.
// Execution is cooperative; external effects cannot be undone. Like Start,
// this method requires one Runner owner per run. It does not route commands
// to another process that is executing the same run.
func (r *Runner) RequestCancel(ctx context.Context, runID, expectedTurnID string) (CancelResult, error) {
	out := CancelResult{RunID: runID, Status: CancelNotActive}
	r.mu.Lock()
	act, active := r.active[runID]
	if !active {
		// Reserve the run while cancelling an inactive turn. Resume and Start
		// must not pass the state check while this command is being written.
		var err error
		ctx, act, err = r.acquireLocked(ctx, runID)
		if err != nil {
			r.mu.Unlock()
			return out, err
		}
	}
	r.mu.Unlock()
	if !active {
		defer r.release(runID)
	}
	if active {
		select {
		case <-act.ready:
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
	act.control.Lock()
	defer act.control.Unlock()
	// Admission can precede the durable turn record. Do not attach a command
	// to the previous turn while the new turn is still being prepared.
	if active && act.turnID == "" {
		return out, nil
	}
	recs, err := r.journal.Replay(ctx, runID)
	if errors.Is(err, ErrRunNotFound) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("bonnie: read cancellation target: %w", err)
	}
	turnID, state, requested := cancellationState(recs)
	if active && act.turnID != "" {
		turnID = act.turnID
	}
	out.TurnID = turnID
	if expectedTurnID != "" && expectedTurnID != turnID {
		out.Status = CancelStale
		return out, nil
	}
	if act.finished || (!active && state != RunWaiting && state != RunInterrupted && state != RunRunning && !requested) {
		return out, nil
	}
	if !requested {
		seq, err := r.journal.Append(ctx, Record{RunID: runID, Text: turnID, Kind: RecordCancel, Timestamp: now()})
		if err != nil {
			return out, fmt.Errorf("bonnie: record cancellation: %w", err)
		}
		r.bus.Publish(Event{RunID: runID, TurnID: turnID, Type: EventCancelRequested, Seq: seq})
	}
	out.Status = CancelRequested
	if active {
		act.cancel()
		return out, nil
	}
	if err := r.checkpoint(ctx, runID, RunCancelled); err != nil {
		return out, err
	}
	return out, nil
}

// cancellationState makes a durable command effective even if its owner died
// before the final checkpoint. A new turn record ends the command's scope.
func cancellationState(recs []Record) (turnID string, state RunState, requested bool) {
	for _, rec := range recs {
		switch rec.Kind {
		case RecordTurn:
			turnID, requested = rec.Text, false
		case RecordState:
			state = rec.State
			if state == RunCancelled {
				requested = false
			}
		case RecordCancel:
			if rec.Text == turnID {
				requested = true
			}
		}
	}
	return
}

func (r *Runner) recoverCancellation(ctx context.Context, runID string) error {
	recs, err := r.journal.Replay(ctx, runID)
	if errors.Is(err, ErrRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, _, requested := cancellationState(recs)
	if requested {
		return r.checkpoint(ctx, runID, RunCancelled)
	}
	return nil
}
