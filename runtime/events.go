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

// Event is one observable moment in a run's life. It is the unit of
// BONNIE's streaming surface: one JSON object, one line on the wire.
//
// Seq is the journal position the event is anchored to, per run. Durable
// events — state, suspend, resume, response — anchor to the record they
// came from, so reconnecting from a cursor, rewinding to 0, or replaying a
// finished run all produce the same event at the same Seq. Live-only events
// (agent deltas forwarded from Kit) anchor to the journal position at the
// moment they were published; they are never replayed, and a cursor that
// only counts durable events never misses one of those on reconnect.
//
// A client's cursor is the last Seq it saw. The stream serves every event
// past it — from the backlog when it still reaches, from the journal when it
// does not.
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

// EventBus fans run events out to subscribers and keeps a bounded backlog
// per run so a client that drops its connection can catch up.
//
// The backlog is in memory only, and it is not the record of truth — the
// journal is. Every event carries the journal position it is anchored to, so
// a reconnect whose cursor has fallen off the backlog edge is served from
// the journal: [Runner.StreamEvents] replays the records and joins the live
// stream without a gap. The anchor is wired by the [Runner]; a standalone
// bus leaves events unstamped, and they then behave as live-only.
type EventBus struct {
	capacity int

	// anchor reports the current journal position for a run. Nil means
	// events publish unstamped.
	anchor func(runID string) int

	mu      sync.Mutex
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
		backlog:  make(map[string][]Event),
		subs:     make(map[string]map[int]*subscriber),
	}
}

// Anchor wires the journal-position function that stamps events which do not
// carry a Seq of their own. The [Runner] sets it from the journal it holds.
func (b *EventBus) Anchor(fn func(runID string) int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.anchor = fn
}

// Publish stamps an event and delivers it to every subscriber. An event that
// carries no Seq of its own is anchored to the current journal position.
// Callers that know the record an event belongs to set Seq themselves and
// win.
func (b *EventBus) Publish(ev Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ev.Seq == 0 && b.anchor != nil {
		ev.Seq = b.anchor(ev.RunID)
	}
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

// Oldest returns the Seq of the oldest event still buffered for a run, or 0
// when nothing is buffered. A cursor below Oldest-1 has fallen off the edge
// of the backlog and must be served from the journal.
func (b *EventBus) Oldest(runID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf := b.backlog[runID]
	if len(buf) == 0 {
		return 0
	}
	return buf[0].Seq
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

// subscriber owns an unbounded queue in front of a bounded channel. A slow
// reader therefore costs memory, never lost events — dropping would break the
// cursor contract that reconnecting clients rely on.
//
// done is closed by [subscriber.close] and releases a send that no one is
// reading. A client that goes away mid-event leaves the pump blocked on
// out <- ev, and the closed flag alone cannot reach it there: the flag is
// read before the send, never during it.
type subscriber struct {
	out  chan Event
	wake chan struct{}
	done chan struct{}

	mu     sync.Mutex
	queue  []Event
	closed bool
}

func newSubscriber() *subscriber {
	s := &subscriber{
		out:  make(chan Event),
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
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
	close(s.done)
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
			select {
			case s.out <- ev:
			case <-s.done:
				return
			}
		}
		if closed {
			return
		}
		select {
		case <-s.wake:
		case <-s.done:
			return
		}
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
