// Package presence provides transport-independent records and interfaces for
// agents that advertise their identity, endpoints, readiness, labels, and state.
// It does not assign work or manage claims.
package presence

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"
)

// ErrNotFound means that no presence record exists for the requested identity.
var ErrNotFound = errors.New("presence: record not found")

// ErrConflict means another instance already owns this agent identity.
var ErrConflict = errors.New("presence: agent is already registered")

// Identity identifies one agent process instance. Instance must distinguish
// simultaneous or successive instances of the same agent.
type Identity struct {
	Agent    string `json:"agent"`
	Instance string `json:"instance"`
}

// Endpoint describes an address exposed by an agent. Input and Delivery are
// independent capabilities; an endpoint can support either or both.
type Endpoint struct {
	Channel string `json:"channel,omitempty"`
	// Ready reports current input readiness, not a guarantee of execution.
	Ready    bool   `json:"ready"`
	Address  string `json:"address"`
	Input    bool   `json:"input"`
	Delivery bool   `json:"delivery"`
}

// State is an application-defined agent state such as ready, draining, or
// stopped. Presence stores but does not interpret state values.
type State string

const (
	// Starting means startup is not complete.
	Starting State = "starting"
	// Ready means the agent completed startup.
	Ready State = "ready"
	// Draining means the agent is stopping new input.
	Draining State = "draining"
)

// Record is the current advertised presence of one agent instance.
type Record struct {
	Identity  Identity          `json:"identity"`
	Endpoints []Endpoint        `json:"endpoints,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	State     State             `json:"state,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// Validate checks that a record has a complete identity and endpoint addresses.
func (r Record) Validate() error {
	if r.Identity.Agent == "" || r.Identity.Instance == "" {
		return errors.New("presence: agent and instance identity are required")
	}
	for i, endpoint := range r.Endpoints {
		if endpoint.Address == "" {
			return fmt.Errorf("presence: endpoint %d has empty address", i)
		}
	}
	return nil
}

// Filter selects records by optional identity fields, state, and exact label
// matches. Empty fields and labels impose no restriction.
type Filter struct {
	Agent    string
	Instance string
	State    State
	Labels   map[string]string
}

// Registry supports registration, replacement, and removal of records.
type Registry interface {
	Register(context.Context, Record) error
	Unregister(context.Context, Identity) error
}

// Event describes a record addition, update, deletion, or terminal watcher error.
type Event struct {
	Record  Record
	Deleted bool
	Err     error
}

// Watcher returns an initial snapshot and a stream of changes.
type Watcher interface {
	Watch(context.Context, Filter) ([]Record, <-chan Event, error)
}

// Discoverer supports listing records that match a filter.
type Discoverer interface {
	Discover(context.Context, Filter) ([]Record, error)
}

// Store combines registration and discovery operations.
type Store interface {
	Registry
	Discoverer
}

// TTLStore reports the lifetime configured for records in a registry.
type TTLStore interface {
	TTL() time.Duration
}

// MemoryStore is a concurrency-safe in-memory presence store.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[Identity]Record
	now     func() time.Time
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[Identity]Record), now: time.Now}
}

// Register creates or replaces the record for its agent instance. It copies
// mutable fields so callers can safely reuse or modify their input afterward.
func (s *MemoryStore) Register(ctx context.Context, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := record.Validate(); err != nil {
		return err
	}
	record = cloneRecord(record)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.now().UTC()
	for id, current := range s.records {
		if !current.ExpiresAt.IsZero() && !now.Before(current.ExpiresAt) {
			delete(s.records, id)
		}
	}
	for id := range s.records {
		if id.Agent == record.Identity.Agent && id.Instance != record.Identity.Instance {
			return ErrConflict
		}
	}
	record.UpdatedAt = now
	s.records[record.Identity] = record
	return nil
}

// Unregister removes a record. Removing an unknown identity returns ErrNotFound.
func (s *MemoryStore) Unregister(ctx context.Context, id Identity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok || (!record.ExpiresAt.IsZero() && !s.now().Before(record.ExpiresAt)) {
		delete(s.records, id)
		return ErrNotFound
	}
	delete(s.records, id)
	return nil
}

// Discover returns matching records in agent, then instance, order.
func (s *MemoryStore) Discover(ctx context.Context, filter Filter) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Record, 0)
	now := s.now()
	for id, record := range s.records {
		if !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
			delete(s.records, id)
			continue
		}
		if !matches(record, filter) {
			continue
		}
		result = append(result, cloneRecord(record))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Identity.Agent != result[j].Identity.Agent {
			return result[i].Identity.Agent < result[j].Identity.Agent
		}
		return result[i].Identity.Instance < result[j].Identity.Instance
	})
	return result, nil
}

func matches(r Record, f Filter) bool {
	if f.Agent != "" && r.Identity.Agent != f.Agent {
		return false
	}
	if f.Instance != "" && r.Identity.Instance != f.Instance {
		return false
	}
	if f.State != "" && r.State != f.State {
		return false
	}
	for key, value := range f.Labels {
		if actual, ok := r.Labels[key]; !ok || actual != value {
			return false
		}
	}
	return true
}

func cloneRecord(r Record) Record {
	r.Endpoints = append([]Endpoint(nil), r.Endpoints...)
	if r.Labels != nil {
		labels := make(map[string]string, len(r.Labels))
		maps.Copy(labels, r.Labels)
		r.Labels = labels
	}
	return r
}
