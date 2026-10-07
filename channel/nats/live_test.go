//go:build integration

package nats

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// TestLiveTaskCompleted proves that a task sent through a real NATS server
// runs with a live Kit model and Landlock tools, then returns its result over
// NATS. It needs a network and costs money. Run it with:
//
//	go test -race -tags integration ./channel/nats -run TestLiveTaskCompleted -count=1 -v
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
		t.Fatalf("OpenSQLiteJournal: %v", err)
	}
	defer func() {
		if err := journal.Close(); err != nil {
			t.Errorf("close journal: %v", err)
		}
	}()

	// Use the public credential option so config files and other provider
	// credentials cannot change the selected model or its API key.
	runner := runtime.NewRunner(journal, sandbox.Agent(provider,
		kit.WithModel("opencode/kimi-k3"),
		kit.WithProviderAPIKey(key),
		kit.WithSystemPrompt("Use write_file to write the requested file, then use shell to read it. "+
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

	// testServer starts an in-process server on a random loopback port.
	// Subscribe before publishing: Core NATS does not keep missed results.
	nc := testServer(t)
	const taskSubject = "live.tasks"
	const resultSubject = "live.results"
	sub, err := nc.SubscribeSync(resultSubject)
	if err != nil {
		t.Fatalf("subscribe to results: %v", err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Errorf("unsubscribe from results: %v", err)
		}
	}()
	c, err := New(runner, Config{
		Conn: nc, Subject: taskSubject, AnswerSubject: "live.answers",
		ResultSubject: resultSubject, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := c.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()

	const taskID = "live-landlock-task"
	const fileName = "nats-live-result.txt"
	const content = "nats-landlock-completed"
	send(t, nc, taskSubject, Task{
		TaskID: taskID,
		Text: "Write " + fileName + " containing exactly " + content +
			", then run cat " + fileName + " and report what it printed.",
	})
	msg, err := sub.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatalf("receive live result: %v", err)
	}
	var result Result
	if err := json.Unmarshal(msg.Data, &result); err != nil {
		t.Fatalf("decode live result: %v", err)
	}
	if result.Error != "" || result.TaskID != taskID || result.RunID == "" ||
		result.State != runtime.RunCompleted || result.Suspend != nil {
		t.Fatalf("want a completed result for %q, got %+v", taskID, result)
	}
	if !strings.Contains(result.Response, content) {
		t.Fatalf("response does not contain the file content: %q", result.Response)
	}

	// A reply alone does not prove that the model used the sandbox tools.
	// Check the file in the workspace selected by the returned run ID.
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
	t.Log("received a completed NATS result from opencode/kimi-k3 with a file in the Landlock workspace")
}
