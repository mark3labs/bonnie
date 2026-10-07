// Package schedule starts durable agent work from code-defined cron jobs.
package schedule

import (
	"context"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// Definition declares a job. Exactly one of Prompt and Prepare is required.
// Prepare runs on the host, not in the agent sandbox. It must be safe to
// repeat if the process stops before its returned dispatches are saved.
// CatchUp accepts "skip" (default) or "latest". Overlap accepts "skip"
// (default) or "allow". Waiting runs count as unfinished work.
type Definition struct {
	Name        string                                          `json:"name"`
	Cron        string                                          `json:"cron"`
	TimeZone    string                                          `json:"time_zone,omitempty"`
	Revision    string                                          `json:"revision,omitempty"`
	Prompt      string                                          `json:"-"`
	Destination Destination                                     `json:"destination"`
	Prepare     func(context.Context, Fire) ([]Dispatch, error) `json:"-"`
	CatchUp     string                                          `json:"catch_up,omitempty"`
	Overlap     string                                          `json:"overlap,omitempty"`
}

// Destination selects a mounted tracked channel. An empty Channel starts
// a background run. Target must be JSON-serializable. On replay, targets
// are decoded into generic JSON values; adapters must accept this shape.
type Destination struct {
	Channel string `json:"channel,omitempty"`
	Target  any    `json:"target,omitempty"`
}

// Dispatch is a prepared turn. Key must be unique within its occurrence.
// Files are not supported for scheduled dispatches in this release.
type Dispatch struct {
	Key         string             `json:"key"`
	Input       runtime.Input      `json:"input"`
	Destination Destination        `json:"destination"`
	Auth        *channel.Principal `json:"auth,omitempty"`
}

// Fire identifies an occurrence. ID is stable across retries.
type Fire struct {
	Name        string    `json:"name"`
	ID          string    `json:"id"`
	ScheduledAt time.Time `json:"scheduled_at"`
	Kind        string    `json:"kind"`
}

// State describes an occurrence, not a conversation's lifetime.
type State string

const (
	// Pending means preparation has not been saved.
	Pending State = "pending"
	// Running means work or delivery is unfinished.
	Running State = "running"
	// Waiting means at least one run needs external input.
	Waiting State = "waiting"
	// Completed means all dispatched turns and initial deliveries finished.
	Completed State = "completed"
	// Failed means preparation or execution failed; no fresh agent retry is made.
	Failed State = "failed"
	// Skipped means overlap policy or preparation produced no work.
	Skipped State = "skipped"
)

// Work is the saved execution and delivery state of one dispatch.
// Delivery is at least once: a crash after a successful post can repeat it.
type Work struct {
	Dispatch  Dispatch                `json:"dispatch"`
	Receipt   channel.DispatchReceipt `json:"receipt"`
	Reserved  bool                    `json:"reserved"`
	Result    *runtime.Run            `json:"result,omitempty"`
	Delivered bool                    `json:"delivered"`
	Error     string                  `json:"error,omitempty"`
	RetryAt   time.Time               `json:"retry_at,omitzero"`
	Attempts  int                     `json:"attempts,omitempty"`
}

// Occurrence is an append-only snapshot of a schedule firing.
type Occurrence struct {
	Fire     Fire   `json:"fire"`
	Revision string `json:"revision,omitempty"`
	State    State  `json:"state"`
	Work     []Work `json:"work,omitempty"`
	Prepared bool   `json:"prepared"`
	Error    string `json:"error,omitempty"`
}
