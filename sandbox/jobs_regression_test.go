//go:build unix

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func waitJobDone(t *testing.T, jobs JobSandbox, id string) *JobStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := jobs.JobStatus(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Running {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return nil
}

func TestLocalHostJobsConcurrentStart(t *testing.T) {
	t.Parallel()
	box, err := Local(WithLocalRoot(t.TempDir())).Open(context.Background(), "parallel-start")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = box.Close() }()
	jobs := box.(JobSandbox)
	ids := make([]string, 20)
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = jobs.StartJob(context.Background(), Command{Args: []string{"sh", "-c", "printf ok"}})
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("StartJob %d: %v", i, errs[i])
		}
		if seen[id] {
			t.Fatalf("duplicate job ID %q", id)
		}
		seen[id] = true
	}
	for _, id := range ids {
		waitJobDone(t, jobs, id)
	}
}

func TestLocalHostJobBackgroundChildDoesNotHoldOutputOpen(t *testing.T) {
	t.Parallel()
	box, err := Local(WithLocalRoot(t.TempDir())).Open(context.Background(), "background-child")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = box.Close() }()
	jobs := box.(JobSandbox)
	started := time.Now()
	id, err := jobs.StartJob(context.Background(), Command{Args: []string{"sh", "-c", "sleep 10 & printf done"}})
	if err != nil {
		t.Fatal(err)
	}
	status := waitJobDone(t, jobs, id)
	if time.Since(started) > 2*time.Second {
		t.Fatalf("job wait took %s; child held output open", time.Since(started))
	}
	reader := box.(JobOutputReader)
	stdout, _, _, err := reader.ReadJobOutput(context.Background(), id, 20)
	if err != nil || stdout != "done" {
		t.Fatalf("output %q, err %v (status %+v)", stdout, err, status)
	}
}

func TestLocalHostJobCancelLeavesOtherJobRunning(t *testing.T) {
	t.Parallel()
	box, err := Local(WithLocalRoot(t.TempDir())).Open(context.Background(), "cancel-one")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = box.Close() }()
	jobs := box.(JobSandbox)
	first, err := jobs.StartJob(context.Background(), Command{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := jobs.StartJob(context.Background(), Command{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.CancelJob(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	waitJobDone(t, jobs, first)
	status, err := jobs.JobStatus(context.Background(), second)
	if err != nil || !status.Running {
		t.Fatalf("other job status %+v, err %v", status, err)
	}
	if err := jobs.CancelJob(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	waitJobDone(t, jobs, second)
}

func TestLocalHostJobDeadline(t *testing.T) {
	t.Parallel()
	box, err := Local(WithLocalRoot(t.TempDir())).Open(context.Background(), "job-deadline")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = box.Close() }()
	id, err := box.(JobSandbox).StartJob(context.Background(), Command{Args: []string{"sleep", "60"}, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	status := waitJobDone(t, box.(JobSandbox), id)
	if status.ExitCode == nil || *status.ExitCode == 0 {
		t.Fatalf("deadline status: %+v", status)
	}
}

func TestLocalHostJobOutputReadLimit(t *testing.T) {
	t.Parallel()
	box, err := Local(WithLocalRoot(t.TempDir())).Open(context.Background(), "output-limit")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = box.Close() }()
	jobs := box.(JobSandbox)
	id, err := jobs.StartJob(context.Background(), Command{Args: []string{"sh", "-c", "head -c 131072 /dev/zero"}})
	if err != nil {
		t.Fatal(err)
	}
	waitJobDone(t, jobs, id)
	stdout, stderr, truncated, err := box.(JobOutputReader).ReadJobOutput(context.Background(), id, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) != 1024 || len(stderr) != 0 || !truncated {
		t.Fatalf("stdout=%d stderr=%d truncated=%v", len(stdout), len(stderr), truncated)
	}
}

func TestLandlockJobUsesPinnedRootAfterWorkspaceRename(t *testing.T) {
	t.Parallel()
	provider := landlockProvider(t)
	box, err := provider.Open(context.Background(), "pinned-job-root")
	if err != nil {
		t.Fatal(err)
	}
	s := box.(*landlockSandbox)
	id, err := box.(JobSandbox).StartJob(context.Background(), Command{Args: []string{"sh", "-c", "printf pinned; sleep .1"}})
	if err != nil {
		t.Fatal(err)
	}
	oldPath := s.hostPath(WorkDir)
	renamed := oldPath + ".renamed"
	if err := os.Rename(oldPath, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(oldPath, 0o700); err != nil {
		t.Fatal(err)
	}
	status := waitJobDone(t, box.(JobSandbox), id)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("job status = %+v", status)
	}
	stdout, _, _, err := box.(JobOutputReader).ReadJobOutput(context.Background(), id, 100)
	if err != nil || stdout != "pinned" {
		t.Fatalf("pinned output = %q, err = %v", stdout, err)
	}
	if _, err := os.Stat(filepath.Join(oldPath, ".bonnie-jobs", id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata written to replacement directory, stat err = %v", err)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := box.(JobSandbox).JobStatus(context.Background(), id); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed status err = %v, want ErrClosed", err)
	}
	if err := box.(JobSandbox).CancelJob(context.Background(), id); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed cancel err = %v, want ErrClosed", err)
	}
}

func TestLandlockJobRefusesWriteOutside(t *testing.T) {
	t.Parallel()
	provider := landlockProvider(t)
	outside := filepath.Join(t.TempDir(), "job-escape")
	box, err := provider.Open(context.Background(), "job-write-outside")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = box.Close() }()
	id, err := box.(JobSandbox).StartJob(context.Background(), Command{Args: []string{"sh", "-c", "printf escape > \"$1\"", "sh", outside}})
	if err != nil {
		t.Fatal(err)
	}
	status := waitJobDone(t, box.(JobSandbox), id)
	if status.ExitCode == nil || *status.ExitCode == 0 {
		t.Fatalf("outside write succeeded: %+v", status)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file exists or stat failed: %v", err)
	}
}

// A provider can own many runs. Stopping one must not stop another run's work.
func TestHostJobsAreScopedToRun(t *testing.T) {
	t.Parallel()
	p := Local(WithLocalRoot(t.TempDir()))
	a, err := p.Open(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Open(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(); _ = b.Stop(context.Background()); _ = b.Close() }()
	id, err := b.(JobSandbox).StartJob(context.Background(), Command{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := b.(JobSandbox).JobStatus(context.Background(), id)
	if err != nil || !st.Running {
		t.Fatalf("other run stopped: %+v, %v", st, err)
	}
	if _, err := a.(JobSandbox).JobStatus(context.Background(), id); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("cross-run status: %v", err)
	}
	if err := a.(JobSandbox).CancelJob(context.Background(), id); !errors.Is(err, ErrJobUnknown) {
		t.Fatalf("cross-run cancel: %v", err)
	}
}
