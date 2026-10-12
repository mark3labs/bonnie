//go:build integration && linux

package sandbox

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestNativeCLIJobs uses an explicitly supplied disposable guest, not a model.
// Set BONNIE_JOB_TEST_BACKEND=docker|microsandbox, BONNIE_JOB_TEST_BIN, and
// BONNIE_JOB_TEST_GUEST. The caller owns guest creation and deletion.
func TestNativeCLIJobs(t *testing.T) {
	backend, bin, guest := os.Getenv("BONNIE_JOB_TEST_BACKEND"), os.Getenv("BONNIE_JOB_TEST_BIN"), os.Getenv("BONNIE_JOB_TEST_GUEST")
	if backend == "" || bin == "" || guest == "" {
		t.Skip("no disposable native guest configured")
	}
	nonce, err := randomNonce()
	if err != nil {
		t.Fatal(err)
	}
	c := &cliSandbox{bin: bin, name: guest, backend: backend, jobsDir: "/tmp/bonnie-job-test-" + nonce}
	c.execArgs = func(_ *cliSandbox, argv []string, _ Command) []string {
		args := []string{"exec", guest, "--no-tty", "--"}
		if backend == "docker" {
			args = []string{"exec", guest}
		}
		return append(args, argv...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := c.StartJob(ctx, Command{Args: []string{"sh", "-c", "printf hello; sleep 60"}, Dir: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CancelJob(context.Background(), id) }()
	st, err := c.JobStatus(ctx, id)
	if err != nil || !st.Running {
		t.Fatalf("status %+v, %v", st, err)
	}
	out, _, _, err := c.ReadJobOutput(ctx, id, 32)
	if err != nil || out != "hello" {
		t.Fatalf("output %q, %v", out, err)
	}
	if err := c.CancelJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	st, err = c.JobStatus(ctx, id)
	if err != nil || st.Running {
		t.Fatalf("cancel status %+v, %v", st, err)
	}
}
