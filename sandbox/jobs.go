package sandbox

import (
	"context"
	"errors"
	"fmt"
)

// ErrJobNotFound means the sandbox has no record of the job.
var ErrJobNotFound = errors.New("bonnie: sandbox: job not found")

// ErrJobUnknown means a job exists but its execution outcome cannot be established.
var ErrJobUnknown = errors.New("bonnie: sandbox: job unknown")

// JobSandbox is an optional interface for asynchronous commands. Job output
// is stored in sandbox files. Closing a handle does not cancel its jobs.
// A StartJob request context controls launch, not the lifetime of a started job.
// Local and Landlock accept Command.Timeout as an execution deadline; CLI
// backends reject it. Process-group cancellation cannot contain children that
// deliberately detach. A sandbox stop need not preserve running processes.
type JobSandbox interface {
	// StartJob launches a command and returns its sandbox-scoped ID.
	StartJob(ctx context.Context, cmd Command) (string, error)
	// JobStatus reads the execution state without repeating the command.
	JobStatus(ctx context.Context, id string) (*JobStatus, error)
	// CancelJob requests targeted termination, not a whole-sandbox stop.
	CancelJob(ctx context.Context, id string) error
}

// JobStatus describes a command and its captured output. A running job has no
// exit code. Output paths can be read with Sandbox.ReadFile or JobOutputReader.
type JobStatus struct {
	ID         string `json:"id"`
	Running    bool   `json:"running"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	StdoutPath string `json:"stdout_path"`
	StderrPath string `json:"stderr_path"`
}

func validateJobID(id string) error {
	if id == "" || len(id) != 32 {
		return fmt.Errorf("bonnie: sandbox: invalid job ID")
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("bonnie: sandbox: invalid job ID")
		}
	}
	return nil
}
