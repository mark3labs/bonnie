package web

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
