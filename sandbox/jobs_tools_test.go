package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

type jobsTestSandbox struct {
	status    *JobStatus
	files     map[string][]byte
	cancelled int
	cancelErr error
	statusErr error
}

func (s *jobsTestSandbox) ID() string                                     { return "test" }
func (s *jobsTestSandbox) Exec(context.Context, Command) (*Result, error) { return &Result{}, nil }
func (s *jobsTestSandbox) ReadFile(_ context.Context, name string) ([]byte, error) {
	return s.files[name], nil
}
func (s *jobsTestSandbox) WriteFile(context.Context, string, []byte) error { return nil }
func (s *jobsTestSandbox) Stop(context.Context) error                      { return nil }
func (s *jobsTestSandbox) Close() error                                    { return nil }
func (s *jobsTestSandbox) StartJob(context.Context, Command) (string, error) {
	return "0123456789abcdef0123456789abcdef", nil
}
func (s *jobsTestSandbox) JobStatus(ctx context.Context, _ string) (*JobStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.statusErr != nil {
		return nil, s.statusErr
	}
	return s.status, nil
}
func (s *jobsTestSandbox) CancelJob(_ context.Context, _ string) error {
	s.cancelled++
	return s.cancelErr
}

func jobTestOutput(t *testing.T, output kit.ToolOutput) jobToolResult {
	t.Helper()
	var result jobToolResult
	if err := json.Unmarshal([]byte(output.Content), &result); err != nil {
		t.Fatalf("invalid job output %q: %v", output.Content, err)
	}
	return result
}

func TestJobOutputCompletedFailureAndTruncation(t *testing.T) {
	t.Parallel()
	code := 17
	sb := &jobsTestSandbox{status: &JobStatus{ID: "job", ExitCode: &code, StdoutPath: "out", StderrPath: "err"}, files: map[string][]byte{"out": []byte(strings.Repeat("x", maxToolOutput+1)), "err": []byte("failure")}}
	out, err := jobOutput(context.Background(), sb, sb.status)
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsError {
		t.Fatal("expected failed job to set IsError")
	}
	got := jobTestOutput(t, out)
	if !got.Completed || got.Running || got.Status != "completed" || got.ExitCode == nil || *got.ExitCode != code {
		t.Fatalf("unexpected status: %+v", got)
	}
	if !got.Truncated || len(got.Stdout) != maxToolOutput || got.Stderr != "failure" {
		t.Fatalf("unexpected output clipping: %+v", got)
	}
	if !strings.Contains(got.Message, "17") {
		t.Fatalf("missing failure message: %q", got.Message)
	}
}

func TestWaitJobReturnsRunningAtDeadline(t *testing.T) {
	t.Parallel()
	sb := &jobsTestSandbox{status: &JobStatus{ID: "job", Running: true}}
	out, err := waitJob(context.Background(), sb, sb, "job", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := jobTestOutput(t, out)
	if !got.Running || got.Completed || got.Status != "running" || sb.cancelled != 0 {
		t.Fatalf("unexpected wait result: %+v, cancels=%d", got, sb.cancelled)
	}
}

func TestWaitJobCancellationCancelsJobWithoutCallerContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	sb := &jobsTestSandbox{status: &JobStatus{ID: "job", Running: true}}
	cancel()
	out, err := waitJob(ctx, sb, sb, "job", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsError || sb.cancelled != 1 {
		t.Fatalf("result=%+v cancels=%d", out, sb.cancelled)
	}
}

func TestWaitJobCancellationJoinsCancelError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelErr := errors.New("cancel failed")
	sb := &jobsTestSandbox{status: &JobStatus{ID: "job", Running: true}, cancelErr: cancelErr}
	out, err := waitJob(ctx, sb, sb, "job", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsError || !strings.Contains(out.Content, cancelErr.Error()) {
		t.Fatalf("cancel error missing: %+v", out)
	}
}

func TestJobToolValidationDoesNotOpenSandbox(t *testing.T) {
	t.Parallel()
	opened := false
	tool := shellStatusTool(func(context.Context) (Sandbox, error) { opened = true; return nil, nil })
	response, err := tool.Run(context.Background(), kit.LLMToolCall{Input: `{"job_id":"invalid"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !response.IsError || opened {
		t.Fatalf("result=%+v opened=%v", response, opened)
	}
}
