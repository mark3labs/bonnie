package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// TestShellJobDurabilityAcrossRunnerRestart checks completed and running job
// recovery through Kit tools, with only the SQLite journal crossing runners.
func TestShellJobDurabilityAcrossRunnerRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const runID = "jobs-durability"
	workRoot, journalRoot := t.TempDir(), t.TempDir()
	providerA := Local(WithLocalRoot(workRoot))
	journalA, err := runtime.OpenSQLiteJournal(journalRoot)
	if err != nil {
		t.Fatalf("open journal A: %v", err)
	}
	hermetic := func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.Quiet = true
	}
	// Turn one must see its job complete. waitJob returns when the job exits,
	// so the long wait costs nothing on a fast machine. A one-second wait was
	// not enough on a loaded CI runner: the job was still running, and the
	// test then followed this job instead of the marker job.
	completedModel := fakemodel.New(
		fakemodel.Call("shell", `{"command":"sleep .2; printf durable","wait_seconds":30}`),
		fakemodel.Say("completed"),
	)
	completed, err := runtime.NewRunner(journalA, Agent(providerA, hermetic, completedModel.Option())).Start(ctx, runID, runtime.Input{Text: "Run sleep .2; printf durable with wait_seconds=30. Report its output."})
	if err != nil {
		t.Fatalf("start completed job: %v", err)
	}
	if completed.State != runtime.RunCompleted {
		t.Fatalf("completed-job turn state=%q response=%q", completed.State, completed.Response)
	}
	if len(completedModel.Requests()) != 2 || completedModel.Remaining() != 0 {
		t.Fatalf("completed-job turn requests=%d replies remaining=%d", len(completedModel.Requests()), completedModel.Remaining())
	}
	if err := journalA.Close(); err != nil {
		t.Fatalf("close journal A: %v", err)
	}

	// Turn two starts a long job, then returns while the job is still running.
	// wait_seconds cannot be below 1, so the job must outlive one second; 1.4
	// seconds keeps a margin without a longer fixed cost.
	journalA, err = runtime.OpenSQLiteJournal(journalRoot)
	if err != nil {
		t.Fatalf("reopen journal A: %v", err)
	}
	runningModel := fakemodel.New(
		fakemodel.Call("shell", `{"command":"sleep 1.4; printf once >> marker","wait_seconds":1}`),
		fakemodel.Say("parked"),
	)
	runA, err := runtime.NewRunner(journalA, Agent(providerA, hermetic, runningModel.Option())).Start(ctx, runID, runtime.Input{Text: "Start a shell job that sleeps 1.4 seconds then appends once to marker, using wait_seconds=1, and report the job id."})
	if err != nil {
		t.Fatalf("start running job: %v", err)
	}
	if runA.State != runtime.RunCompleted {
		t.Fatalf("running-job turn state=%q response=%q", runA.State, runA.Response)
	}
	if err := journalA.Close(); err != nil {
		t.Fatalf("close journal before restart: %v", err)
	}

	// A fresh provider and Runner recover the recorded job ID from the journal.
	journalB, err := runtime.OpenSQLiteJournal(journalRoot)
	if err != nil {
		t.Fatalf("open journal B: %v", err)
	}
	defer func() { _ = journalB.Close() }()
	// Extract the real job ID from the first runner's journal-backed history.
	restored, err := runtime.Restore(ctx, runID, journalB)
	if err != nil {
		t.Fatalf("restore run history: %v", err)
	}
	jobID := findRunningJobID(t, restored.GetMessages())

	// Let the process exit before the second provider reads its receipt. A
	// separate probe provider polls, so providerB starts with no state.
	waitJobExit(t, Local(WithLocalRoot(workRoot)), runID, jobID)
	providerB := Local(WithLocalRoot(workRoot))
	modelB := fakemodel.New(fakemodel.Call("shell_status", `{"job_id":"`+jobID+`"}`), fakemodel.Say("recovered"))
	runB, err := runtime.NewRunner(journalB, Agent(providerB, hermetic, modelB.Option())).Start(ctx, runID, runtime.Input{Text: "Check the prior job with shell_status using its recorded job id. Report its output."})
	if err != nil {
		t.Fatalf("recover job in runner B: %v", err)
	}
	if runB.State != runtime.RunCompleted || runB.Response != "recovered" {
		t.Fatalf("runner B state=%q response=%q", runB.State, runB.Response)
	}
	requests := modelB.Requests()
	if len(requests) != 2 || modelB.Remaining() != 0 {
		t.Fatalf("runner B requests=%d replies remaining=%d, want two tool/answer turns", len(requests), modelB.Remaining())
	}
	st, err := providerB.Open(ctx, runID)
	if err != nil {
		t.Fatalf("open recovered sandbox: %v", err)
	}
	jobStatus, err := st.(JobSandbox).JobStatus(ctx, jobID)
	_ = st.Close()
	if err != nil || jobStatus.Running || jobStatus.ExitCode == nil || *jobStatus.ExitCode != 0 {
		t.Fatalf("recovered status=%+v err=%v", jobStatus, err)
	}
	sb, err := providerB.Open(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()
	marker, err := sb.ReadFile(ctx, "marker")
	if err != nil || string(marker) != "once" {
		t.Fatalf("marker=%q err=%v, want one append", marker, err)
	}
}

// waitJobExit polls a job until its exit receipt is on disk. It replaces a
// fixed sleep sized to the job, which cost the full job time on every run.
//
// A provider that did not start the job reads only the receipt, which the
// starting provider writes after the process exits. Until then it reports
// [ErrJobUnknown], so that error means "not yet", not failure.
func waitJobExit(t *testing.T, p Provider, runID, jobID string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for {
		sb, err := p.Open(ctx, runID)
		if err != nil {
			t.Fatalf("open probe sandbox: %v", err)
		}
		st, err := sb.(JobSandbox).JobStatus(ctx, jobID)
		_ = sb.Close()
		switch {
		case errors.Is(err, ErrJobUnknown):
		case err != nil:
			t.Fatalf("probe job status: %v", err)
		case !st.Running:
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still running after 30s", jobID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// findRunningJobID returns the job ID of the newest tool result that reports a
// running job. It must be the newest: an older turn's job can also be recorded
// as running when a slow machine misses that turn's wait, and that job does not
// write the marker.
func findRunningJobID(t *testing.T, messages []kit.LLMMessage) string {
	t.Helper()
	found := ""
	for _, message := range messages {
		for _, part := range message.Content {
			result, ok := part.(kit.LLMToolResultPart)
			if !ok {
				continue
			}
			var data struct {
				JobID   string `json:"job_id"`
				Running bool   `json:"running"`
			}
			if json.Unmarshal([]byte(result.Output.(kit.LLMToolResultOutputContentText).Text), &data) == nil && data.Running && data.JobID != "" {
				found = data.JobID
			}
		}
	}
	if found == "" {
		t.Fatal("journal has no running job result")
	}
	return found
}
