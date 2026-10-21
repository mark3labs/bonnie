package wire

// CancelResult reports a cancellation command, not execution completion.
// Requested means the command is durable. RunCancelled on the stream confirms
// completion. NotActive and Stale are harmless no-ops.
type CancelResult struct {
	RunID  string `json:"run_id,omitempty"`
	TurnID string `json:"turn_id,omitempty"`
	Status string `json:"status"`
}

// Cancellation outcomes shared by all transports.
const (
	CancelRequested = "requested"
	CancelNotActive = "not_active"
	CancelStale     = "stale"
)
