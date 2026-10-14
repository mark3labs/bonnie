package runtime

import "context"

// Finish a bottom-up cancellation if its owner stopped after storing intent.
func (r *Runner) recoverOwnedCancellation(ctx context.Context, runID string) error {
	recs, err := replayKinds(ctx, r.journal, runID, RecordOwnerCancel)
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
	if state.IsTerminal() {
		settled, err := r.cancellationSettled(ctx, runID, background)
		if err != nil {
			return err
		}
		if settled {
			return nil
		}
	}
	return r.CancelOwned(ctx, runID, background)
}

// A terminal owner can still have queued input or live descendants after a
// crash. Check the whole included tree, not only its direct children.
func (r *Runner) cancellationSettled(ctx context.Context, runID string, background bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.IsActive(runID) {
		return false, nil
	}
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return false, err
	}
	if !state.IsTerminal() {
		return false, nil
	}
	items, err := r.Submissions(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.State == SubmissionQueued || item.State == SubmissionRunning {
			return false, nil
		}
	}
	recs, err := replayKinds(ctx, r.journal, runID, RecordOwnerCancel)
	if err != nil {
		return false, err
	}
	for _, rec := range recs {
		background = background || string(rec.Payload) == "true"
	}
	children, err := r.Children(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, child := range children {
		if child.Background && !background {
			continue
		}
		settled, err := r.cancellationSettled(ctx, child.ID, background)
		if err != nil || !settled {
			return false, err
		}
	}
	return true, nil
}
