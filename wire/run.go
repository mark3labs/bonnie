package wire

import kit "github.com/mark3labs/kit/pkg/kit"

// Run is a snapshot of a durable run after a turn boundary.
type Run struct {
	ID string
	// TurnID identifies the latest turn for scoped controls.
	TurnID   string
	State    RunState
	Response string
	// Suspend is non-nil when State is [RunWaiting].
	Suspend *SuspendRequest
	Usage   *kit.LLMUsage
	Err     error
}
