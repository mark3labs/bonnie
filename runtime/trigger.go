package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
)

// CurrentTrigger returns this turn's trigger, or nil for ordinary input.
// Resume retains the suspended turn's trigger. A fresh human turn clears it.
// The returned copy can be changed without changing the session.
func (s *Session) CurrentTrigger() *Trigger {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.trigger == nil {
		return nil
	}
	value := *s.trigger
	return &value
}

// Caller returns the identity that sent the current turn's input, or nil.
// Resume keeps the suspended turn's caller. The returned copy can be changed
// without changing the session.
func (s *Session) Caller() *Caller {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.caller == nil {
		return nil
	}
	value := *s.caller
	value.Attributes = maps.Clone(value.Attributes)
	return &value
}

// recordCaller journals the turn's caller before the session exposes it.
// Nothing is written when the caller does not change from nil.
func (s *Session) recordCaller(ctx context.Context, c *Caller) error {
	if c == nil && s.Caller() == nil {
		return nil
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("bonnie: encode caller: %w", err)
	}
	if _, err := s.journal.Append(ctx, Record{RunID: s.runID, Kind: RecordCaller, Timestamp: now(), Payload: payload}); err != nil {
		return err
	}
	s.mu.Lock()
	s.caller = c
	s.mu.Unlock()
	return nil
}
