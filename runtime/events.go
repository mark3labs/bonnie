package runtime

import (
	"encoding/json"
	"sync"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Event types that the runtime itself publishes. Every other type on an
// [Event] comes verbatim from a Kit lifecycle event, so a consumer sees the
// same vocabulary Kit uses.
const (
	// EventState reports a run-state transition.
	EventState = "run_state"
	// EventSuspend reports that the run parked for input.
	EventSuspend = "run_suspend"
	// EventResume reports the input that released a suspension.
	EventResume = "run_resume"
	// EventResponse reports the assistant's final text for a turn.
	EventResponse = "run_response"
)

// Event is one observable moment in a run's life. It is the unit of BONNIE's
// streaming surface: one JSON object, one line on the wire.
//
// Seq is monotonic per run and starts at 1. A client that reconnects passes
// the last Seq it saw as a cursor, and the bus replays everything after it
// that is still buffered.
type Event struct {
	RunID string          `json:"run_id"`
	Seq   int             `json:"seq"`
	Type  string          `json:"type"`
	Time  time.Time       `json:"time"`
	Text  string          `json:"text,omitempty"`
	State RunState        `json:"state,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// DefaultEventBuffer is how many recent events a run keeps for reconnecting
// clients.
const DefaultEventBuffer = 1024

// EventBus fans run events out to subscribers and keeps a bounded backlog per
// run so a client that drops its connection can catch up.
//
// The backlog is in memory only. Events are a live view, not a durable record:
// the journal is the durable record. A client that reconnects after more than
// [DefaultEventBuffer] events have passed sees a gap, and should replay the
// journal instead.
type EventBus struct {
	capacity int

	mu      sync.Mutex
	seq     map[string]int
	backlog map[string][]Event
	subs    map[string]map[int]*subscriber
	nextSub int
}

// NewEventBus returns a bus that keeps capacity events per run. A capacity of
// zero or less uses [DefaultEventBuffer].
func NewEventBus(capacity int) *EventBus {
	if capacity <= 0 {
		capacity = DefaultEventBuffer
	}
	return &EventBus{
		capacity: capacity,
		seq:      make(map[string]int),
		backlog:  make(map[string][]Event),
		subs:     make(map[string]map[int]*subscriber),
	}
}

// Publish stamps an event with the next sequence number for its run and
// delivers it to every subscriber. It returns the stamped event.
func (b *EventBus) Publish(ev Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq[ev.RunID]++
	ev.Seq = b.seq[ev.RunID]
	if ev.Time.IsZero() {
		ev.Time = now()
	}

	buf := append(b.backlog[ev.RunID], ev)
	if len(buf) > b.capacity {
		buf = buf[len(buf)-b.capacity:]
	}
	b.backlog[ev.RunID] = buf

	for _, s := range b.subs[ev.RunID] {
		s.push(ev)
	}
	return ev
}

// PublishData is a convenience for publishing a typed payload. A payload that
// cannot be marshalled is dropped rather than failing the run: events are
// observability, never the source of truth.
func (b *EventBus) PublishData(runID, typ string, text string, payload any) Event {
	ev := Event{RunID: runID, Type: typ, Text: text}
	if payload != nil {
		if raw, err := json.Marshal(payload); err == nil {
			ev.Data = raw
		}
	}
	return b.Publish(ev)
}

// Subscribe returns a channel of events for a run, starting after the given
// cursor. Pass 0 to get the whole buffered backlog.
//
// The returned function unsubscribes and closes the channel. It is safe to
// call more than once.
func (b *EventBus) Subscribe(runID string, after int) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := newSubscriber()
	for _, ev := range b.backlog[runID] {
		if ev.Seq > after {
			s.push(ev)
		}
	}

	b.nextSub++
	id := b.nextSub
	if b.subs[runID] == nil {
		b.subs[runID] = make(map[int]*subscriber)
	}
	b.subs[runID][id] = s

	return s.out, func() {
		b.mu.Lock()
		if m, ok := b.subs[runID]; ok {
			delete(m, id)
			if len(m) == 0 {
				delete(b.subs, runID)
			}
		}
		b.mu.Unlock()
		s.close()
	}
}

// Backlog returns the buffered events for a run after the given cursor. It is
// the polling counterpart of [EventBus.Subscribe].
func (b *EventBus) Backlog(runID string, after int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []Event
	for _, ev := range b.backlog[runID] {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out
}

// subscriber owns an unbounded queue in front of a bounded channel. A slow
// reader therefore costs memory, never lost events — dropping would break the
// cursor contract that reconnecting clients rely on.
type subscriber struct {
	out  chan Event
	wake chan struct{}

	mu     sync.Mutex
	queue  []Event
	closed bool
}

func newSubscriber() *subscriber {
	s := &subscriber{
		out:  make(chan Event),
		wake: make(chan struct{}, 1),
	}
	go s.pump()
	return s
}

func (s *subscriber) push(ev Event) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, ev)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscriber) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscriber) pump() {
	defer close(s.out)
	for {
		s.mu.Lock()
		queue := s.queue
		s.queue = nil
		closed := s.closed
		s.mu.Unlock()

		for _, ev := range queue {
			if closed {
				return
			}
			s.out <- ev
		}
		if closed {
			return
		}
		<-s.wake
	}
}

// eventSource is the optional part of an [Agent] that emits Kit lifecycle
// events. *kit.Kit satisfies it; a test double need not. The runner
// type-asserts for it, so streaming is a bonus rather than a requirement, and
// [Agent] stays the three small methods that make L1 testable.
type eventSource interface {
	Subscribe(listener kit.EventListener) func()
}

// forwardAgentEvents mirrors an agent's Kit lifecycle events onto the bus,
// under the same run ID. It returns a no-op stop function for an agent that
// emits nothing.
func forwardAgentEvents(agent Agent, bus *EventBus, runID string) func() {
	src, ok := agent.(eventSource)
	if !ok {
		return func() {}
	}
	return src.Subscribe(func(ev kit.Event) {
		bus.PublishData(runID, string(ev.EventType()), "", ev)
	})
}
