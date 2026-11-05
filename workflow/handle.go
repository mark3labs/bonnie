package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Handle addresses an admitted workflow. Waiting on a handle does not own
// execution. Cancelling a Result context cancels that wait only.
type Handle[T any] struct {
	service *Service
	id      string
}

// ID returns the durable run ID.
func (h *Handle[T]) ID() string { return h.id }

// Start durably admits a typed workflow input. requestID is required and
// unique across the service namespace. Identical requests return the same
// handle, including after a restart. Different content is ErrConflict.
// The caller context controls admission only, never execution.
func Start[I, O any](ctx context.Context, s *Service, d *Definition[I, O], requestID string, input I) (*Handle[O], error) {
	if s == nil || d == nil || requestID == "" {
		return nil, errors.New("bonnie: service, definition and request ID are required")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("bonnie: encode workflow input: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(ctx); err != nil {
		return nil, err
	}
	registered, ok := s.definitions[definitionKey(d.def.name, d.def.version)]
	if !ok || registered.inputType != d.def.inputType || registered.resultType != d.def.resultType {
		return nil, errors.New("bonnie: workflow definition is not registered")
	}
	if err := s.loadLocked(ctx); err != nil {
		return nil, err
	}
	id := requestRunID(requestID)
	if old, ok := s.runs[id]; ok {
		if old.RequestID != requestID || old.Name != d.def.name || old.Version != d.def.version || old.InputType != d.def.inputType || old.ResultType != d.def.resultType || !bytes.Equal(old.Input, raw) {
			return nil, ErrConflict
		}
		return &Handle[O]{service: s, id: id}, nil
	}
	st := snapshot{Format: 1, ID: id, RequestID: requestID, Name: d.def.name, Version: d.def.version, InputType: d.def.inputType, ResultType: d.def.resultType, Input: raw, State: Pending}
	if err := s.saveLocked(ctx, st); err != nil {
		return nil, err
	}
	return &Handle[O]{service: s, id: id}, nil
}

// Open returns a typed handle for an existing run. Its result type must match
// the admitted definition. It does not require that definition to be registered.
func Open[T any](ctx context.Context, s *Service, id string) (*Handle[T], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx); err != nil {
		return nil, err
	}
	st, ok := s.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if st.ResultType != typeName[T]() {
		return nil, ErrReplay
	}
	return &Handle[T]{service: s, id: id}, nil
}

// Status reads the durable run state.
func (h *Handle[T]) Status(ctx context.Context) (Status, error) { return h.service.Status(ctx, h.id) }

// Cancel durably cancels execution. It is distinct from cancelling Result's context.
func (h *Handle[T]) Cancel(ctx context.Context) error { return h.service.Cancel(ctx, h.id) }

// Signal sends a deduplicated mailbox value to this run.
func (h *Handle[T]) Signal(ctx context.Context, key, id string, value any) error {
	return h.service.Signal(ctx, h.id, key, id, value)
}

// Result waits for the durable result or a terminal error. Blocked workflows
// keep waiting for reconciliation. ctx cancellation stops only this wait.
func (h *Handle[T]) Result(ctx context.Context) (T, error) {
	var zero T
	s := h.service
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		s.mu.Lock()
		if err := s.loadLocked(ctx); err != nil {
			s.mu.Unlock()
			return zero, err
		}
		st, ok := s.runs[h.id]
		changed := s.changed
		fatal := s.fatal
		closed := s.closed
		s.mu.Unlock()
		if !ok {
			return zero, ErrNotFound
		}
		switch st.State {
		case Completed:
			var value T
			if err := json.Unmarshal(st.Result, &value); err != nil {
				return zero, fmt.Errorf("bonnie: decode workflow result: %w", err)
			}
			return value, nil
		case Failed:
			return zero, errors.New(st.Error)
		case Cancelled:
			return zero, ErrCancelled
		}
		if fatal != nil {
			return zero, fatal
		}
		if closed {
			return zero, errors.New("bonnie: workflow service is closed")
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-changed:
		}
	}
}
