package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"time"
)

// Observer watches each turn a [Runner] executes. It is the seam for
// tracing: package observe implements it with OpenTelemetry, and the runtime
// itself imports no tracing library.
//
// An observer sees live turns only. A replay, a snapshot, or an event stream
// rebuilt from the journal never reaches it, so a tracer that runs again over
// a finished run does not emit the run again.
//
// The runtime calls an observer synchronously on the turn's goroutine. Each
// method must be safe for concurrent use across runs and must return
// promptly: a slow observer delays the run.
type Observer interface {
	// StartTurn is called once per executed turn, before the turn record
	// is written. It returns the context the turn runs in, which reaches
	// the agent factory and the agent, and the observation that follows
	// the rest of the turn.
	StartTurn(ctx context.Context, t TurnStart) (context.Context, TurnObservation)
}

// TurnObservation follows one turn after [Observer.StartTurn].
type TurnObservation interface {
	// Carrier returns the trace context to journal with the turn record:
	// W3C trace context keys such as "traceparent". The next turn of the
	// run receives it as [TurnStart.Previous], also in another process.
	// Nil journals nothing.
	Carrier() map[string]string
	// Attach is called when the turn's agent exists, before the agent
	// runs. The returned function is called when the agent closes.
	Attach(agent Agent) (detach func())
	// End is called once when the turn reaches a boundary or fails.
	End(TurnEnd)
}

// DecisionObservation is an optional interface of a [TurnObservation]. The
// runtime calls Decision once for each decision call of the turn that
// reaches the [Decider], after its record is written or has failed to be. ctx is the caller's context, so a span made from it has the
// caller's span as its parent.
type DecisionObservation interface {
	Decision(ctx context.Context, d DecisionRecord, start, end time.Time)
}

// TurnKind says how a turn relates to the turns before it.
type TurnKind string

const (
	// TurnNew starts on new input.
	TurnNew TurnKind = "new"
	// TurnRecovery continues work that an interrupted or crashed turn left
	// unfinished. [TurnStart.Previous] is that turn's carrier.
	TurnRecovery TurnKind = "recovery"
	// TurnResume continues a run that waited for human input.
	// [TurnStart.Previous] is the carrier of the turn that waited.
	TurnResume TurnKind = "resume"
)

// TurnStart describes a turn to [Observer.StartTurn].
type TurnStart struct {
	RunID  string
	TurnID string
	Kind   TurnKind
	// Title and Origin are what the run recorded on its first turn.
	Title  string
	Origin Origin
	// Prompt is the text the turn sends to the agent. It is empty for a
	// recovery that continues durable input.
	Prompt string
	// Parent is the trace context the input carried ([Input.Trace]).
	Parent map[string]string
	// Previous is the carrier journalled with the run's last turn record,
	// or nil when there is none.
	Previous map[string]string
	// Responses are the answers that resume a waiting run. Only
	// [TurnResume] carries them.
	Responses []InputResponse
}

// TurnEnd describes how a turn ended to [TurnObservation.End].
type TurnEnd struct {
	// Run is the turn's outcome. It is nil when the turn failed before it
	// had one, and then Err is not nil.
	Run *Run
	Err error
}

// WithObserver makes the Runner report each turn to o. Nil disables it.
func WithObserver(o Observer) RunnerOption {
	return func(r *Runner) { r.observer = o }
}

// turnPayload is the durable form of a [RecordTurn] payload. A turn record
// without a payload is valid: it is what every journal held before tracing.
type turnPayload struct {
	Trace map[string]string `json:"trace,omitempty"`
}

func encodeTurnPayload(trace map[string]string) []byte {
	if len(trace) == 0 {
		return nil
	}
	payload, err := json.Marshal(turnPayload{Trace: trace})
	if err != nil {
		return nil
	}
	return payload
}

// lastTurnCarrier returns the trace carrier of the run's last turn record.
// A record from before tracing, or one that does not decode, has none.
func lastTurnCarrier(recs []Record) map[string]string {
	for _, rec := range slices.Backward(recs) {
		if rec.Kind != RecordTurn {
			continue
		}
		var p turnPayload
		if len(rec.Payload) == 0 || json.Unmarshal(rec.Payload, &p) != nil {
			return nil
		}
		return p.Trace
	}
	return nil
}

// nopObservation stands in when the Runner has no observer, so the turn
// code has one path.
type nopObservation struct{}

func (nopObservation) Carrier() map[string]string { return nil }
func (nopObservation) Attach(Agent) func()        { return func() {} }
func (nopObservation) End(TurnEnd)                {}
