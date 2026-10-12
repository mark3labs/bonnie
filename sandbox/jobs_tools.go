package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

const jobPollInterval = 100 * time.Millisecond

type jobToolResult struct {
	Completed  bool   `json:"completed"`
	Running    bool   `json:"running"`
	JobID      string `json:"job_id"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   *int   `json:"exit_code"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	Truncated  bool   `json:"truncated"`
	StdoutPath string `json:"stdout_path"`
	StderrPath string `json:"stderr_path"`
}

func waitJob(ctx context.Context, sb Sandbox, jobs JobSandbox, id string, wait time.Duration) (kit.ToolOutput, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		st, err := jobs.JobStatus(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if cancelErr := jobs.CancelJob(cancelCtx, id); cancelErr != nil {
					err = errors.Join(ctx.Err(), fmt.Errorf("cancel job: %w", cancelErr))
				} else {
					err = ctx.Err()
				}
			}
			return kit.ErrorResult(fmt.Sprintf("sandbox error: job status: %v", err)), nil
		}
		if !st.Running {
			return jobOutput(ctx, sb, st)
		}
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cancelErr := jobs.CancelJob(cancelCtx, id)
			if cancelErr != nil {
				return kit.ErrorResult(errors.Join(ctx.Err(), fmt.Errorf("cancel job: %w", cancelErr)).Error()), nil
			}
			return kit.ErrorResult(ctx.Err().Error()), nil
		case <-deadline.C:
			return jobOutput(ctx, sb, st)
		case <-time.After(jobPollInterval):
		}
	}
}

func capJobOutput(s string) string {
	if len(s) > maxToolOutput {
		return s[:maxToolOutput]
	}
	return s
}

func jobOutput(ctx context.Context, sb Sandbox, st *JobStatus) (kit.ToolOutput, error) {
	status := "running"
	if !st.Running {
		status = "completed"
	}
	out := jobToolResult{Completed: !st.Running, Running: st.Running, JobID: st.ID, ExitCode: st.ExitCode, Status: status, StdoutPath: st.StdoutPath, StderrPath: st.StderrPath}
	if reader, ok := sb.(JobOutputReader); ok {
		stdout, stderr, truncated, err := reader.ReadJobOutput(ctx, st.ID, maxToolOutput)
		if err != nil {
			return kit.ErrorResult(err.Error()), nil
		}
		out.Stdout, out.Stderr, out.Truncated = stdout, stderr, truncated
	} else {
		if st.StdoutPath != "" {
			b, err := sb.ReadFile(ctx, st.StdoutPath)
			if err != nil {
				return kit.ErrorResult(err.Error()), nil
			}
			out.Stdout = capJobOutput(string(b))
			out.Truncated = out.Truncated || len(b) > maxToolOutput
		}
		if st.StderrPath != "" {
			b, err := sb.ReadFile(ctx, st.StderrPath)
			if err != nil {
				return kit.ErrorResult(err.Error()), nil
			}
			out.Stderr = capJobOutput(string(b))
			out.Truncated = out.Truncated || len(b) > maxToolOutput
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return kit.ErrorResult(err.Error()), nil
	}
	isError := st.ExitCode != nil && *st.ExitCode != 0
	if isError {
		out.Message = fmt.Sprintf("job completed with exit code %d", *st.ExitCode)
		b, err = json.Marshal(out)
		if err != nil {
			return kit.ErrorResult(err.Error()), nil
		}
	}
	return kit.ToolOutput{Content: string(b), IsError: isError}, nil
}

func jobTool(open Opener, name, desc string, fn func(context.Context, Sandbox, JobSandbox, string, int) (kit.ToolOutput, error)) kit.Tool {
	type input struct {
		JobID       string `json:"job_id" description:"Job ID returned by shell."`
		WaitSeconds int    `json:"wait_seconds,omitempty" description:"Seconds to wait (maximum 30)."`
	}
	return kit.NewTool(name, desc, func(ctx context.Context, in input) (kit.ToolOutput, error) {
		if in.WaitSeconds < 0 || in.WaitSeconds > 30 {
			return kit.ErrorResult("wait_seconds must be between 0 and 30"), nil
		}
		if err := validateJobID(in.JobID); err != nil {
			return kit.ErrorResult(err.Error()), nil
		}
		sb, err := open(ctx)
		if err != nil {
			return unavailable(err), nil
		}
		jobs, ok := sb.(JobSandbox)
		if !ok {
			return kit.ErrorResult("sandbox does not support jobs"), nil
		}
		return fn(ctx, sb, jobs, in.JobID, in.WaitSeconds)
	})
}

func shellStatusTool(open Opener) kit.Tool {
	return jobTool(open, "shell_status", "Check the current state and output of a shell job.", func(ctx context.Context, sb Sandbox, jobs JobSandbox, id string, _ int) (kit.ToolOutput, error) {
		st, err := jobs.JobStatus(ctx, id)
		if err != nil {
			return kit.ErrorResult(fmt.Sprintf("sandbox error: job status: %v", err)), nil
		}
		return jobOutput(ctx, sb, st)
	})
}

func shellWaitTool(open Opener) kit.Tool {
	return jobTool(open, "shell_wait", "Wait for a shell job to finish, up to wait_seconds (0 means 30).", func(ctx context.Context, sb Sandbox, jobs JobSandbox, id string, seconds int) (kit.ToolOutput, error) {
		if seconds == 0 {
			seconds = 30
		}
		return waitJob(ctx, sb, jobs, id, time.Duration(seconds)*time.Second)
	})
}

func shellCancelTool(open Opener) kit.Tool {
	return jobTool(open, "shell_cancel", "Cancel a running shell job.", func(ctx context.Context, sb Sandbox, jobs JobSandbox, id string, _ int) (kit.ToolOutput, error) {
		if err := jobs.CancelJob(ctx, id); err != nil {
			return kit.ErrorResult(fmt.Sprintf("sandbox error: cancel job: %v", err)), nil
		}
		st, err := jobs.JobStatus(ctx, id)
		if err != nil {
			return kit.ErrorResult(fmt.Sprintf("sandbox error: confirm cancel: %v", err)), nil
		}
		if st.Running {
			return kit.ErrorResult("job cancellation was not confirmed"), nil
		}
		return jobOutput(ctx, sb, st)
	})
}
