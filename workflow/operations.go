package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type executionKey struct{}
type execution struct {
	service  *Service
	id       string
	position int
}
type park struct{ err error }
type replayFault struct{ err error }

func (f replayFault) Error() string { return f.err.Error() }

func executionFrom(ctx context.Context) (*execution, error) {
	e, ok := ctx.Value(executionKey{}).(*execution)
	if !ok {
		return nil, ErrNotWorkflow
	}
	return e, nil
}

// nextLocked checks ordered operation identity. Operation keys must be unique
// within a run. Reusing a key, also for another kind, is a deterministic error.
func (e *execution) nextLocked(st *snapshot, want operation) *operation {
	if want.Key == "" {
		panic(replayFault{fmt.Errorf("%w: empty operation key", ErrReplay)})
	}
	pos := e.position
	e.position++
	if pos < len(st.Operations) {
		op := &st.Operations[pos]
		if op.Key != want.Key || op.Kind != want.Kind || op.Type != want.Type || op.Config != want.Config || op.Duration != want.Duration {
			panic(replayFault{fmt.Errorf("%w: operation %d (%q)", ErrReplay, pos, want.Key)})
		}
		return op
	}
	for _, op := range st.Operations {
		if op.Key == want.Key {
			panic(replayFault{fmt.Errorf("%w: duplicate key %q", ErrReplay, want.Key)})
		}
	}
	st.Operations = append(st.Operations, want)
	return &st.Operations[pos]
}
func (e *execution) saveLocked(ctx context.Context, st snapshot) {
	if err := e.service.saveLocked(context.WithoutCancel(ctx), st); err != nil {
		panic(park{err})
	}
}
func (e *execution) checkLocked(ctx context.Context) snapshot {
	st := clone(e.service.runs[e.id])
	if terminal(st.State) || ctx.Err() != nil {
		panic(park{})
	}
	return st
}
func decodeResult[T any](op operation) (T, error) {
	var value T
	if op.State == "error" {
		return value, errors.New(op.Error)
	}
	if err := json.Unmarshal(op.Result, &value); err != nil {
		panic(replayFault{fmt.Errorf("%w: decode %q: %v", ErrReplay, op.Key, err)})
	}
	return value, nil
}

// Step executes callback only when there is no saved outcome. The pending
// attempt is saved before the callback starts; success is saved before Step
// returns. An interrupted unsafe attempt blocks for Resolve, not automatic
// repetition. Callback errors are saved and returned to the workflow.
//
// Call Step only from the workflow's main goroutine. Do not call workflow
// operations from callbacks. Keep external effects inside callbacks.
func Step[T any](ctx context.Context, key string, callback func(context.Context, Attempt) (T, error), options ...StepOption) (T, error) {
	var zero T
	e, err := executionFrom(ctx)
	if err != nil {
		return zero, err
	}
	cfg := stepConfig{Retry: RetryPolicy{MaxAttempts: 1}}
	for _, opt := range options {
		if opt != nil {
			opt(&cfg)
		}
	}
	if err := cfg.validate(); err != nil {
		panic(replayFault{err})
	}
	if callback == nil {
		panic(replayFault{errors.New("bonnie: nil step callback")})
	}
	s := e.service
	var attempt Attempt
	var index int
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		st := e.checkLocked(ctx)
		op := e.nextLocked(&st, operation{Key: key, Kind: "step", Type: typeName[T](), Config: cfg, State: "ready"})
		index = e.position - 1
		if op.State == "success" || op.State == "error" {
			return
		}
		if op.State == "pending" {
			if !cfg.RepeatSafe {
				op.State = "unknown"
				st.State = Blocked
				st.Error = "step outcome is unknown: " + key
				e.saveLocked(ctx, st)
				panic(park{})
			}
			if op.Attempts >= cfg.Retry.MaxAttempts {
				op.State = "error"
				op.Error = "bonnie: retry attempts exhausted after interrupted attempt"
				e.saveLocked(ctx, st)
				return
			}
			op.State = "retry"
			op.Due = s.now().Add(cfg.delay(op.Attempts))
			st.State = Waiting
			e.saveLocked(ctx, st)
			panic(park{})
		}
		if op.State == "unknown" {
			panic(park{})
		}
		if op.State == "retry" && s.now().Before(op.Due) {
			st.State = Waiting
			e.saveLocked(ctx, st)
			panic(park{})
		}
		op.State = "pending"
		op.Attempts++
		op.Error = ""
		op.Due = time.Time{}
		hash := sha256.Sum256([]byte(e.id + "\x00" + key))
		attempt = Attempt{Number: op.Attempts, IdempotencyKey: hex.EncodeToString(hash[:])}
		e.saveLocked(ctx, st)
	}()
	if attempt.Number == 0 {
		s.mu.Lock()
		op := s.runs[e.id].Operations[index]
		s.mu.Unlock()
		return decodeResult[T](op)
	}
	callbackCtx := context.WithValue(ctx, executionKey{}, nil)
	cancel := func() {}
	if cfg.Timeout > 0 {
		callbackCtx, cancel = context.WithTimeout(callbackCtx, cfg.Timeout)
	}
	var value T
	var callbackErr error
	var panicked bool
	func() {
		defer func() {
			if p := recover(); p != nil {
				panicked = true
			}
		}()
		value, callbackErr = callback(callbackCtx, attempt)
	}()
	if callbackCtx.Err() != nil && callbackErr == nil {
		callbackErr = callbackCtx.Err()
	}
	cancel()
	var raw json.RawMessage
	if callbackErr == nil && !panicked {
		raw, callbackErr = json.Marshal(value)
		if callbackErr != nil {
			callbackErr = Permanent(fmt.Errorf("bonnie: encode step result: %w", callbackErr))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := clone(s.runs[e.id])
	if terminal(st.State) {
		panic(park{})
	}
	op := &st.Operations[index]
	if panicked || (ctx.Err() != nil && callbackErr != nil) {
		panic(park{})
	}
	if callbackErr == nil {
		op.State = "success"
		op.Result = raw
		e.saveLocked(ctx, st)
		return decodeResult[T](*op)
	}
	op.Error = callbackErr.Error()
	if cfg.RepeatSafe && !isPermanent(callbackErr) && op.Attempts < cfg.Retry.MaxAttempts {
		op.State = "retry"
		op.Due = s.now().Add(cfg.delay(op.Attempts))
		st.State = Waiting
		e.saveLocked(ctx, st)
		panic(park{})
	}
	op.State = "error"
	e.saveLocked(ctx, st)
	return decodeResult[T](*op)
}

// Sleep saves its deadline once, then parks without holding a worker or
// goroutine. Replays use the original deadline, not a new duration from now.
func Sleep(ctx context.Context, key string, duration time.Duration) error {
	e, err := executionFrom(ctx)
	if err != nil {
		return err
	}
	if duration < 0 {
		panic(replayFault{errors.New("bonnie: negative workflow sleep")})
	}
	s := e.service
	s.mu.Lock()
	defer s.mu.Unlock()
	st := e.checkLocked(ctx)
	op := e.nextLocked(&st, operation{Key: key, Kind: "sleep", Duration: duration, State: "ready"})
	if op.State == "success" {
		return nil
	}
	if op.Due.IsZero() {
		op.Due = s.now().Add(duration)
	}
	if s.now().Before(op.Due) {
		st.State = Waiting
		e.saveLocked(ctx, st)
		panic(park{})
	}
	op.State = "success"
	e.saveLocked(ctx, st)
	return nil
}

// Wait consumes the first unconsumed signal for key. It saves consumption and
// the typed result together before returning. An early signal is supported.
// A JSON decoding error is a durable operation error, not a lost signal.
func Wait[T any](ctx context.Context, key string) (T, error) {
	var zero T
	e, err := executionFrom(ctx)
	if err != nil {
		return zero, err
	}
	s := e.service
	s.mu.Lock()
	defer s.mu.Unlock()
	st := e.checkLocked(ctx)
	op := e.nextLocked(&st, operation{Key: key, Kind: "wait", Type: typeName[T](), State: "ready"})
	if op.State == "success" || op.State == "error" {
		return decodeResult[T](*op)
	}
	for i := range st.Messages {
		m := &st.Messages[i]
		if m.Consumed || m.Key != key {
			continue
		}
		m.Consumed = true
		var value T
		if err := json.Unmarshal(m.Value, &value); err != nil {
			op.State = "error"
			op.Error = fmt.Sprintf("bonnie: decode signal %q: %v", key, err)
			e.saveLocked(ctx, st)
			return zero, errors.New(op.Error)
		}
		op.State = "success"
		op.Result = m.Value
		e.saveLocked(ctx, st)
		return value, nil
	}
	st.State = Waiting
	e.saveLocked(ctx, st)
	panic(park{})
}
