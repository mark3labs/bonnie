package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// RecordSubmission durably admits an input before execution.
	RecordSubmission RecordKind = "submission"
	// RecordSubmissionState records delivery and settlement of an input.
	RecordSubmissionState RecordKind = "submission_state"
)

// BusyPolicy specifies how a durable input waits for a run.
type BusyPolicy string

const (
	// BusyQueue executes after the current turn finishes.
	BusyQueue BusyPolicy = "queue"
	// BusyReject refuses input while this runner executes the run.
	BusyReject BusyPolicy = "reject"
	// BusySteer durably records input before injection into the active turn.
	// If the process stops before placement, recovery delivers it as a follow-up.
	BusySteer BusyPolicy = "steer"
)

// SubmissionState describes delivery of a durable input.
type SubmissionState string

const (
	// SubmissionQueued means the input has not started.
	SubmissionQueued SubmissionState = "queued"
	// SubmissionRunning means delivery started and may need recovery.
	SubmissionRunning SubmissionState = "running"
	// SubmissionDone means the input reached a turn boundary, including a human wait.
	SubmissionDone SubmissionState = "done"
	// SubmissionAborted means the input was withdrawn before execution.
	SubmissionAborted SubmissionState = "aborted"
	// SubmissionFailed means execution returned an error and is not retried automatically.
	SubmissionFailed SubmissionState = "failed"
)

// Submission is an accepted input that can be found after a restart.
// RequestID deduplicates admission within one run. It is not an external
// service idempotency key. A waiting human response still uses Runner.Resume.
type Submission struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	RequestID string          `json:"request_id,omitempty"`
	Input     Input           `json:"input"`
	State     SubmissionState `json:"state"`
	Error     string          `json:"error,omitempty"`
	Policy    BusyPolicy      `json:"policy,omitempty"`
}

// Submit stores an input in the run's inbox. RunScheduler executes it.
// One process must own the journal. Multiple runners must not schedule the
// same journal concurrently; SQLite write locks are not execution leases.
func (r *Runner) Submit(ctx context.Context, runID, requestID string, in Input, policy BusyPolicy) (*Submission, error) {
	if runID == "" || IsReservedRun(runID) {
		return nil, errors.New("bonnie: invalid submission run ID")
	}
	if policy != "" && policy != BusyQueue && policy != BusyReject && policy != BusySteer {
		return nil, errors.New("bonnie: unsupported busy policy")
	}
	if policy == BusySteer && (len(in.Files) > 0 || len(in.Context) > 0) {
		return nil, errors.New("bonnie: durable steering supports text only")
	}
	r.workMu.Lock()
	defer r.workMu.Unlock()
	items, err := r.Submissions(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if requestID != "" && item.RequestID == requestID {
			return &item, nil
		}
	}
	state, err := r.journal.State(ctx, runID)
	if err != nil && !errors.Is(err, ErrRunNotFound) {
		return nil, err
	}
	if state == RunRetired {
		return nil, ErrRunRetired
	}
	if !errors.Is(err, ErrRunNotFound) {
		recs, replayErr := r.journal.Replay(ctx, runID)
		if replayErr != nil {
			return nil, replayErr
		}
		for _, rec := range recs {
			if rec.Kind == RecordOwnerCancel {
				return nil, errors.New("bonnie: run ownership is cancelled")
			}
		}
	}
	if policy == BusyReject && (r.IsActive(runID) || state == RunWaiting) {
		return nil, ErrRunActive
	}
	item := Submission{ID: rand.Text(), RunID: runID, RequestID: requestID, Input: in, State: SubmissionQueued, Policy: policy}
	if err := r.writeSubmission(ctx, RecordSubmission, item); err != nil {
		return nil, err
	}
	if policy == BusySteer {
		if err := r.Steer(runID, steerText(item)); err != nil && !errors.Is(err, ErrRunNotActive) {
			return nil, err
		}
	}
	return &item, nil
}

func (r *Runner) writeSubmission(ctx context.Context, kind RecordKind, item Submission) error {
	b, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("bonnie: encode submission: %w", err)
	}
	_, err = r.journal.Append(ctx, Record{RunID: item.RunID, Kind: kind, Timestamp: now(), Payload: b})
	return err
}

// Submissions returns the inbox and its settled inputs in admission order.
func (r *Runner) Submissions(ctx context.Context, runID string) ([]Submission, error) {
	recs, err := r.journal.Replay(ctx, runID)
	if errors.Is(err, ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Submission
	index := map[string]int{}
	for _, rec := range recs {
		if rec.Kind != RecordSubmission && rec.Kind != RecordSubmissionState {
			continue
		}
		var item Submission
		if err := json.Unmarshal(rec.Payload, &item); err != nil {
			return nil, fmt.Errorf("bonnie: decode submission: %w", err)
		}
		if i, ok := index[item.ID]; ok {
			items[i] = item
		} else {
			index[item.ID] = len(items)
			items = append(items, item)
		}
	}
	return items, nil
}

// AbortSubmission withdraws an input that has not started. Active work must
// be stopped with CancelOwned or Runner.Cancel.
func (r *Runner) AbortSubmission(ctx context.Context, runID, id string) error {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	items, err := r.Submissions(ctx, runID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		if item.State != SubmissionQueued {
			return errors.New("bonnie: submission already delivered")
		}
		item.State = SubmissionAborted
		return r.writeSubmission(ctx, RecordSubmissionState, item)
	}
	return ErrRunNotFound
}

// WaitSubmission waits for settlement. Cancelling the wait does not cancel
// execution. The caller must run RunScheduler separately.
func (r *Runner) WaitSubmission(ctx context.Context, runID, id string) (*Submission, error) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		items, err := r.Submissions(ctx, runID)
		if err != nil {
			return nil, err
		}
		found := false
		for _, item := range items {
			if item.ID == id {
				found = true
				if item.State != SubmissionQueued && item.State != SubmissionRunning {
					return &item, nil
				}
			}
		}
		if !found {
			return nil, ErrRunNotFound
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// RunScheduler delivers durable submissions, including work left running by
// a previous process. It leaves human waits parked. It does not retry failed
// work or old runs that have no submission record. Call it once per journal
// owner. Cancellation stops admission of more work and cancels owned turns.
func (r *Runner) RunScheduler(ctx context.Context) error {
	r.workMu.Lock()
	if r.scheduling {
		r.workMu.Unlock()
		return errors.New("bonnie: scheduler already running")
	}
	r.scheduling = true
	r.workMu.Unlock()
	defer func() { r.workMu.Lock(); r.scheduling = false; r.workMu.Unlock() }()
	workerCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	busy := sync.Map{}
	failures := make(chan error, 1)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids, err := r.journal.Runs(ctx, "")
		if err != nil {
			return err
		}
		for _, id := range ids {
			if IsReservedRun(id) || r.IsActive(id) {
				continue
			}
			if _, loaded := busy.LoadOrStore(id, true); loaded {
				continue
			}
			workers.Go(func() {
				defer busy.Delete(id)
				if err := r.recoverOwnedCancellation(workerCtx, id); err != nil {
					select {
					case failures <- err:
					default:
					}
					return
				}
				if err := r.deliverSubmission(workerCtx, id); err != nil {
					select {
					case failures <- err:
					default:
					}
				}
			})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-failures:
			return err
		case <-tick.C:
		}
	}
}

func (r *Runner) deliverSubmission(ctx context.Context, runID string) error {
	r.workMu.Lock()
	items, err := r.Submissions(ctx, runID)
	if err != nil {
		r.workMu.Unlock()
		return err
	}
	recs, err := r.journal.Replay(ctx, runID)
	if err != nil {
		r.workMu.Unlock()
		return err
	}
	for _, rec := range recs {
		if rec.Kind != RecordOwnerCancel {
			continue
		}
		for _, item := range items {
			if item.State == SubmissionQueued || item.State == SubmissionRunning {
				item.State = SubmissionAborted
				if err := r.writeSubmission(ctx, RecordSubmissionState, item); err != nil {
					r.workMu.Unlock()
					return err
				}
			}
		}
		r.workMu.Unlock()
		return nil
	}
	var selected *Submission
	for _, item := range items {
		if item.State == SubmissionQueued || item.State == SubmissionRunning {
			selected = &item
			break
		}
	}
	if selected == nil {
		r.workMu.Unlock()
		return nil
	}
	item := *selected
	if item.Policy == BusySteer && item.State == SubmissionQueued {
		placed, err := r.steerPlaced(ctx, item)
		if err != nil {
			r.workMu.Unlock()
			return err
		}
		if placed {
			item.State = SubmissionDone
			err = r.writeSubmission(ctx, RecordSubmissionState, item)
			r.workMu.Unlock()
			return err
		}
	}
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		r.workMu.Unlock()
		return err
	}
	if item.State == SubmissionRunning && (state.IsTerminal() || state == RunWaiting) {
		item.State = SubmissionDone
		if state != RunCompleted && state != RunWaiting {
			item.State = SubmissionFailed
		}
		err = r.writeSubmission(ctx, RecordSubmissionState, item)
		r.workMu.Unlock()
		return err
	}
	if state == RunRetired {
		item.State = SubmissionAborted
		err = r.writeSubmission(ctx, RecordSubmissionState, item)
		r.workMu.Unlock()
		return err
	}
	if state == RunWaiting {
		r.workMu.Unlock()
		return nil
	}
	turnCtx, act, acquireErr := r.acquire(ctx, runID)
	if acquireErr != nil {
		r.workMu.Unlock()
		if errors.Is(acquireErr, ErrRunActive) {
			return nil
		}
		return acquireErr
	}
	started := false
	defer func() {
		if !started {
			r.release(runID)
		}
	}()
	// The marker precedes Start. Recovery checks messages after this marker to
	// avoid placing the original user input twice after a process crash.
	input := item.Input
	if item.State == SubmissionRunning {
		recs, replayErr := r.journal.Replay(ctx, runID)
		if replayErr != nil {
			r.workMu.Unlock()
			return replayErr
		}
		marker := 0
		for _, rec := range recs {
			if rec.Kind == RecordSubmissionState {
				var x Submission
				if err := json.Unmarshal(rec.Payload, &x); err != nil {
					r.workMu.Unlock()
					return err
				}
				if x.ID == item.ID {
					marker = rec.Seq
				}
			}
		}
		for _, rec := range recs {
			if rec.Seq > marker && rec.Kind == RecordMessage && rec.Role == "user" {
				input.Text = "Continue the interrupted work from the saved conversation. Do not repeat completed external actions."
				input.Files = nil
				break
			}
		}
	} else {
		item.State = SubmissionRunning
		j, ok := r.journal.(StepJournal)
		if !ok {
			r.workMu.Unlock()
			return errors.New("bonnie: scheduler requires atomic step journal")
		}
		payload, err := json.Marshal(item)
		if err != nil {
			r.workMu.Unlock()
			return err
		}
		if _, err := j.AppendStep(ctx, []Record{
			{RunID: runID, Kind: RecordSubmissionState, Timestamp: now(), Payload: payload},
			{RunID: runID, Kind: RecordState, Timestamp: now(), State: RunPending},
		}); err != nil {
			r.workMu.Unlock()
			return err
		}
	}
	r.workMu.Unlock()
	started = true
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			act.cancel()
		case <-done:
		}
	}()
	run, startErr := r.startAcquired(turnCtx, act, runID, input)
	close(done)
	if errors.Is(startErr, ErrRunActive) || errors.Is(startErr, ErrRunWaiting) {
		return nil
	}
	item.State = SubmissionDone
	if startErr != nil {
		item.State, item.Error = SubmissionFailed, startErr.Error()
	} else if run != nil && run.State == RunCancelled {
		item.State = SubmissionAborted
	}
	r.workMu.Lock()
	defer r.workMu.Unlock()
	latest, err := r.Submissions(context.WithoutCancel(ctx), runID)
	if err != nil {
		return err
	}
	for _, current := range latest {
		if current.ID == item.ID && current.State == SubmissionAborted {
			return nil
		}
	}
	return r.writeSubmission(context.WithoutCancel(ctx), RecordSubmissionState, item)
}
