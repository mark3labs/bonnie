package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mark3labs/bonnie/runtime"
)

// Durable routes return admission, not execution, and never create a run for
// an unknown ID. Request retries find the same saved submission.
func TestDurableSubmissionRoutes(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})
	if _, err := s.runner.Start(context.Background(), "r", runtime.Input{Text: "initial"}); err != nil {
		t.Fatal(err)
	}
	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := s.Client().Post(s.URL+path, "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
		})
		return resp
	}
	path := "/bonnie/v1/runs/r/submissions"
	var first runtime.Submission
	resp := post(path, `{"request_id":"one","text":"next"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	resp = post(path, `{"request_id":"one","text":"duplicate"}`)
	var again runtime.Submission
	if err := json.NewDecoder(resp.Body).Decode(&again); err != nil {
		t.Fatal(err)
	}
	if first.ID != again.ID {
		t.Fatal("duplicate admission")
	}
	resp = post("/bonnie/v1/runs/unknown/submissions", `{"text":"no"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown status=%d", resp.StatusCode)
	}
	req, err := http.NewRequest(http.MethodDelete, s.URL+path+"/"+first.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete=%d", resp.StatusCode)
	}
	items, err := s.runner.Submissions(context.Background(), "r")
	if err != nil || len(items) != 1 || items[0].State != runtime.SubmissionAborted {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	resp = post("/bonnie/v1/runs/r/children", `{"key":"child","text":"review"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("child=%d", resp.StatusCode)
	}
	children, err := s.runner.Children(context.Background(), "r")
	if err != nil || len(children) != 1 {
		t.Fatalf("children=%v err=%v", children, err)
	}
}
