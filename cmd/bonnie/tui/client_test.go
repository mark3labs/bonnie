package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/runtime"
)

// TestHTTPClientWire drives the HTTP client against a fake channel that speaks
// the wire shapes the real one does. It pins that the client decodes the same
// JSON a server emits, so the TUI and the channel cannot drift.
func TestHTTPClientWire(t *testing.T) {
	t.Parallel()

	// The fake channel: POST /runs starts a run, /runs/{id}/respond answers it.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id": "run-42",
			"state":  "waiting",
			"suspend": map[string]any{
				"prompt": "which region?",
			},
		})
	})
	mux.HandleFunc("POST /runs/run-42/respond", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":   "run-42",
			"state":    "completed",
			"response": "eu-west-1",
		})
	})
	cancelled := make(chan struct{}, 1)
	mux.HandleFunc("POST /runs/run-42/cancel", func(w http.ResponseWriter, _ *http.Request) {
		cancelled <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /runs/run-42/stream", func(w http.ResponseWriter, r *http.Request) {
		evs := []runtime.Event{
			{RunID: "run-42", Seq: 1, Type: runtime.EventState, State: runtime.RunRunning},
			{RunID: "run-42", Seq: 2, Type: runtime.EventSuspend, Text: "which region?"},
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		flusher, _ := w.(http.Flusher)
		for _, ev := range evs {
			_ = enc.Encode(ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTP(srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	started, err := c.Start(ctx, "tui-test", "deploy")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.ID != "run-42" || started.State != runtime.RunWaiting {
		t.Fatalf("Start = %+v, want run-42 waiting", started)
	}
	if started.Suspend == nil || started.Suspend.Prompt != "which region?" {
		t.Fatalf("Suspend = %+v, want the question", started.Suspend)
	}

	done, err := c.Respond(ctx, "run-42", "eu-west-1")
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if done.State != runtime.RunCompleted || done.Response != "eu-west-1" {
		t.Fatalf("Respond = %+v, want completed eu-west-1", done)
	}
	if err := c.Cancel(ctx, "run-42"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel route was not called")
	}

	ch, _, err := c.Stream(ctx, "run-42", 0)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got []runtime.Event
	for ev := range ch {
		got = append(got, ev)
		if len(got) >= 2 {
			break
		}
	}
	if len(got) != 2 {
		t.Fatalf("stream got %d events, want 2", len(got))
	}
	if got[0].Type != runtime.EventState || got[1].Type != runtime.EventSuspend {
		t.Fatalf("stream = %+v", got)
	}
}

func TestHTTPClientCloseStreams(t *testing.T) {
	t.Parallel()
	closed := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /runs/run-1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		close(closed)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTP(srv.URL, nil)
	ch, _, err := c.Stream(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	c.CloseStreams()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("server stream stayed open")
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("client stream stayed open")
		}
	case <-time.After(time.Second):
		t.Fatal("client stream did not close")
	}
}

// TestHTTPClientNotFound: a 404 surfaces as ErrNotFound, telling the model to
// start a conversation rather than treat it as a fault.
func TestHTTPClientNotFound(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs/nope", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"bonnie: run not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTP(srv.URL, nil)
	_, err := c.Send(context.Background(), "nope", "hello")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the not-found marker", err)
	}
}
