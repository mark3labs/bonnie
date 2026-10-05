package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
)

// A new Runner shares only the journal. Snapshot must recover all messages,
// including typed tool parts, and the waiting prompt without a model call.
// A completed turn between the snapshot and subscription must not be lost.
func TestSnapshotRestoredRunnerAndStreamJoin(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j, err := runtime.OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	first := newTestServerOn(t, j, &stubAgent{})
	if _, err := first.runner.Start(ctx, "snapshot-run", runtime.Input{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	s, err := runtime.Restore(ctx, "snapshot-run", j)
	if err != nil {
		t.Fatal(err)
	}
	messages := []kit.LLMMessage{
		kit.NewLLMUserMessage("second"),
		{Role: "assistant", Content: []kit.LLMMessagePart{
			kit.LLMTextPart{Text: "checking"},
			kit.LLMToolCallPart{ToolCallID: "call-1", ToolName: "read", Input: `{}`},
		}},
		{Role: "tool", Content: []kit.LLMMessagePart{
			kit.LLMToolResultPart{ToolCallID: "call-1", Output: kit.LLMToolResultOutputContentText{Text: "result"}},
		}},
	}
	if _, err := s.AppendStep(ctx, messages); err != nil {
		t.Fatal(err)
	}
	sus := runtime.SuspendRequest{Prompt: "which region?"}
	payload, err := json.Marshal(sus)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, runtime.Record{RunID: "snapshot-run", Kind: runtime.RecordSuspend, Text: sus.Prompt, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := j.Checkpoint(ctx, "snapshot-run", runtime.RunWaiting); err != nil {
		t.Fatal(err)
	}
	second := newTestServerOn(t, j, &stubAgent{})
	c := client.New(second.URL)
	snapshot, err := c.Snapshot(ctx, "snapshot-run")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != runtime.RunWaiting || snapshot.Suspend == nil || snapshot.Suspend.Prompt != sus.Prompt {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if !reflect.DeepEqual(snapshot.Messages, s.GetMessages()) {
		t.Fatalf("messages = %#v, want %#v", snapshot.Messages, s.GetMessages())
	}
	if second.agent.calls() != 0 {
		t.Fatal("snapshot executed a model turn")
	}
	// Preserve the public Get contract while adding the richer method.
	run, err := c.Get(ctx, "snapshot-run")
	if err != nil || run.State != runtime.RunWaiting {
		t.Fatalf("Get = %+v, %v", run, err)
	}
	if _, err := c.Respond(ctx, "snapshot-run", "eu-west"); err != nil {
		t.Fatal(err)
	}
	resumed, err := c.Snapshot(ctx, "snapshot-run")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != runtime.RunCompleted || resumed.Suspend != nil || len(resumed.Messages) <= len(snapshot.Messages) {
		t.Fatalf("resumed snapshot = %+v", resumed)
	}
	events, stop, err := c.Stream(ctx, "snapshot-run", snapshot.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	responses := 0
	for {
		select {
		case ev := <-events:
			if ev.Seq <= snapshot.Cursor {
				t.Fatalf("snapshot event replayed: %+v", ev)
			}
			if ev.Type == runtime.EventResponse {
				responses++
			}
			if ev.Type == runtime.EventState && ev.State == runtime.RunCompleted {
				if responses != 1 {
					t.Fatalf("responses = %d", responses)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// This journal writes just after returning a replay view. A later Position or
// State read would move the snapshot beyond that view and create a stream gap.
type advancingJournal struct {
	runtime.Journal
	advance bool
}

func (j *advancingJournal) Replay(ctx context.Context, id string) ([]runtime.Record, error) {
	recs, err := j.Journal.Replay(ctx, id)
	if j.advance && err == nil {
		j.advance = false
		err = j.Checkpoint(ctx, id, runtime.RunCompleted)
	}
	return recs, err
}

func TestSnapshotStateAndCursorUseSameReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := runtime.NewMemoryJournal()
	if err := base.Checkpoint(ctx, "same-view", runtime.RunPending); err != nil {
		t.Fatal(err)
	}
	j := &advancingJournal{Journal: base}
	r := runtime.NewRunner(j, nil)
	h := New(r).Handler()
	// Attach checks existence through State. The next Replay is the
	// snapshot view; the added record must stay outside its cursor.
	j.advance = true
	request := httptest.NewRequest(http.MethodGet, "/bonnie/v1/runs/same-view/snapshot", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request)
	var snapshot SnapshotResponse
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor != 1 || snapshot.State != runtime.RunPending {
		t.Fatalf("snapshot = %+v (status %d)", snapshot, w.Code)
	}
}

// Display history follows the selected branch, not every append in the
// journal. A clear hides old messages without deleting their durable records.
func TestSnapshotSelectedBranchAndClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := runtime.NewMemoryJournal()
	if err := j.Checkpoint(ctx, "branch", runtime.RunPending); err != nil {
		t.Fatal(err)
	}
	s := runtime.NewSession("branch", j)
	root, err := s.AppendMessage(kit.NewLLMUserMessage("root"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(kit.NewLLMUserMessage("abandoned")); err != nil {
		t.Fatal(err)
	}
	if err := s.Branch(root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(kit.NewLLMUserMessage("selected")); err != nil {
		t.Fatal(err)
	}
	srv := newTestServerOn(t, j, &stubAgent{})
	c := client.New(srv.URL)
	snapshot, err := c.Snapshot(ctx, "branch")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Messages, s.GetMessages()) || len(snapshot.Messages) != 2 {
		t.Fatalf("selected messages = %+v", snapshot.Messages)
	}
	if _, err := j.Append(ctx, runtime.Record{RunID: "branch", Kind: runtime.RecordClear}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = c.Snapshot(ctx, "branch")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 0 || snapshot.Response != "" {
		t.Fatalf("clear snapshot = %+v", snapshot)
	}
}
