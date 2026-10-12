package sandbox

import (
	"context"
	"encoding/json"
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
	completedModel := fakemodel.New(
		fakemodel.Call("shell", `{"command":"sleep .2; printf durable","wait_seconds":1}`),
		fakemodel.Say("completed"),
	)
	completed, err := runtime.NewRunner(journalA, Agent(providerA, hermetic, completedModel.Option())).Start(ctx, runID, runtime.Input{Text: "Run sleep .2; printf durable with wait_seconds=1. Report its output."})
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
	journalA, err = runtime.OpenSQLiteJournal(journalRoot)
	if err != nil {
		t.Fatalf("reopen journal A: %v", err)
	}
	runningModel := fakemodel.New(
		fakemodel.Call("shell", `{"command":"sleep 2; printf once >> marker","wait_seconds":1}`),
		fakemodel.Say("parked"),
	)
	runA, err := runtime.NewRunner(journalA, Agent(providerA, hermetic, runningModel.Option())).Start(ctx, runID, runtime.Input{Text: "Start a shell job that sleeps 2 seconds then appends once to marker, using wait_seconds=1, and report the job id."})
	if err != nil {
		t.Fatalf("start running job: %v", err)
	}
	if runA.State != runtime.RunCompleted {
		t.Fatalf("running-job turn state=%q response=%q", runA.State, runA.Response)
	}
	if err := journalA.Close(); err != nil {
		t.Fatalf("close journal before restart: %v", err)
	}

	// Allow the process to exit before the second provider reads its receipt.
	time.Sleep(2100 * time.Millisecond)

	// A fresh provider and Runner recover the recorded job ID from the journal.
	journalB, err := runtime.OpenSQLiteJournal(journalRoot)
	if err != nil {
		t.Fatalf("open journal B: %v", err)
	}
	defer func() { _ = journalB.Close() }()
	providerB := Local(WithLocalRoot(workRoot))
	// Extract the real job ID from the first runner's journal-backed history.
	restored, err := runtime.Restore(ctx, runID, journalB)
	if err != nil {
		t.Fatalf("restore run history: %v", err)
	}
	jobID := findRunningJobID(t, restored.GetMessages())
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

func findRunningJobID(t *testing.T, messages []kit.LLMMessage) string {
	t.Helper()
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
				return data.JobID
			}
		}
	}
	t.Fatal("journal has no running job result")
	return ""
}
