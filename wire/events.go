package wire

import (
	"encoding/json"
	"time"
)

// Event types that the runtime itself publishes. Every other type on an
// [Event] comes verbatim from a Kit lifecycle event, so a consumer sees the
// same vocabulary Kit uses.
const (
	// EventTurn reports the durable identity of a new turn.
	EventTurn = "run_turn"
	// EventCancelRequested reports a durable cancellation command. A cancelled
	// state confirms completion and withdraws all pending input for that turn.
	EventCancelRequested = "run_cancel_requested"
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
	RunID  string          `json:"run_id"`
	TurnID string          `json:"turn_id,omitempty"`
	Seq    int             `json:"seq"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Text   string          `json:"text,omitempty"`
	State  RunState        `json:"state,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}
