package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// RecordOwnership assigns a child run to a parent run.
const RecordOwnership RecordKind = "ownership"

// RecordOwnerCancel closes child admission before cancellation traverses the tree.
const RecordOwnerCancel RecordKind = "owner_cancel"

// ChildRun describes durable run ownership. Background work is not stopped by
// normal parent cancellation. One parent run can own many children.
type ChildRun struct {
	ID         string `json:"id"`
	ParentID   string `json:"parent_id"`
	Key        string `json:"key"`
	Background bool   `json:"background,omitempty"`
}

// SpawnChild creates a durable child and its first submission atomically.
// Key must be stable across retries, for example a tool-call identity stored
// by the caller. Reusing a key returns the existing child. The runner's factory
// selects each child's agent; this does not call Kit's ephemeral Subagent API.
// Atomic child creation requires a StepJournal.
func (r *Runner) SpawnChild(ctx context.Context, parentID, key string, in Input, background bool) (*ChildRun, error) {
	if parentID == "" || key == "" || IsReservedRun(parentID) {
		return nil, errors.New("bonnie: invalid child owner or key")
	}
	r.workMu.Lock()
	defer r.workMu.Unlock()
	state, err := r.journal.State(ctx, parentID)
	if err != nil {
		return nil, err
	}
	parentRecords, err := r.journal.Replay(ctx, parentID)
	if err != nil {
		return nil, err
	}
	for _, rec := range parentRecords {
		if rec.Kind == RecordOwnerCancel {
			return nil, errors.New("bonnie: child owner cancellation requested")
		}
	}
	if state == RunRetired || state == RunCancelled {
		return nil, errors.New("bonnie: child owner is closed")
	}
	sum := sha256.Sum256([]byte(parentID + "\x00" + key))
	id := "child-" + hex.EncodeToString(sum[:])
	recs, err := r.journal.Replay(ctx, id)
	if err == nil {
		for _, rec := range recs {
			if rec.Kind == RecordOwnership {
				var child ChildRun
				if err := json.Unmarshal(rec.Payload, &child); err != nil {
					return nil, err
				}
				return &child, nil
			}
		}
		return nil, errors.New("bonnie: child ID exists without ownership")
	}
	if !errors.Is(err, ErrRunNotFound) {
		return nil, err
	}
	j, ok := r.journal.(StepJournal)
	if !ok {
		return nil, errors.New("bonnie: child creation requires atomic step journal")
	}
	child := ChildRun{ID: id, ParentID: parentID, Key: key, Background: background}
	owner, err := json.Marshal(child)
	if err != nil {
		return nil, fmt.Errorf("bonnie: encode owner: %w", err)
	}
	item := Submission{ID: id + "-input", RunID: id, RequestID: key, Input: in, State: SubmissionQueued}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("bonnie: encode child input: %w", err)
	}
	_, err = j.AppendStep(ctx, []Record{
		{RunID: id, Kind: RecordOwnership, Timestamp: now(), Payload: owner},
		{RunID: id, Kind: RecordSubmission, Timestamp: now(), Payload: payload},
	})
	if err != nil {
		return nil, err
	}
	return &child, nil
}

// Children lists children of a run, including completed children.
func (r *Runner) Children(ctx context.Context, parentID string) ([]ChildRun, error) {
	ids, err := r.journal.Runs(ctx, "")
	if err != nil {
		return nil, err
	}
	var children []ChildRun
	for _, id := range ids {
		recs, err := r.journal.Replay(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			if rec.Kind != RecordOwnership {
				continue
			}
			var child ChildRun
			if err := json.Unmarshal(rec.Payload, &child); err != nil {
				return nil, fmt.Errorf("bonnie: decode ownership: %w", err)
			}
			if child.ParentID == parentID {
				children = append(children, child)
			}
		}
	}
	return children, nil
}

// CancelOwned stops a run and its owned descendants. Background children are
// skipped unless includeBackground is true. Queued inputs are withdrawn and
// parked runs become cancelled. Callers can wait for an active turn to finish
// through IsActive. Cancellation is bottom-up and is recorded before return.
func (r *Runner) CancelOwned(ctx context.Context, runID string, includeBackground bool) error {
	r.workMu.Lock()
	recs, err := r.journal.Replay(ctx, runID)
	if err != nil {
		r.workMu.Unlock()
		return err
	}
	recorded, recordedBackground := false, false
	for _, rec := range recs {
		if rec.Kind == RecordOwnerCancel {
			recorded = true
			if string(rec.Payload) == "true" {
				recordedBackground = true
			}
		}
	}
	requestedBackground := includeBackground
	includeBackground = includeBackground || recordedBackground
	if !recorded || (requestedBackground && !recordedBackground) {
		payload := []byte("false")
		if includeBackground {
			payload = []byte("true")
		}
		_, err = r.journal.Append(ctx, Record{RunID: runID, Kind: RecordOwnerCancel, Timestamp: now(), Payload: payload})
	}
	r.workMu.Unlock()
	if err != nil {
		return err
	}
	children, err := r.Children(ctx, runID)
	if err != nil {
		return err
	}
	for _, child := range children {
		if child.Background && !includeBackground {
			continue
		}
		if err := r.CancelOwned(ctx, child.ID, includeBackground); err != nil {
			return err
		}
	}
	r.workMu.Lock()
	defer r.workMu.Unlock()
	items, err := r.Submissions(ctx, runID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.State != SubmissionQueued && item.State != SubmissionRunning {
			continue
		}
		item.State = SubmissionAborted
		if err := r.writeSubmission(ctx, RecordSubmissionState, item); err != nil {
			return err
		}
	}
	if r.IsActive(runID) {
		_, err := r.RequestCancel(ctx, runID, "")
		return err
	}
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return err
	}
	if state.IsTerminal() {
		return nil
	}
	return r.checkpoint(ctx, runID, RunCancelled)
}
