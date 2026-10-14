package runtime

import (
	"cmp"
	"context"
	"fmt"
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
	byKind  map[RecordKind]map[string][]Record
}

// NewMemoryJournal returns an empty in-memory journal.
func NewMemoryJournal() *MemoryJournal {
	return &MemoryJournal{
		records: make(map[string][]Record),
		states:  make(map[string]RunState),
		byKind:  make(map[RecordKind]map[string][]Record),
	}
}

var (
	_ Journal     = (*MemoryJournal)(nil)
	_ KindReader  = (*MemoryJournal)(nil)
	_ Positioner  = (*MemoryJournal)(nil)
	_ StepJournal = (*MemoryJournal)(nil)
)

// Position implements [Positioner].
func (j *MemoryJournal) Position(_ context.Context, runID string) (int, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.records[runID]), nil
}

// Append implements [Journal].
func (j *MemoryJournal) Append(_ context.Context, rec Record) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	rec.Payload = slices.Clone(rec.Payload)
	rec.Seq = len(j.records[rec.RunID]) + 1
	j.records[rec.RunID] = append(j.records[rec.RunID], rec)
	j.indexRecord(rec)
	if rec.Kind == RecordState {
		j.states[rec.RunID] = rec.State
	}
	if _, ok := j.states[rec.RunID]; !ok {
		j.states[rec.RunID] = RunPending
	}
	return rec.Seq, nil
}

// AppendStep implements [StepJournal]. Every record lands under one lock, so
// a reader never observes part of a step. An in-memory journal cannot be
// torn by a crash — the process and the journal die together — so this is
// about keeping the step a unit for concurrent readers, and about giving
// [Session] one code path instead of a durable and a volatile one.
func (j *MemoryJournal) AppendStep(_ context.Context, recs []Record) ([]int, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	// One step belongs to one run; see SQLiteJournal.AppendStep.
	for i := range recs {
		if recs[i].RunID != recs[0].RunID {
			return nil, fmt.Errorf("bonnie: append step: record %d is for run %q, not %q",
				i+1, recs[i].RunID, recs[0].RunID)
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	seqs := make([]int, len(recs))
	base := len(j.records[recs[0].RunID])
	for i := range recs {
		rec := recs[i]
		rec.Payload = slices.Clone(rec.Payload)
		rec.Seq = base + i + 1
		seqs[i] = rec.Seq
		j.records[recs[i].RunID] = append(j.records[rec.RunID], rec)
		j.indexRecord(rec)
		if rec.Kind == RecordState {
			j.states[rec.RunID] = rec.State
		}
	}
	if _, ok := j.states[recs[0].RunID]; !ok {
		j.states[recs[0].RunID] = RunPending
	}
	return seqs, nil
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
	for i := range out {
		out[i].Payload = slices.Clone(out[i].Payload)
	}
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
	j.indexRecord(rec)
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

// indexRecord shares immutable records with the kind index. Call under mu.
func (j *MemoryJournal) indexRecord(rec Record) {
	if j.byKind[rec.Kind] == nil {
		j.byKind[rec.Kind] = make(map[string][]Record)
	}
	j.byKind[rec.Kind][rec.RunID] = append(j.byKind[rec.Kind][rec.RunID], rec)
}

// ReplayKinds implements [KindReader].
func (j *MemoryJournal) ReplayKinds(_ context.Context, runID string, kinds ...RecordKind) ([]Record, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	var out []Record
	seen := make(map[RecordKind]bool)
	for _, kind := range kinds {
		if seen[kind] {
			continue
		}
		seen[kind] = true
		for _, rec := range j.byKind[kind][runID] {
			rec.Payload = slices.Clone(rec.Payload)
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b Record) int { return cmp.Compare(a.Seq, b.Seq) })
	return out, nil
}

// RunsWithKinds implements [KindReader].
func (j *MemoryJournal) RunsWithKinds(_ context.Context, kinds ...RecordKind) ([]string, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	ids := make(map[string]bool)
	for _, kind := range kinds {
		for id := range j.byKind[kind] {
			ids[id] = true
		}
	}
	var out []string
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

// RecordsOfKind implements [KindReader].
func (j *MemoryJournal) RecordsOfKind(_ context.Context, kind RecordKind) ([]Record, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	var ids []string
	for id := range j.byKind[kind] {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var out []Record
	for _, id := range ids {
		for _, rec := range j.byKind[kind][id] {
			rec.Payload = slices.Clone(rec.Payload)
			out = append(out, rec)
		}
	}
	return out, nil
}
