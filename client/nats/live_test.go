//go:build integration

package nats

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	transport "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// TestLiveTaskCompleted sends a task through the typed JetStream client to a
// live Kit agent with Landlock tools. A completed result must name a run whose
// workspace contains the requested file. It needs a network and costs money:
//
//	go test -race -tags integration ./client/nats -run TestLiveTaskCompleted -count=1 -v
//
// It skips when OPENCODE_API_KEY is absent or Landlock is not available.
func TestLiveTaskCompleted(t *testing.T) {
	t.Parallel()
	key := os.Getenv("OPENCODE_API_KEY")
	if key == "" {
		t.Skip("no provider credential: set OPENCODE_API_KEY for opencode/kimi-k3")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	provider := sandbox.Landlock(sandbox.WithLandlockRoot(t.TempDir()), sandbox.WithLandlockCleanup())
	if err := provider.Available(ctx); err != nil {
		t.Skipf("Landlock is not available: %v", err)
	}

	journal, err := runtime.OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer func() {
		if err := journal.Close(); err != nil {
			t.Errorf("close journal: %v", err)
		}
	}()

	// No host configuration or discovery can change the live agent.
	runner := runtime.NewRunner(journal, sandbox.Agent(provider,
		kit.WithModel("opencode/kimi-k3"),
		kit.WithProviderAPIKey(key),
		kit.WithSystemPrompt("Use write_file to write the requested file, then use bash to read it. "+
			"Use only relative workspace paths. Reply with the file content in one short sentence. "+
			"Do not ask for human input or do any other work."),
		func(o *kit.Options) {
			o.MaxSteps = 6
			o.SkipConfig = true
			o.NoContextFiles = true
			o.NoSkills = true
			o.NoExtensions = true
			o.NoAgents = true
			o.Quiet = true
		},
	))

	// The client creates the result stream. The channel creates the input
	// stream for tasks and worker answers, and uses a durable JS consumer.
	nc, _ := broker(t)
	cfg := config()
	c, err := New(nc, cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	const inputStream = "INPUT"
	const workerID = "live-worker"
	ch, err := transport.New(runner, transport.Config{
		Conn: nc, Subject: cfg.TaskSubject, ResultSubject: cfg.ResultSubject,
		AnswerSubject: cfg.AnswerSubject, Stream: inputStream, Consumer: "workers",
		WorkerID: workerID, CreateStream: true, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := ch.Start(ctx); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := ch.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown channel: %v", err)
		}
	}()

	const taskID = "live-client-landlock-task"
	const fileName = "nats-client-live-result.txt"
	const content = "nats-client-landlock-completed"
	// Leave Version unset to test Submit's version 1 default.
	receipt, err := c.Submit(ctx, Task{
		TaskID: taskID,
		Text: "Write " + fileName + " containing exactly " + content +
			", then run cat " + fileName + " and report what it printed.",
	})
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}
	if receipt.TaskID != taskID || receipt.Stream != inputStream || receipt.Sequence == 0 || receipt.Duplicate {
		t.Fatalf("unexpected submit receipt: %+v", receipt)
	}

	finished := errors.New("received live result")
	var result Outcome
	err = c.Consume(ctx, func(_ context.Context, outcome Outcome) error {
		result = outcome
		return finished
	})
	if !errors.Is(err, finished) {
		t.Fatalf("consume live result: %v", err)
	}
	if result.Version != 1 || result.TaskID != taskID || result.RunID == "" ||
		result.WorkerID != workerID || result.Error != "" ||
		result.State != runtime.RunCompleted || result.Suspend != nil {
		t.Fatalf("want a completed result for %q, got %+v", taskID, result)
	}
	if !strings.Contains(result.Response, content) {
		t.Fatalf("response does not contain the file content: %q", result.Response)
	}

	// A model reply alone does not prove that it used the sandbox tools.
	sb, err := provider.Open(ctx, result.RunID)
	if err != nil {
		t.Fatalf("open result sandbox: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := provider.DeleteRun(cleanupCtx, result.RunID); err != nil {
			t.Errorf("delete result sandbox: %v", err)
		}
	}()
	data, err := sb.ReadFile(ctx, fileName)
	if err != nil {
		t.Fatalf("read sandbox file: %v", err)
	}
	if strings.TrimSpace(string(data)) != content {
		t.Fatalf("sandbox file = %q, want %q", data, content)
	}
	state, err := journal.State(ctx, result.RunID)
	if err != nil || state != runtime.RunCompleted {
		t.Fatalf("journal state = %q, %v, want completed", state, err)
	}
	t.Log("received a completed typed JetStream result from opencode/kimi-k3 with a file in the Landlock workspace")
}
