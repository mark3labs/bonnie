package nats

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// Redelivery after interruption resumes the admitted attempt/run without a cached outcome.
func TestJetStreamInterruptedAttempt(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	resultStream(t, js)
	inputStream(t, js, 100*time.Millisecond)
	journal := runtime.NewMemoryJournal()
	var calls atomic.Int32
	c := jsChannel(t, slowRunner(journal, &calls, time.Hour), jsConfig(nc, "one"))
	jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("task did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids, err := journal.Runs(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	original := ""
	for _, id := range ids {
		if !runtime.IsReservedRun(id) {
			original = id
		}
	}
	if original == "" {
		t.Fatal("missing original run")
	}
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	jsChannel(t, slowRunner(journal, &calls, 0), jsConfig(nc, "one"))
	result := receive(t, sub)
	if result.Error != "" || result.RunID != original || result.AttemptID == "" {
		t.Fatalf("attempt: %+v original %s", result, original)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls: %d", calls.Load())
	}
}

// Legacy version zero remains valid only in Core mode. A JetStream rejection
// is published and acknowledged like every other completed outcome.
func TestJetStreamRequiresVersion(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	resultStream(t, js)
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	jsChannel(t, testRunner(fakemodel.New()), jsConfig(nc, "one"))
	jsSend(t, js, "tasks", Task{TaskID: "a", Text: "work"})
	result := receive(t, sub)
	if result.Version != 1 || result.Error != "JetStream requires version 1" || result.RunID != "" {
		t.Fatalf("result: %+v", result)
	}
}
