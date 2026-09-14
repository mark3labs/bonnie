package http

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// TestOversizedBodyIsRefused caps what a client can make the server buffer.
// A turn's text is a message, not a file. The webhook adapters have always
// passed their bodies through http.MaxBytesReader; the channel's own routes
// read whatever arrived until the JSON decoder gave up, so one request could
// hold as much memory as a client cared to send.
func TestOversizedBodyIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})

	body := `{"text":"` + strings.Repeat("a", maxRequestBody+1024) + `"}`
	resp, err := http.Post(s.URL+"/bonnie/v1/runs", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST /bonnie/v1/runs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	// Nothing may have started: an oversized body is refused before the run.
	ids, err := s.runner.Journal().Runs(t.Context(), "")
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("an oversized request started %d run(s)", len(ids))
	}
}

// TestBodyAtTheLimitIsAccepted pins the other side of the cap, so a future
// change cannot make the limit so tight that ordinary messages fail.
func TestBodyAtTheLimitIsAccepted(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "read it"}}})

	resp, run := s.post(t, "/bonnie/v1/runs", StartRequest{Text: strings.Repeat("a", 64<<10)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if run.Response != "read it" {
		t.Fatalf("run = %+v", run)
	}
}

// TestReservedRunIsNotAddressable is invariant 8 on the wire. The address
// map lives in a run under runtime.ReservedRunPrefix, and that run has a
// state, so Attach accepted it: a caller who knew the prefix could run a
// model turn inside the store every address binding lives in, and read its
// records back through the stream. Reserved runs are BONNIE's, so the answer
// is the one an unknown run gets.
func TestReservedRunIsNotAddressable(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})

	// Bind an address, which creates the reserved run for real.
	if resp, _ := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi", Address: "slack/C1"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	reserved := runtime.ReservedRunPrefix + "addresses"
	if _, err := s.runner.Journal().State(t.Context(), reserved); err != nil {
		t.Fatalf("the reserved run was never written, so this test proves nothing: %v", err)
	}

	for _, tc := range []struct {
		name string
		do   func() *http.Response
	}{
		{"send", func() *http.Response {
			r, _ := s.post(t, "/bonnie/v1/runs/"+reserved, SendRequest{Text: "own it"})
			return r
		}},
		{"get", func() *http.Response { r, _ := s.get(t, "/bonnie/v1/runs/"+reserved); return r }},
		{"respond", func() *http.Response {
			r, _ := s.post(t, "/bonnie/v1/runs/"+reserved+"/respond", RespondRequest{
				Responses: []runtime.InputResponse{{Text: "no"}},
			})
			return r
		}},
	} {
		if got := tc.do().StatusCode; got != http.StatusNotFound {
			t.Errorf("%s on a reserved run = %d, want 404", tc.name, got)
		}
	}

	// The bookkeeping must be untouched: the address still resolves to the
	// run it was bound to.
	resp, addr := s.get(t, "/bonnie/v1/addresses/slack%2FC1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("address lookup = %d, want 200", resp.StatusCode)
	}
	if addr.RunID == "" || runtime.IsReservedRun(addr.RunID) {
		t.Fatalf("address resolved to %q", addr.RunID)
	}
}
