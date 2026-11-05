package workflow

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/mark3labs/bonnie/runtime"
)

const runPrefix = runtime.ReservedRunPrefix + "workflow."
const snapshotKind runtime.RecordKind = "workflow_snapshot"

// State describes a workflow run, not an individual callback attempt.
type State string

const (
	// Pending means the run is admitted and ready to execute.
	Pending State = "pending"
	// Running means execution started without a saved stop. After a crash it
	// can remain Running until recovery.
	Running State = "running"
	// Waiting means the run is parked on a timer, retry, or signal.
	Waiting State = "waiting"
	// Blocked means an unsafe attempt has an unknown outcome.
	Blocked State = "blocked"
	// Completed means the result is durable.
	Completed State = "completed"
	// Failed means the workflow returned an error or violated replay rules.
	Failed State = "failed"
	// Cancelled means execution cancellation is durable.
	Cancelled State = "cancelled"
)

var (
	// ErrNotFound means the workflow run is unknown.
	ErrNotFound = errors.New("bonnie: workflow not found")
	// ErrConflict means an admission or signal ID has different content.
	ErrConflict = errors.New("bonnie: workflow idempotency conflict")
	// ErrReplay means operation identity, order, options, or type changed.
	ErrReplay = errors.New("bonnie: workflow replay mismatch")
	// ErrCancelled means the workflow was explicitly cancelled.
	ErrCancelled = errors.New("bonnie: workflow cancelled")
	// ErrTerminal means the workflow has already ended.
	ErrTerminal = errors.New("bonnie: workflow is terminal")
	// ErrNotWorkflow means an operation was called outside a workflow.
	ErrNotWorkflow = errors.New("bonnie: no workflow execution in context")
)

type operation struct {
	Key      string          `json:"key"`
	Kind     string          `json:"kind"`
	Type     string          `json:"type"`
	Config   stepConfig      `json:"config"`
	Duration time.Duration   `json:"duration,omitempty"`
	State    string          `json:"state"`
	Attempts int             `json:"attempts,omitempty"`
	Due      time.Time       `json:"due,omitzero"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type message struct {
	Key      string          `json:"key"`
	ID       string          `json:"id"`
	Value    json.RawMessage `json:"value"`
	Consumed bool            `json:"consumed,omitempty"`
}

type snapshot struct {
	Format     int             `json:"format"`
	ID         string          `json:"id"`
	RequestID  string          `json:"request_id"`
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	InputType  string          `json:"input_type"`
	ResultType string          `json:"result_type"`
	Input      json.RawMessage `json:"input"`
	State      State           `json:"state"`
	Error      string          `json:"error,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Operations []operation     `json:"operations,omitempty"`
	Messages   []message       `json:"messages,omitempty"`
}

func terminal(s State) bool { return s == Completed || s == Failed || s == Cancelled }
func clone(s snapshot) snapshot {
	s.Operations = append([]operation(nil), s.Operations...)
	s.Messages = append([]message(nil), s.Messages...)
	return s
}

// Status is a point-in-time view of a run. BlockedKey identifies the step
// whose external outcome must be reconciled with Resolve or cancellation.
type Status struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	State      State  `json:"state"`
	Error      string `json:"error,omitempty"`
	BlockedKey string `json:"blocked_key,omitempty"`
}
