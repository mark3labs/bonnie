package http

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// HTTP cancellation is a durable request, and an observed identity protects
// a newer parked approval from delayed controls and answers.
func TestScopedCancelParkedHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{FinalValue: runtime.SuspendRequest{Kind: runtime.SuspendApproval, Prompt: "delete?"}}}}, WithIDGenerator(func() string { return "parked" }))
	_, run := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "work"})
	post := func(body string) (*http.Response, runtime.CancelResult) {
		resp, err := http.Post(s.URL+"/bonnie/v1/runs/parked/cancel", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out runtime.CancelResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return resp, out
	}
	if run.TurnID == "" || run.State != runtime.RunWaiting {
		t.Fatalf("parked: %+v", run)
	}
	resp, result := post(`{"turn_id":"old"}`)
	if resp.StatusCode != http.StatusOK || result.Status != runtime.CancelStale {
		t.Fatalf("stale: %d %+v", resp.StatusCode, result)
	}
	resp, result = post(`{"turn_id":"` + run.TurnID + `"}`)
	if resp.StatusCode != http.StatusAccepted || result.Status != runtime.CancelRequested {
		t.Fatalf("cancel: %d %+v", resp.StatusCode, result)
	}
	resp, _ = s.post(t, "/bonnie/v1/runs/unknown/cancel", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown: %d", resp.StatusCode)
	}
}
