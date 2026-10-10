package nats

import (
	"context"
	"encoding/json"
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

// A task delivery can survive its owner and the cancellation checkpoint.
// Retrying that admission publishes cancellation, not a replacement turn.
func TestJetStreamCancelledAdmission(t *testing.T) {
	t.Parallel()
	for _, state := range []runtime.RunState{runtime.RunInterrupted, runtime.RunRunning, runtime.RunWaiting} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			nc, js := jsServer(t)
			resultStream(t, js)
			inputStream(t, js, 100*time.Millisecond)
			journal := runtime.NewMemoryJournal()
			var calls atomic.Int32
			owner := slowRunner(journal, &calls, 0)
			cfg := jsConfig(nc, "one")
			first, err := New(owner, cfg)
			if err != nil {
				t.Fatal(err)
			}
			runID, err := first.core.From("js/attempt").RunID(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			admission := admittedTask{RunID: runID, Result: Result{Version: 1, TaskID: "a", AttemptID: "attempt", AgentID: "one", AnswerSubject: "answers.one"}}
			data, err := json.Marshal(admission)
			if err != nil {
				t.Fatal(err)
			}
			recs := []runtime.Record{
				{RunID: first.cacheKey("input", "1"), Kind: runtime.RecordExtensionData, ExtType: "nats_task_attempt_admitted", Payload: data},
				{RunID: runID, Kind: runtime.RecordTurn, Text: "old"},
				{RunID: runID, Kind: runtime.RecordState, State: state},
			}
			for _, rec := range recs {
				if _, err := journal.Append(t.Context(), rec); err != nil {
					t.Fatal(err)
				}
			}
			if state == runtime.RunRunning {
				// Simulate death after intent but before the final checkpoint.
				if _, err := journal.Append(t.Context(), runtime.Record{RunID: runID, Kind: runtime.RecordCancel, Text: "old"}); err != nil {
					t.Fatal(err)
				}
			} else {
				result, err := owner.RequestCancel(t.Context(), runID, "old")
				if err != nil || result.Status != runtime.CancelRequested {
					t.Fatalf("cancel: %+v %v", result, err)
				}
			}
			sub, err := nc.SubscribeSync("results")
			if err != nil {
				t.Fatal(err)
			}
			// The new channel and Runner share only the journal with the old owner.
			second := jsChannel(t, slowRunner(journal, &calls, 0), cfg)
			jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
			result := receive(t, sub)
			if result.Error != "" || result.State != runtime.RunCancelled || result.RunID != runID || result.AttemptID != "attempt" || calls.Load() != 0 {
				t.Fatalf("retry: %+v calls=%d", result, calls.Load())
			}
			cached, err := second.loadResult(t.Context(), second.cacheKey("input", "1"))
			if err != nil || cached == nil || cached.State != runtime.RunCancelled {
				t.Fatalf("cache: %+v %v", cached, err)
			}
		})
	}
}
