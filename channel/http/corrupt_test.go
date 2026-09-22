package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// plantCorruptRun writes a journal whose conversation holds an unanswered
// tool call that is NOT at the tail: an assistant message that calls a tool,
// then an ordinary user message with no tool result in between.
//
// A tail orphan is a torn write and [runtime.Restore] repairs it. This shape
// cannot be repaired — dropping a step with live conversation after it would
// rewrite history — so Restore refuses with [runtime.ErrCorruptConversation].
func plantCorruptRun(t *testing.T, j runtime.Journal, runID string) {
	t.Helper()
	ctx := context.Background()

	write := func(entryID, parentID, role string, msg kit.LLMMessage) {
		t.Helper()
		payload, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("encode %s: %v", entryID, err)
		}
		if _, err := j.Append(ctx, runtime.Record{
			RunID:    runID,
			Kind:     runtime.RecordMessage,
			EntryID:  entryID,
			ParentID: parentID,
			Role:     role,
			Payload:  payload,
		}); err != nil {
			t.Fatalf("append %s: %v", entryID, err)
		}
	}

	write("m-1", "", "user", kit.NewLLMUserMessage("deploy the app"))
	write("m-2", "m-1", "assistant", kit.LLMMessage{
		Role: kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{
			kit.LLMToolCallPart{ToolCallID: "call-1", ToolName: "bash"},
		},
	})
	// The message that makes the orphan unrepairable: the conversation
	// carried on past the unanswered call.
	write("m-3", "m-2", "user", kit.NewLLMUserMessage("are you there?"))

	if err := j.Checkpoint(ctx, runID, runtime.RunCompleted); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// A journal that [runtime.Restore] refuses is a conflict, not a fault in
// this request.
//
// It used to fall through writeError's default and reach the client as an
// opaque 500 with code "internal". A 500 invites a retry, and no retry can
// fix a damaged journal — the same argument writeError already makes for
// [runtime.ErrRunOwnedElsewhere]. The client gets a code it can switch on
// and stop; the operator gets the detail on stderr.
func TestCorruptConversationIsAConflict(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	s := newTestServerOn(t, j, &stubAgent{turns: []*kit.TurnResult{{Response: "hi"}}})
	plantCorruptRun(t, j, "corrupt-run")

	status, body := s.postRaw(t, "/bonnie/v1/runs/corrupt-run", SendRequest{Text: "hello?"})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", status, body)
	}

	var got ErrorResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got.Code != errCorrupt {
		t.Fatalf("code = %q, want %q", got.Code, errCorrupt)
	}
	if got.Code == errInternal {
		t.Fatal("a corrupt journal still reaches the client as an opaque 500")
	}
	// The text has to say what is wrong. An operator reading a ticket needs
	// more than a status code, and this error names no path and no SQL —
	// it is the sentinel's own wording.
	if !strings.Contains(strings.ToLower(got.Error), "corrupt") {
		t.Fatalf("the error does not say what happened: %q", got.Error)
	}
}

// The repairable shape must NOT take the corrupt path: an unanswered tool
// call at the tail is a torn write, Restore drops it, and the turn runs.
// Mapping the sentinel must not turn every orphan into a 409.
func TestTornWriteStillResumes(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	s := newTestServerOn(t, j, &stubAgent{turns: []*kit.TurnResult{{Response: "resumed"}}})

	ctx := context.Background()
	write := func(entryID, parentID, role string, msg kit.LLMMessage) {
		t.Helper()
		payload, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := j.Append(ctx, runtime.Record{
			RunID: "torn-run", Kind: runtime.RecordMessage,
			EntryID: entryID, ParentID: parentID, Role: role, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("m-1", "", "user", kit.NewLLMUserMessage("deploy the app"))
	write("m-2", "m-1", "assistant", kit.LLMMessage{
		Role: kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{
			kit.LLMToolCallPart{ToolCallID: "call-1", ToolName: "bash"},
		},
	})
	if err := j.Checkpoint(ctx, "torn-run", runtime.RunCompleted); err != nil {
		t.Fatal(err)
	}

	resp, run := s.post(t, "/bonnie/v1/runs/torn-run", SendRequest{Text: "still there?"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a repairable torn write got %d, want 200", resp.StatusCode)
	}
	if run.Response != "resumed" {
		t.Fatalf("response = %q, want the turn to have run", run.Response)
	}
}
