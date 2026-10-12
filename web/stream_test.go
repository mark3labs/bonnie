package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	httpchannel "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
)

// Live text is escaped and stays outside durable history. A runtime state
// change replaces it with a snapshot, including after cancellation.
func TestLiveText(t *testing.T) {
	t.Parallel()
	journal := runtime.NewMemoryJournal()
	defer func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := journal.Checkpoint(t.Context(), "chat", runtime.RunRunning); err != nil {
		t.Fatal(err)
	}
	runner := runtime.NewRunner(journal, nil)
	h := New(runner, journal, httpchannel.New(runner))
	cookie := csrf(t, h)
	server := httptest.NewServer(h)
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/web/live?run=chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	reader := bufio.NewReader(response.Body)
	readPatch := func() string {
		t.Helper()
		var patch strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line == "\n" {
				return patch.String()
			}
			patch.WriteString(line)
		}
	}
	readPatch()
	payload, err := json.Marshal(kit.MessageUpdateEvent{Chunk: "<script>hello</script>"})
	if err != nil {
		t.Fatal(err)
	}
	runner.Events().Publish(runtime.Event{RunID: "chat", Type: string(kit.EventMessageUpdate), Data: payload})
	patch := readPatch()
	if !strings.Contains(patch, "&lt;script&gt;hello&lt;/script&gt;") || strings.Contains(patch, "<script>") || !strings.Contains(patch, `id="streaming-message"`) {
		t.Fatalf("unsafe or missing streaming text: %s", patch)
	}
	records, err := journal.Replay(t.Context(), "chat")
	if err != nil || len(records) != 1 {
		t.Fatalf("stream changed journal: %v %v", records, err)
	}
	if err := journal.Checkpoint(t.Context(), "chat", runtime.RunCancelled); err != nil {
		t.Fatal(err)
	}
	runner.Events().Publish(runtime.Event{RunID: "chat", Type: runtime.EventState, State: runtime.RunCancelled})
	patch = readPatch()
	if strings.Contains(patch, "hello") || !strings.Contains(patch, "cancelled") {
		t.Fatalf("stale streaming text: %s", patch)
	}
}

// Events queued before the snapshot must not restore text from an older turn.
// An equal-cursor delta from the snapshot's turn must still reach the stream.
func TestLiveSnapshotReconcilesQueuedTurnEvents(t *testing.T) {
	t.Parallel()
	journal := runtime.NewMemoryJournal()
	defer func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	}()
	entered := make(chan struct{})
	release := make(chan struct{})
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"chat","state":"running","cursor":2,"turn_id":"new-turn","messages":[]}`))
	})
	runner := runtime.NewRunner(journal, nil)
	h := &server{journal: journal, runner: runner, api: api}
	cookie := &http.Cookie{Name: "bonnie_web_csrf", Value: strings.Repeat("a", 64)}
	httpServer := httptest.NewServer(http.HandlerFunc(h.live))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"?run=chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	responseCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		response, err := httpServer.Client().Do(req)
		if err != nil {
			errCh <- err
			return
		}
		responseCh <- response
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("snapshot handler did not start")
	}
	publish := func(seq int, turn, chunk string) {
		t.Helper()
		data, err := json.Marshal(kit.MessageUpdateEvent{Chunk: chunk})
		if err != nil {
			t.Fatal(err)
		}
		runner.Events().Publish(runtime.Event{RunID: "chat", Type: string(kit.EventMessageUpdate), Seq: seq, TurnID: turn, Data: data})
	}
	publish(1, "old-turn", "stale-before")
	publish(2, "old-turn", "stale-equal")
	close(release)
	var response *http.Response
	select {
	case response = <-responseCh:
	case err := <-errCh:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("live request did not return")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	reader := bufio.NewReader(response.Body)
	readPatch := func() string {
		t.Helper()
		var patch strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line == "\n" {
				return patch.String()
			}
			patch.WriteString(line)
		}
	}
	initial := readPatch()
	if !strings.Contains(initial, `id="live-panel"`) {
		t.Fatalf("initial snapshot missing: %s", initial)
	}
	publish(2, "new-turn", "current-delta")
	patch := readPatch()
	if !strings.Contains(patch, "current-delta") || strings.Contains(patch, "stale-") {
		t.Fatalf("stream did not reconcile queued events: %s", patch)
	}
}

// Datastar navigation receives an element patch, not a second document.
func TestNavigationPatch(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	req := httptest.NewRequest("GET", "/web/?view=runs", nil)
	req.Header.Set("Datastar-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), `id="application"`) || strings.Contains(w.Body.String(), "<!DOCTYPE") {
		t.Fatalf("navigation: %d %s", w.Code, w.Body.String())
	}
}
