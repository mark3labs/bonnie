package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJobCapabilitySurvivesProviderWrappers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		wrap func(Provider) Provider
	}{
		{name: "environment", wrap: func(p Provider) Provider {
			return EnvInjected(p, map[string]string{"JOB_WRAPPER_TEST": "present"})
		}},
		{name: "seeded", wrap: func(p Provider) Provider { return Seeded(p, t.TempDir()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.wrap(Local(WithLocalRoot(t.TempDir())))
			sb, err := p.Open(ctx, "wrapped-job-"+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sb.Close() }()
			jobs, ok := sb.(JobSandbox)
			if !ok {
				t.Fatal("wrapper dropped JobSandbox")
			}
			cmd := Command{Args: []string{"sh", "-c", "printf ready"}}
			id, err := jobs.StartJob(ctx, cmd)
			if err != nil {
				t.Fatalf("StartJob: %v", err)
			}
			var status *JobStatus
			for range 100 {
				status, err = jobs.JobStatus(ctx, id)
				if err != nil {
					t.Fatalf("JobStatus: %v", err)
				}
				if !status.Running {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if status.Running {
				t.Fatal("job did not finish")
			}

		})
	}
}

func TestLocalHostJobLifecycle(t *testing.T) {
	t.Parallel()
	p := Local(WithLocalRoot(t.TempDir()))
	sb, err := p.Open(context.Background(), "job-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()
	jobs, ok := sb.(JobSandbox)
	if !ok {
		t.Fatal("local sandbox lacks JobSandbox")
	}
	ctx, cancel := context.WithCancel(context.Background())
	id, err := jobs.StartJob(ctx, Command{Args: []string{"sh", "-c", "printf hello; printf warning >&2"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	var st *JobStatus
	for range 100 {
		st, err = jobs.JobStatus(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.Running || st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("unexpected status: %+v", st)
	}
	out, err := os.ReadFile(filepath.Join(p.WorkingDir("job-test"), filepath.FromSlash(st.StdoutPath)))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(filepath.Join(p.WorkingDir("job-test"), filepath.FromSlash(st.StderrPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello" || string(stderr) != "warning" {
		t.Fatalf("outputs %q %q", out, stderr)
	}
	if _, err = jobs.JobStatus(context.Background(), "00000000000000000000000000000000"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("unknown job error %v", err)
	}
}

func TestLocalHostJobRestartStatus(t *testing.T) {
	t.Parallel()
	provider := Local(WithLocalRoot(t.TempDir()))
	first, err := provider.Open(context.Background(), "restart")
	if err != nil {
		t.Fatal(err)
	}
	jobs := first.(JobSandbox)
	id, err := jobs.StartJob(context.Background(), Command{Args: []string{"sh", "-c", "printf persisted"}})
	if err != nil {
		t.Fatal(err)
	}
	var status *JobStatus
	for range 200 {
		status, err = jobs.JobStatus(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status.Running {
		t.Fatal("job did not finish")
	}
	second, err := provider.Open(context.Background(), "restart")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	recovered, err := second.(JobSandbox).JobStatus(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Running || recovered.ExitCode == nil || *recovered.ExitCode != 0 {
		t.Fatalf("bad recovered status: %+v", recovered)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalHostJobSurvivesHandleAndCanBeCancelled(t *testing.T) {
	t.Parallel()
	provider := Local(WithLocalRoot(t.TempDir()))
	first, err := provider.Open(context.Background(), "turns")
	if err != nil {
		t.Fatal(err)
	}
	id, err := first.(JobSandbox).StartJob(context.Background(), Command{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := provider.Open(context.Background(), "turns")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	jobs := second.(JobSandbox)
	status, err := jobs.JobStatus(context.Background(), id)
	if err != nil || !status.Running {
		t.Fatalf("job status %+v, err %v", status, err)
	}
	if err := jobs.CancelJob(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	status, err = jobs.JobStatus(context.Background(), id)
	if err != nil || status.Running {
		t.Fatalf("after cancel: %+v, err %v", status, err)
	}
	stopID, err := jobs.StartJob(context.Background(), Command{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = jobs.JobStatus(context.Background(), stopID)
	if err != nil || status.Running {
		t.Fatalf("after stop: %+v, err %v", status, err)
	}
}

func TestLocalHostJobNewProviderRecovery(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	firstProvider := Local(WithLocalRoot(root))
	first, err := firstProvider.Open(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	id, err := first.(JobSandbox).StartJob(context.Background(), Command{Args: []string{"sh", "-c", "sleep .1"}})
	if err != nil {
		t.Fatal(err)
	}
	for range 200 {
		st, e := first.(JobSandbox).JobStatus(context.Background(), id)
		if e != nil {
			t.Fatal(e)
		}
		if !st.Running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_ = first.Close()
	second, err := Local(WithLocalRoot(root)).Open(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	st, err := second.(JobSandbox).JobStatus(context.Background(), id)
	if err != nil || st.Running || st.ExitCode == nil {
		t.Fatalf("completed status %+v, err %v", st, err)
	}
	active, err := firstProvider.Open(context.Background(), "active")
	if err != nil {
		t.Fatal(err)
	}
	activeID, err := active.(JobSandbox).StartJob(context.Background(), Command{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, err := firstProvider.Open(context.Background(), "active")
		if err != nil {
			t.Error(err)
			return
		}
		if err := cleanup.(JobSandbox).CancelJob(context.Background(), activeID); err != nil {
			t.Error(err)
		}
		_ = cleanup.Close()
	}()
	_ = active.Close()
	other, err := Local(WithLocalRoot(root)).Open(context.Background(), "active")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.(JobSandbox).JobStatus(context.Background(), activeID); !errors.Is(err, ErrJobUnknown) {
		t.Fatalf("active job from new provider: %v", err)
	}
}

func TestLocalHostJobCancelAndOutputBound(t *testing.T) {
	t.Parallel()
	sb0, err := Local(WithLocalRoot(t.TempDir())).Open(context.Background(), "cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb0.Close() }()
	jobs := sb0.(JobSandbox)
	id, err := jobs.StartJob(context.Background(), Command{Args: []string{"sh", "-c", "yes x"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.CancelJob(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Write a fixed payload after cancellation; the job output file has no disk cap.
	if err := os.WriteFile(filepath.Join(sb0.(*localSandbox).dir, ".bonnie-jobs", id+".stdout"), make([]byte, 256), 0o600); err != nil {
		t.Fatal(err)
	}
	var st *JobStatus
	for range 100 {
		st, err = jobs.JobStatus(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.Running {
		t.Fatal("job still running")
	}
	reader, ok := sb0.(JobOutputReader)
	if !ok {
		t.Fatal("local sandbox lacks JobOutputReader")
	}
	stdout, stderr, truncated, err := reader.ReadJobOutput(context.Background(), id, 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) > 128 || len(stderr) > 128 || !truncated {
		t.Fatalf("bounded output: stdout=%d stderr=%d truncated=%v", len(stdout), len(stderr), truncated)
	}
}
