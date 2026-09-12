package runtime

import (
	"context"
	"slices"
	"sync"
)

// MemoryJournal is an in-memory [Journal]. It is the default for tests and for
// ephemeral runs. Data is lost when the process exits — use a persistent
// journal for production.
type MemoryJournal struct {
	mu      sync.RWMutex
	records map[string][]Record
	states  map[string]RunState
}

// NewMemoryJournal returns an empty in-memory journal.
func NewMemoryJournal() *MemoryJournal {
	return &MemoryJournal{
		records: make(map[string][]Record),
		states:  make(map[string]RunState),
	}
}

var _ Journal = (*MemoryJournal)(nil)

// Append implements [Journal].
func (j *MemoryJournal) Append(_ context.Context, rec Record) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	rec.Seq = len(j.records[rec.RunID]) + 1
	j.records[rec.RunID] = append(j.records[rec.RunID], rec)
	if _, ok := j.states[rec.RunID]; !ok {
		j.states[rec.RunID] = RunPending
	}
	return rec.Seq, nil
}

// Replay implements [Journal].
func (j *MemoryJournal) Replay(_ context.Context, runID string) ([]Record, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()

	recs, ok := j.records[runID]
	if !ok {
		return nil, ErrRunNotFound
	}
	out := make([]Record, len(recs))
	copy(out, recs)
	return out, nil
}

// Checkpoint implements [Journal]. The state change and the record that
// documents it are written under one lock, so a reader never sees a state that
// has no record behind it.
func (j *MemoryJournal) Checkpoint(_ context.Context, runID string, state RunState) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.states[runID] = state
	rec := Record{
		RunID: runID, Kind: RecordState, State: state,
		Timestamp: now(), Seq: len(j.records[runID]) + 1,
	}
	j.records[runID] = append(j.records[runID], rec)
	return nil
}

// State implements [Journal].
func (j *MemoryJournal) State(_ context.Context, runID string) (RunState, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()

	s, ok := j.states[runID]
	if !ok {
		return "", ErrRunNotFound
	}
	return s, nil
}

// Runs implements [Journal].
func (j *MemoryJournal) Runs(_ context.Context, state RunState) ([]string, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()

	var out []string
	for id, s := range j.states {
		if state == "" || s == state {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Persisted implements [Journal]. An in-memory journal never survives the
// process, so it always reports false.
func (j *MemoryJournal) Persisted() bool { return false }

// Close implements [Journal].
func (j *MemoryJournal) Close() error { return nil }
