package runtime

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
