package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// SandboxCleanupPolicy sets how long to keep a sandbox after the latest
// terminal checkpoint. A zero duration keeps the sandbox for that state.
// Durations must not be negative.
type SandboxCleanupPolicy struct {
	// CompletedAfter is the retention period for completed runs.
	CompletedAfter time.Duration
	// FailedAfter is the retention period for failed runs.
	FailedAfter time.Duration
	// CancelledAfter is the retention period for cancelled runs.
	CancelledAfter time.Duration
	// RetiredAfter is the retention period for retired runs.
	RetiredAfter time.Duration
}

func (p SandboxCleanupPolicy) retention(state RunState) time.Duration {
	switch state {
	case RunCompleted:
		return p.CompletedAfter
	case RunFailed:
		return p.FailedAfter
	case RunCancelled:
		return p.CancelledAfter
	case RunRetired:
		return p.RetiredAfter
	default:
		return 0
	}
}

// CleanupSandboxes deletes sandboxes of terminal runs whose retention period
// has passed. It skips reserved runs and runs active on this Runner. Waiting,
// running, and pending runs keep their sandboxes.
//
// The delete callback receives the run ID and the caller's context. The caller
// supplies any timeout. The callback must be safe to repeat: a crash or a failed
// journal write after deletion can cause another call. Its bool reports whether
// the sandbox existed; either bool with a nil error means it is now absent.
// A successful call writes RecordSandboxDeleted, even for an absent sandbox.
// No further deletion is attempted until a new terminal checkpoint is written.
// History and run state do not change.
//
// One Runner must own all turns and cleanup for these runs. The run lock is local
// to this Runner; it does not exclude turns on another Runner or in another
// process. A new Runner can take ownership after the previous owner stops.
//
// Cleanup continues after per-run errors and returns errors.Join of those errors.
// Failed deletions and failed journal writes are retried on the next call.
func (r *Runner) CleanupSandboxes(ctx context.Context, policy SandboxCleanupPolicy, delete func(context.Context, string) (bool, error)) error {
	for _, d := range []time.Duration{policy.CompletedAfter, policy.FailedAfter, policy.CancelledAfter, policy.RetiredAfter} {
		if d < 0 {
			return errors.New("bonnie: sandbox cleanup retention must not be negative")
		}
	}
	if delete == nil {
		return errors.New("bonnie: cleanup sandboxes: nil delete callback")
	}
	runs, err := r.journal.Runs(ctx, "")
	if err != nil {
		return fmt.Errorf("bonnie: list runs for sandbox cleanup: %w", err)
	}
	var errs []error
	for _, runID := range runs {
		if IsReservedRun(runID) {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := r.cleanupSandbox(ctx, runID, policy, delete); err != nil {
			errs = append(errs, fmt.Errorf("bonnie: cleanup sandbox %s: %w", runID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) cleanupSandbox(ctx context.Context, runID string, policy SandboxCleanupPolicy, delete func(context.Context, string) (bool, error)) error {
	// acquire detaches the turn context from caller cancellation. Cleanup must
	// use ctx instead, so the host can stop or bound a cleanup pass.
	if _, _, err := r.acquire(ctx, runID); err != nil {
		if errors.Is(err, ErrRunActive) {
			return nil
		}
		return err
	}
	defer r.release(runID)

	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return err
	}
	retention := policy.retention(state)
	if retention <= 0 {
		return nil
	}
	recs, err := r.journal.Replay(ctx, runID)
	if err != nil {
		return err
	}
	// Journal order, not timestamp order, identifies the latest checkpoint.
	// Later metadata must not extend the retention period.
	for _, rec := range slices.Backward(recs) {
		if rec.Kind == RecordSandboxDeleted {
			return nil
		}
		if rec.Kind != RecordState || !rec.State.IsTerminal() {
			continue
		}
		if rec.State != state || rec.Timestamp.IsZero() || now().Sub(rec.Timestamp) < retention {
			return nil
		}
		if _, err := delete(ctx, runID); err != nil {
			return err
		}
		// Once deletion succeeds, persist its receipt even if the caller's
		// deadline expired. This is metadata, not a conversation tree entry.
		receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := r.journal.Append(receiptCtx, Record{
			RunID: runID, Kind: RecordSandboxDeleted, Timestamp: now(),
		})
		return err
	}
	return nil
}
