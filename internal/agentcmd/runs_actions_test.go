package agentcmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/runsui"
	"github.com/mark3labs/bonnie/runtime"
)

// seedTraceJournal writes one turn with one tool call.
func seedTraceJournal(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	j, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, rec := range []runtime.Record{
		{Kind: runtime.RecordTurn, Text: "T1"},
		{Kind: runtime.RecordState, State: runtime.RunRunning},
		{Kind: "tool_intent", Payload: json.RawMessage(`{"id":"c1","name":"shell","args":"{}"}`)},
		{Kind: "tool_outcome", Payload: json.RawMessage(`{"id":"c1","result":"ok"}`)},
		{Kind: runtime.RecordStep, Text: "stop"},
		{Kind: runtime.RecordState, State: runtime.RunCompleted},
	} {
		rec.RunID = "run-1"
		rec.Timestamp = at
		at = at.Add(time.Second)
		if _, err := j.Append(t.Context(), rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunsTraceCommand(t *testing.T) {
	t.Parallel()
	dir := seedTraceJournal(t)
	var out bytes.Buffer
	if err := executeCommand(t.Context(), NewRunsCommand(dir, ""), &out, "trace", "run-1", "--width", "90"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"turn 1", "invoke_agent", "chat · step 1", "shell"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("trace is missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := executeCommand(t.Context(), NewRunsCommand(dir, ""), &out, "trace", "run-1", "--json"); err != nil {
		t.Fatal(err)
	}
	var tr runsui.Trace
	if err := json.Unmarshal(out.Bytes(), &tr); err != nil {
		t.Fatalf("trace JSON: %v\n%s", err, out.String())
	}
	if len(tr.Spans) != 3 || tr.Duration() != 5*time.Second {
		t.Fatalf("trace = %+v", tr)
	}

	err := executeCommand(t.Context(), NewRunsCommand(dir, ""), io.Discard, "trace", "run-1", "--turn", "2")
	if err == nil || !strings.Contains(err.Error(), "turns 1 to 1") {
		t.Fatalf("out-of-range turn: %v", err)
	}
}

// recorder is a server that records each request and answers with a run.
type recorder struct {
	mu    sync.Mutex
	paths []string
	body  map[string]any
}

func (r *recorder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.Method+" "+req.URL.Path)
		r.body = nil
		_ = json.NewDecoder(req.Body).Decode(&r.body)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"run_id":"run-1","state":"completed","response":"done","status":"requested"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An approval answer carries the verdict as a field, not as words, and the
// turn it answers.
func TestRunsAnswerCommandSendsTheVerdict(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	srv := rec.server(t)
	var out bytes.Buffer
	err := executeCommand(t.Context(), NewRunsCommand(t.TempDir(), srv.URL), &out, "answer", "run-1", "--reject", "--turn", "T9", "not", "today")
	if err != nil {
		t.Fatal(err)
	}
	if rec.paths[0] != "POST /bonnie/v1/runs/run-1/respond" {
		t.Fatalf("path = %v", rec.paths)
	}
	responses, _ := rec.body["responses"].([]any)
	if len(responses) != 1 {
		t.Fatalf("body = %v", rec.body)
	}
	resp := responses[0].(map[string]any)
	if resp["approved"] != false || resp["text"] != "not today" || resp["turn_id"] != "T9" {
		t.Fatalf("response = %v", resp)
	}
	if !strings.Contains(out.String(), "state=completed") || !strings.Contains(out.String(), "done") {
		t.Fatalf("output = %q", out.String())
	}

	for _, args := range [][]string{{"answer", "run-1"}, {"answer", "run-1", "--approve", "--reject"}} {
		if err := executeCommand(t.Context(), NewRunsCommand(t.TempDir(), srv.URL), io.Discard, args...); err == nil {
			t.Fatalf("%v: want an error", args)
		}
	}
}

func TestRunsActionCommandsPostToTheRun(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	srv := rec.server(t)
	for _, c := range []struct {
		args []string
		path string
	}{
		{[]string{"cancel", "run-1"}, "POST /bonnie/v1/runs/run-1/cancel"},
		{[]string{"retire", "run-1", "--reason", "duplicate"}, "POST /bonnie/v1/runs/run-1/reset"},
		{[]string{"clear", "run-1"}, "POST /bonnie/v1/runs/run-1/clear"},
		{[]string{"compact", "run-1"}, "POST /bonnie/v1/runs/run-1/compact"},
		{[]string{"send", "run-1", "hello", "there"}, "POST /bonnie/v1/runs/run-1"},
	} {
		var out bytes.Buffer
		if err := executeCommand(t.Context(), NewRunsCommand(t.TempDir(), srv.URL), &out, c.args...); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		rec.mu.Lock()
		got, body := rec.paths[len(rec.paths)-1], rec.body
		rec.mu.Unlock()
		if got != c.path {
			t.Fatalf("%v: path %q, want %q", c.args, got, c.path)
		}
		if c.args[0] == "retire" && body["reason"] != "duplicate" {
			t.Fatalf("retire body = %v", body)
		}
		if c.args[0] == "send" && body["text"] != "hello there" {
			t.Fatalf("send body = %v", body)
		}
		if !strings.Contains(out.String(), "run-1") {
			t.Fatalf("%v: output %q", c.args, out.String())
		}
	}
}
