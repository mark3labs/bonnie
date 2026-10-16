package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// ErrNothingToContinue is returned by [Runner.Continue] when the run has no
// interrupted work: it is pending, waiting for a person, finished, or owned
// by the submission scheduler. It is not a failure.
var ErrNothingToContinue = errors.New("bonnie: run has no interrupted work")

// ErrRecoveryInputLost is returned by [Runner.Continue] when the process
// stopped after the turn started but before its input became durable. There
// is nothing to continue from, so the run is recorded as failed, and the
// person or program that sent the input must send it again. Without the
// failed state, every restart would find the same run and fail again.
var ErrRecoveryInputLost = errors.New("bonnie: the interrupted turn's input was not saved; send it again")

// DefaultRecoverConcurrency is how many runs [Runner.Recover] continues at
// the same time when [RecoverOptions.Concurrency] is zero.
const DefaultRecoverConcurrency = 4

// Continue continues a run whose turn has no stop reason: its state is
// [RunInterrupted], or [RunRunning] written by a process that is gone. It is
// the recovery that needs no input from the caller, which is what makes it
// safe to call for every such run at startup.
//
// Continue reads the journal and does the least work that ends the turn:
//
//   - a turn that halted for human input before its state was saved parks
//     again as [RunWaiting], with no model call;
//   - a turn whose final answer is durable completes with that answer, with
//     no model call;
//   - a human answer that is durable but did not reach the conversation is
//     sent to the model, as [Runner.Resume] would have done;
//   - otherwise the model continues the saved conversation through
//     [ContinuationAgent]. A tool call that the crash left without a result
//     gets the interrupted-tool result first, so an action with an unknown
//     effect is not repeated (see bonnie.WithReplaySafeTools).
//
// A run with a durable cancellation is recorded as cancelled, not continued.
// A run with queued or running submissions belongs to [Runner.RunScheduler]
// and Continue returns [ErrNothingToContinue] for it, as it does for every
// other state. Continue holds the run for the whole check, so a concurrent
// Start, StartRetry, or scheduler gets [ErrRunActive] and cannot run the same
// turn twice.
func (r *Runner) Continue(ctx context.Context, runID string) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if IsReservedRun(runID) {
		return nil, fmt.Errorf("%w: %s", ErrNothingToContinue, runID)
	}
	// Owner cancellation takes the run itself, so it runs before acquire.
	if err := r.recoverOwnedCancellation(ctx, runID); err != nil {
		return nil, err
	}
	turnCtx, act, err := r.acquire(ctx, runID)
	if err != nil {
		return nil, err
	}
	defer r.release(runID)
	if err := r.recoverCancellation(turnCtx, runID); err != nil {
		return nil, err
	}
	state, err := r.journal.State(turnCtx, runID)
	if err != nil {
		return nil, err
	}
	if state != RunRunning && state != RunInterrupted {
		return nil, fmt.Errorf("%w: %s is %s", ErrNothingToContinue, runID, state)
	}
	items, err := r.Submissions(turnCtx, runID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.State == SubmissionQueued || item.State == SubmissionRunning {
			return nil, fmt.Errorf("%w: %s belongs to the scheduler", ErrNothingToContinue, runID)
		}
	}

	recs, err := r.journal.Replay(turnCtx, runID)
	if err != nil {
		return nil, err
	}
	s, err := Restore(turnCtx, runID, r.journal)
	if err != nil {
		return nil, err
	}
	completion, err := replayCompletion(recs)
	if err != nil {
		return nil, err
	}
	plan := planContinuation(s, recs)
	turnID, _, _ := cancellationState(recs)
	book := context.WithoutCancel(turnCtx)

	switch {
	case plan.suspend != nil:
		if err := r.checkpoint(book, runID, RunWaiting); err != nil {
			return nil, err
		}
		plan.suspend.TurnID = turnID
		return &Run{ID: runID, TurnID: turnID, State: RunWaiting, Response: plan.response, Suspend: plan.suspend}, nil
	case completion != nil:
		// The completion record holds its own prompt and decides itself
		// whether that prompt is durable.
		s.SetTurnContext(plan.context)
		return r.turn(turnCtx, act, s, "", nil, false, false)
	case plan.finished != "":
		r.bus.Publish(Event{RunID: runID, TurnID: turnID, Type: EventResponse, Seq: s.LastMessageSeq(), Text: plan.finished})
		if err := r.checkpoint(book, runID, RunCompleted); err != nil {
			return nil, err
		}
		return &Run{ID: runID, TurnID: turnID, State: RunCompleted, Response: plan.finished}, nil
	case plan.answer != "":
		s.SetTurnContext(plan.context)
		return r.turn(turnCtx, act, s, plan.answer, nil, true, false)
	case plan.continueSaved:
		s.SetTurnContext(plan.context)
		return r.turn(turnCtx, act, s, "", nil, false, true)
	default:
		if err := r.checkpoint(book, runID, RunFailed); err != nil {
			return nil, err
		}
		return &Run{ID: runID, TurnID: turnID, State: RunFailed, Err: ErrRecoveryInputLost}, ErrRecoveryInputLost
	}
}

// continuation is what the journal says an interrupted turn still needs.
type continuation struct {
	// suspend is set when the turn halted for input but the waiting state
	// was not saved. response is the text the turn had produced.
	suspend  *SuspendRequest
	response string
	// finished is the final answer of a turn whose closing state was lost.
	finished string
	// answer is a durable human answer the model has not seen.
	answer string
	// continueSaved means the turn's input is in the conversation.
	continueSaved bool
	// context is the turn's per-turn context, which is not part of the
	// conversation and would otherwise be lost on recovery.
	context []string
}

// planContinuation reads the current logical turn: the records after the
// last terminal state or clear. A suspension or resume does not end a
// logical turn, because the turn continues after it.
func planContinuation(s *Session, recs []Record) continuation {
	var (
		plan                              continuation
		userSeq, resumeSeq, suspendSeq    int
		answer, response                  string
		suspendPayload                    json.RawMessage
		contextLines                      []string
		assistantSeq                      int
		assistantText                     string
		assistantCalls                    bool
		suspendSeen, resumeSeen, userSeen bool
	)
	for _, rec := range recs {
		if rec.Kind == RecordClear || (rec.Kind == RecordState && rec.State.IsTerminal()) {
			userSeq, resumeSeq, suspendSeq, assistantSeq = 0, 0, 0, 0
			answer, response, assistantText = "", "", ""
			suspendPayload, contextLines = nil, nil
			assistantCalls, suspendSeen, resumeSeen, userSeen = false, false, false, false
			continue
		}
		switch rec.Kind {
		case RecordMessage:
			switch rec.Role {
			case "user":
				userSeq, userSeen = rec.Seq, true
			case "assistant":
				assistantSeq = rec.Seq
				msg, err := decodeMessage(rec)
				if err == nil {
					assistantText = messageText(msg)
					assistantCalls = hasToolCall(msg)
					if assistantText != "" {
						response = assistantText
					}
				}
			}
		case RecordResume:
			resumeSeq, answer, resumeSeen = rec.Seq, rec.Text, true
		case RecordSuspend:
			suspendSeq, suspendPayload, suspendSeen = rec.Seq, rec.Payload, true
		case RecordContext:
			var payload contextPayload
			if json.Unmarshal(rec.Payload, &payload) == nil {
				contextLines = payload.Context
			}
		}
	}
	plan.context = contextLines
	input := max(userSeq, resumeSeq)
	if suspendSeen && suspendSeq > input {
		var sus SuspendRequest
		if err := json.Unmarshal(suspendPayload, &sus); err == nil {
			plan.suspend, plan.response = &sus, response
			return plan
		}
	}
	if resumeSeen && resumeSeq > userSeq {
		plan.answer = answer
		return plan
	}
	if !userSeen {
		return plan
	}
	// A final answer is the last message on the active branch: assistant
	// text with no tool call, after the turn's input.
	msgs := s.GetMessages()
	if assistantSeq > input && !assistantCalls && assistantText != "" && len(msgs) > 0 &&
		msgs[len(msgs)-1].Role == "assistant" && s.LastMessageSeq() == assistantSeq {
		plan.finished = assistantText
		return plan
	}
	plan.continueSaved = true
	return plan
}

func hasToolCall(msg kit.LLMMessage) bool {
	for _, part := range msg.Content {
		if _, ok := part.(kit.LLMToolCallPart); ok {
			return true
		}
	}
	return false
}

// Unfinished lists the runs whose turn has no stop reason: the state is
// [RunRunning] or [RunInterrupted]. Reserved runs are not listed. A run that
// waits for a person is not unfinished; it has a stop reason.
func (r *Runner) Unfinished(ctx context.Context) ([]string, error) {
	var ids []string
	for _, state := range []RunState{RunRunning, RunInterrupted} {
		got, err := r.journal.Runs(ctx, state)
		if err != nil {
			return nil, fmt.Errorf("bonnie: list unfinished runs: %w", err)
		}
		for _, id := range got {
			if !IsReservedRun(id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// RecoveredRun describes one run [Runner.Recover] found. Run and Err are set
// only in [RecoverOptions.Done].
type RecoveredRun struct {
	ID string
	// Origin is where the conversation lives, so the host can send the
	// result there.
	Origin Origin
	// Trigger is the current turn's trigger, or nil.
	Trigger *Trigger
	Run     *Run
	Err     error
}

// RecoverOptions configures [Runner.Recover].
type RecoverOptions struct {
	// Concurrency limits how many runs continue at the same time. Zero means
	// [DefaultRecoverConcurrency].
	Concurrency int
	// Skip reports whether another owner continues the run, for example a
	// broker that redelivers its message. Nil skips nothing.
	Skip func(RecoveredRun) bool
	// Done receives the outcome of each run that Recover continued. It is
	// called from the goroutine that continued the run.
	Done func(RecoveredRun)
}

// Recover continues every unfinished run with [Runner.Continue], with
// bounded concurrency, and returns when each one reaches a boundary. A
// served agent calls it at startup, so a run that a crash or a restart
// stopped continues without a new message.
//
// When ctx ends, Recover starts no more runs and interrupts the runs it
// continues; they stay [RunInterrupted] for the next process. A run that was
// interrupted again is not reported to Done: it has no outcome yet.
func (r *Runner) Recover(ctx context.Context, opts RecoverOptions) error {
	ids, err := r.Unfinished(ctx)
	if err != nil {
		return err
	}
	limit := opts.Concurrency
	if limit <= 0 {
		limit = DefaultRecoverConcurrency
	}
	var (
		mu       sync.Mutex
		launched = map[string]bool{}
		wg       sync.WaitGroup
	)
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		for id := range launched {
			_ = r.Interrupt(id)
		}
	})
	defer stop()
	slots := make(chan struct{}, limit)
	defer wg.Wait()
	for _, id := range ids {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		info, err := recoveryInfo(ctx, r.journal, id)
		if err != nil {
			<-slots
			if opts.Done != nil {
				opts.Done(RecoveredRun{ID: id, Err: err})
			}
			continue
		}
		if opts.Skip != nil && opts.Skip(info) {
			<-slots
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			mu.Lock()
			launched[id] = true
			mu.Unlock()
			run, err := r.Continue(ctx, id)
			mu.Lock()
			delete(launched, id)
			mu.Unlock()
			if errors.Is(err, ErrNothingToContinue) || errors.Is(err, ErrRunActive) ||
				errors.Is(err, ErrRunnerDraining) || (err != nil && ctx.Err() != nil) {
				return
			}
			if err == nil && run != nil && run.State == RunInterrupted {
				return
			}
			info.Run, info.Err = run, err
			if opts.Done != nil {
				opts.Done(info)
			}
		})
	}
	return nil
}

// recoveryInfo reads a run's origin and current trigger from its records.
// It does not restore the session: restore can write a repair record, and a
// run that another owner continues must not get a write from here.
func recoveryInfo(ctx context.Context, j Journal, runID string) (RecoveredRun, error) {
	info := RecoveredRun{ID: runID}
	recs, err := j.Replay(ctx, runID)
	if err != nil {
		return info, err
	}
	originFound := false
	for _, rec := range recs {
		switch {
		case rec.Kind == RecordExtensionData && rec.ExtType == ExtOrigin && !originFound:
			if json.Unmarshal([]byte(rec.Text), &info.Origin) == nil {
				originFound = true
			}
		case rec.Kind == RecordTrigger:
			var trigger *Trigger
			if err := json.Unmarshal(rec.Payload, &trigger); err != nil {
				return info, fmt.Errorf("bonnie: decode trigger: %w", err)
			}
			info.Trigger = trigger
		}
	}
	return info, nil
}
