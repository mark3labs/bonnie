package runtime

import "context"

// Finish a bottom-up cancellation if its owner stopped after storing intent.
func (r *Runner) recoverOwnedCancellation(ctx context.Context, runID string) error {
	recs, err := r.journal.Replay(ctx, runID)
	if err != nil {
		return err
	}
	requested, background := false, false
	for _, rec := range recs {
		if rec.Kind == RecordOwnerCancel {
			requested = true
			background = background || string(rec.Payload) == "true"
		}
	}
	if !requested {
		return nil
	}
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return err
	}
	// Cancelled owners may still have descendants left by an interrupted walk.
	if state == RunCompleted || state == RunFailed || state == RunRetired {
		return nil
	}
	return r.CancelOwned(ctx, runID, background)
}
