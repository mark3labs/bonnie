package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	httpchannel "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
)

// Exercise the shipped Datastar morph, not a mock of its attribute rules.
// Chromium is optional; the server tests do not need a browser installation.
func TestBrowserLiveState(t *testing.T) {
	t.Parallel()
	browser, err := exec.LookPath("chromium")
	if err != nil {
		t.Skip("Chromium is not installed")
	}
	v := view{ID: "chat", Snapshot: httpchannel.SnapshotResponse{Suspend: &runtime.SuspendRequest{TurnID: "approval-turn", Kind: runtime.SuspendApproval}}, Messages: []message{{Role: "user", Text: "first", Payload: "first"}}}
	r := httptest.NewRequest("GET", "/web/?run=chat", nil)
	var initial, patch bytes.Buffer
	if err := renderComponent(r, panel(v), &initial); err != nil {
		t.Fatal(err)
	}
	v.Messages = append(v.Messages, message{Role: "assistant", Text: "second", Payload: "second"})
	if err := renderComponent(r, panel(v), &patch); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(patch.String())
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the trace view independently from the chat view in the same browser.
	traceView := view{View: "trace", ID: "chat", Trace: []traceRow{{Seq: 1, Preview: "first"}}}
	var traceInitial, tracePatch bytes.Buffer
	if err := renderComponent(r, panel(traceView), &traceInitial); err != nil {
		t.Fatal(err)
	}
	traceView.Trace = append(traceView.Trace, traceRow{Seq: 2, Turn: 1, Preview: "second"})
	if err := renderComponent(r, panel(traceView), &tracePatch); err != nil {
		t.Fatal(err)
	}
	traceHTML := strings.Replace(traceInitial.String(), `id="live-panel"`, `id="trace-panel"`, 1)
	traceEncoded, err := json.Marshal(strings.Replace(tracePatch.String(), `id="live-panel"`, `id="trace-panel"`, 1))
	if err != nil {
		t.Fatal(err)
	}
	fixture := fmt.Sprintf(`<!doctype html><body><span id="connection-status"></span><div id="live-connection"></div>%s%s
<script src="/web.js"></script>
<script type="module">
import '/datastar.js';
const input = document.getElementById('send-text');
input.value = 'unfinished draft';
input.focus();
input.setSelectionRange(3, 8);
const approval = document.getElementById('respond-approval');
approval.value = 'approve';
const note = document.getElementById('respond-note');
note.value = 'unfinished note';
const messageDetail = document.querySelector('.message details');
messageDetail.open = true;
const trace = document.getElementById('trace-1');
trace.open = true;
const filter = document.getElementById('trace-filter');
filter.value = 'unfinished filter';

const connection = document.getElementById('live-connection');
const emit = (type, argsRaw = {}) => document.dispatchEvent(new CustomEvent('datastar-fetch', {detail: {type, el: connection, argsRaw}}));
emit('retrying');
const retry = document.getElementById('connection-status').textContent === 'Reconnecting…';
emit('datastar-patch-elements', {mode: 'outer', elements: %s});
emit('datastar-patch-elements', {mode: 'outer', elements: %s});
const ok = approval === document.getElementById('respond-approval') && approval.value === 'approve' && note === document.getElementById('respond-note') && note.value === 'unfinished note' && retry && input === document.getElementById('send-text') && input.value === 'unfinished draft' && input.selectionStart === 3 && input.selectionEnd === 8 && messageDetail === document.querySelector('.message details') && messageDetail.open && document.querySelectorAll('.message').length===2 && trace === document.getElementById('trace-1') && trace.open && filter.value === 'unfinished filter' && document.getElementById('trace-2') && document.getElementById('connection-status').dataset.state === 'connected';
document.body.dataset.testResult = ok ? 'passed' : 'failed';
</script></body>`, initial.String(), traceHTML, encoded, traceEncoded)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, fixture)
			return
		}
		w.Header().Set("Content-Type", "text/javascript")
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "datastar.js" && name != "web.js" {
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			http.Error(w, "Missing asset", 500)
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, browser, "--headless", "--no-sandbox", "--disable-gpu", "--user-data-dir="+t.TempDir(), "--dump-dom", "--virtual-time-budget=2000", server.URL).Output()
	if err != nil {
		t.Fatalf("Chromium: %v", err)
	}
	if !strings.Contains(string(output), `data-test-result="passed"`) {
		t.Fatalf("browser state test failed: %s", output)
	}
}
