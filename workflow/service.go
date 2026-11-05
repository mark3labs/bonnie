package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/bonnie/runtime"
)

// Service executes workflows against a host-owned journal. Exactly one service
// may own a workflow journal namespace. It does not close the journal. It has
// no distributed lease or cross-process execution claims.
//
// Run drives execution. Start only admits work. Tick is available for hosts
// and tests that drive execution themselves. Shutdown cancels callbacks and
// waits for them; callbacks must honor their context.
type Service struct {
	mu          sync.Mutex
	journal     runtime.Journal
	definitions map[string]definition
	runs        map[string]snapshot
	active      map[string]context.CancelFunc
	loaded      bool
	closed      bool
	wg          sync.WaitGroup
	changed     chan struct{}
	now         func() time.Time
	poll        time.Duration
	fatal       error
	running     bool
}

// NewService constructs an embeddable workflow service. Definitions must be
// unique by name and version. No background goroutine starts here.
func NewService(j runtime.Journal, definitions ...Workflow) (*Service, error) {
	if j == nil {
		return nil, errors.New("bonnie: workflow requires a journal")
	}
	s := &Service{journal: j, definitions: make(map[string]definition), runs: make(map[string]snapshot), active: make(map[string]context.CancelFunc), changed: make(chan struct{}), now: time.Now, poll: 20 * time.Millisecond}
	for _, w := range definitions {
		if w == nil {
			return nil, errors.New("bonnie: nil workflow definition")
		}
		d := w.workflowDefinition()
		if d.name == "" || d.version == "" || strings.ContainsRune(d.name, 0) || strings.ContainsRune(d.version, 0) || d.invoke == nil {
			return nil, errors.New("bonnie: workflow name, version and function are required")
		}
		key := definitionKey(d.name, d.version)
		if _, exists := s.definitions[key]; exists {
			return nil, fmt.Errorf("bonnie: duplicate workflow %q version %q", d.name, d.version)
		}
		s.definitions[key] = d
	}
	return s, nil
}
func definitionKey(name, version string) string { return name + "\x00" + version }
func requestRunID(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return runPrefix + hex.EncodeToString(sum[:])
}
func (s *Service) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }

func (s *Service) loadLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.loaded {
		return nil
	}
	ids, err := s.journal.Runs(ctx, "")
	if err != nil {
		return fmt.Errorf("bonnie: list workflows: %w", err)
	}
	loaded := make(map[string]snapshot)
	for _, id := range ids {
		if !strings.HasPrefix(id, runPrefix) {
			continue
		}
		records, err := s.journal.Replay(ctx, id)
		if err != nil {
			return fmt.Errorf("bonnie: read workflow: %w", err)
		}
		var state snapshot
		found := false
		for _, r := range records {
			if r.Kind == snapshotKind {
				state = snapshot{}
				if err := json.Unmarshal(r.Payload, &state); err != nil {
					return fmt.Errorf("bonnie: decode workflow: %w", err)
				}
				found = true
			}
		}
		if !found {
			continue
		}
		if state.Format != 1 || state.ID != id || requestRunID(state.RequestID) != id {
			return fmt.Errorf("bonnie: invalid workflow snapshot %q", id)
		}
		loaded[id] = state
	}
	s.runs = loaded
	s.loaded = true
	return nil
}

func (s *Service) writableLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return errors.New("bonnie: workflow service is closed")
	}
	return s.fatal
}

// saveLocked publishes memory only after the single snapshot record is durable.
func (s *Service) saveLocked(ctx context.Context, st snapshot) error {
	if s.fatal != nil {
		return s.fatal
	}

	payload, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("bonnie: encode workflow snapshot: %w", err)
	}
	if _, err = s.journal.Append(ctx, runtime.Record{RunID: st.ID, Kind: snapshotKind, Payload: payload, Timestamp: s.now()}); err != nil {
		s.fatal = fmt.Errorf("bonnie: save workflow snapshot: %w", err)
		s.notifyLocked()
		return s.fatal
	}
	s.runs[st.ID] = st
	s.notifyLocked()
	return nil
}

// Run polls durable workflows until ctx ends or storage fails. On return all
// callbacks have stopped. Cancellation of ctx interrupts execution for later
// recovery; it does not cancel the durable runs. Do not call Run concurrently.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running || s.closed {
		s.mu.Unlock()
		return errors.New("bonnie: workflow service is running or closed")
	}
	s.running = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()

	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		if err := s.Tick(ctx); err != nil {
			return errors.Join(err, s.Shutdown(context.Background()))
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), s.Shutdown(context.Background()))
		case <-ticker.C:
		}
	}
}

// Tick starts each ready run once, without waiting for its callback. It also
// recovers runs from the journal on the first call. A missing definition
// leaves its runs unchanged so that a later binary can recover them.
func (s *Service) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("bonnie: workflow service is closed")
	}
	if s.fatal != nil {
		return s.fatal
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.loadLocked(ctx); err != nil {
		return err
	}
	for id, st := range s.runs {
		if terminal(st.State) || st.State == Blocked || s.active[id] != nil {
			continue
		}
		d, ok := s.definitions[definitionKey(st.Name, st.Version)]
		if !ok {
			continue
		}
		if st.InputType != d.inputType || st.ResultType != d.resultType {
			return fmt.Errorf("%w: definition types for %s", ErrReplay, id)
		}
		if !s.readyLocked(st) {
			continue
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.active[id] = cancel
		s.wg.Add(1)
		go s.execute(runCtx, id, d)
	}
	return nil
}
func (s *Service) readyLocked(st snapshot) bool {
	if len(st.Operations) == 0 {
		return true
	}
	op := st.Operations[len(st.Operations)-1]
	if op.State == "retry" || (op.Kind == "sleep" && op.State != "success") {
		return !s.now().Before(op.Due)
	}
	if op.Kind == "wait" && op.State != "success" && op.State != "error" {
		for _, m := range st.Messages {
			if !m.Consumed && m.Key == op.Key {
				return true
			}
		}
		return false
	}
	return true
}

// Shutdown stops this service and interrupts active callbacks. It does not
// write durable cancellation. If ctx ends, callbacks can still be running;
// the host must not close the journal until they have returned.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	for _, cancel := range s.active {
		cancel()
	}
	s.notifyLocked()
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) execute(ctx context.Context, id string, d definition) {
	defer func() {
		s.mu.Lock()
		if cancel := s.active[id]; cancel != nil {
			cancel()
		}
		delete(s.active, id)
		s.notifyLocked()
		s.mu.Unlock()
		s.wg.Done()
	}()
	e := &execution{service: s, id: id}
	var result json.RawMessage
	var err error
	parked := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				if stop, ok := p.(park); ok {
					parked = true
					err = stop.err
				} else if fault, ok := p.(replayFault); ok {
					err = fault.err
				} else {
					err = fmt.Errorf("bonnie: workflow panic: %v", p)
				}
			}
		}()
		s.mu.Lock()
		st := clone(s.runs[id])
		if terminal(st.State) {
			s.mu.Unlock()
			parked = true
			return
		}
		st.State = Running
		err = s.saveLocked(context.WithoutCancel(ctx), st)
		s.mu.Unlock()
		if err != nil {
			panic(park{err})
		}
		result, err = d.invoke(context.WithValue(ctx, executionKey{}, e), st.Input)
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := clone(s.runs[id])
	if terminal(st.State) {
		return
	}
	if err != nil && parked {
		s.fatal = err
		return
	}
	if parked || ctx.Err() != nil {
		return
	}
	if e.position != len(st.Operations) {
		err = fmt.Errorf("%w: function returned before saved operations", ErrReplay)
	}
	st.Result = result
	st.State = Completed
	st.Error = ""
	if err != nil {
		st.State = Failed
		st.Result = nil
		st.Error = err.Error()
	}
	if saveErr := s.saveLocked(context.WithoutCancel(ctx), st); saveErr != nil {
		s.fatal = saveErr
	}
}

// Signal durably adds a mailbox value. It can arrive before Wait. IDs are
// unique within a run; identical resends succeed, conflicting resends fail.
// Signals sent to terminal runs are rejected, except identical resends.
func (s *Service) Signal(ctx context.Context, id, key, signalID string, value any) error {
	if key == "" || signalID == "" {
		return errors.New("bonnie: signal key and ID must not be empty")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("bonnie: encode signal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(ctx); err != nil {
		return err
	}
	if err := s.loadLocked(ctx); err != nil {
		return err
	}
	st, ok := s.runs[id]
	if !ok {
		return ErrNotFound
	}
	st = clone(st)
	for _, m := range st.Messages {
		if m.ID == signalID {
			if m.Key != key || !bytes.Equal(m.Value, raw) {
				return ErrConflict
			}
			return nil
		}
	}
	if terminal(st.State) {
		return ErrTerminal
	}
	st.Messages = append(st.Messages, message{Key: key, ID: signalID, Value: raw})
	return s.saveLocked(ctx, st)
}

// Status reads the latest durable state, including a blocked step key.
func (s *Service) Status(ctx context.Context, id string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx); err != nil {
		return Status{}, err
	}
	st, ok := s.runs[id]
	if !ok {
		return Status{}, ErrNotFound
	}
	status := Status{ID: id, Name: st.Name, Version: st.Version, State: st.State, Error: st.Error}
	if st.State == Blocked {
		for _, op := range st.Operations {
			if op.State == "unknown" {
				status.BlockedKey = op.Key
				break
			}
		}
	}
	return status, nil
}

// Cancel durably cancels execution, then cancels an active callback context.
// It cannot undo external effects. Terminal runs are left unchanged.
func (s *Service) Cancel(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(ctx); err != nil {
		return err
	}
	if err := s.loadLocked(ctx); err != nil {
		return err
	}
	st, ok := s.runs[id]
	if !ok {
		return ErrNotFound
	}
	if terminal(st.State) {
		return nil
	}
	st = clone(st)
	st.State = Cancelled
	st.Error = ErrCancelled.Error()
	if err := s.saveLocked(ctx, st); err != nil {
		return err
	}
	if cancel := s.active[id]; cancel != nil {
		cancel()
	}
	return nil
}

// resolve checks the saved Go result type before it changes a blocked step.
func (s *Service) resolve(ctx context.Context, id, key, typeName string, result json.RawMessage, stepErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(ctx); err != nil {
		return err
	}
	if err := s.loadLocked(ctx); err != nil {
		return err
	}
	st, ok := s.runs[id]
	if !ok {
		return ErrNotFound
	}
	st = clone(st)
	if st.State != Blocked || s.active[id] != nil {
		return errors.New("bonnie: workflow is not available for reconciliation")
	}
	for i := range st.Operations {
		op := &st.Operations[i]
		if op.Key != key || op.State != "unknown" {
			continue
		}
		if op.Type != typeName {
			return ErrReplay
		}
		op.State = "success"
		op.Result = result
		op.Error = ""
		if stepErr != nil {
			op.State = "error"
			op.Result = nil
			op.Error = stepErr.Error()
		}
		st.State = Pending
		st.Error = ""
		return s.saveLocked(ctx, st)
	}
	return errors.New("bonnie: unknown reconciliation step")
}

// Resolve supplies the verified outcome of a blocked step. A non-nil error
// becomes that step's durable error. Resolve never repeats the external action.
func Resolve[T any](ctx context.Context, s *Service, id, key string, value T, stepErr error) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("bonnie: encode reconciliation: %w", err)
	}
	return s.resolve(ctx, id, key, typeName[T](), raw, stepErr)
}
