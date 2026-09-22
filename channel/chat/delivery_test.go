package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// deliveryRecorder is a fake platform API: it records what arrived and answers
// whatever the test set.
type deliveryRecorder struct {
	mu      sync.Mutex
	server  *httptest.Server
	bodies  []string
	headers []http.Header
	status  int
	answer  string
	// closed counts how many request bodies the server saw to completion,
	// which is one per delivery that actually reached it.
	closed int
}

func newDeliveryRecorder(t *testing.T, status int, answer string) *deliveryRecorder {
	t.Helper()
	r := &deliveryRecorder{status: status, answer: answer}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, string(body))
		r.headers = append(r.headers, req.Header.Clone())
		r.closed++
		status, answer := r.status, r.answer
		r.mu.Unlock()

		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *deliveryRecorder) seen() ([]string, []http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...), append([]http.Header(nil), r.headers...)
}

// The happy path: the payload is marshalled, the content type is set by the
// helper, the caller's headers are added, and the answer comes back whole
// for an adapter that needs to read it.
func TestPostJSONSendsAndReturnsTheAnswer(t *testing.T) {
	t.Parallel()
	rec := newDeliveryRecorder(t, http.StatusOK, `{"ok":true,"ts":"1.2"}`)
	d := Delivery{Client: rec.server.Client(), Prefix: "test"}

	answer, ok := d.PostJSON(context.Background(), rec.server.URL,
		BearerHeader("Bearer", "xoxb-token"),
		map[string]any{"channel": "C1", "text": "hello"})
	if !ok {
		t.Fatal("PostJSON reported a failure for a 200")
	}

	var out struct {
		OK bool   `json:"ok"`
		TS string `json:"ts"`
	}
	if err := json.Unmarshal(answer, &out); err != nil {
		t.Fatalf("the answer did not come back whole: %v", err)
	}
	if !out.OK || out.TS != "1.2" {
		t.Fatalf("answer = %+v", out)
	}

	bodies, headers := rec.seen()
	if len(bodies) != 1 {
		t.Fatalf("the platform saw %d requests, want 1", len(bodies))
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("the body was not JSON: %v", err)
	}
	if sent["text"] != "hello" {
		t.Fatalf("body = %v", sent)
	}
	if got := headers[0].Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json: the helper sets it so no adapter forgets", got)
	}
	if got := headers[0].Get("Authorization"); got != "Bearer xoxb-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

// A platform that answers 201 or 204 has accepted the message. Reporting
// that as a failure would log a delivery that plainly happened, which is
// what a bare `!= http.StatusOK` did in four adapters.
func TestPostJSONAcceptsEvery2xx(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		rec := newDeliveryRecorder(t, status, "")
		d := Delivery{Client: rec.server.Client(), Prefix: "test"}
		if _, ok := d.PostJSON(context.Background(), rec.server.URL, nil, map[string]string{"a": "b"}); !ok {
			t.Errorf("status %d reported as a failure", status)
		}
	}
}

// A refusal is reported as one, and the body still comes back: that is
// where a platform puts the reason, and an adapter that can say more than
// "it failed" should be able to.
func TestPostJSONReportsARefusalAndKeepsTheBody(t *testing.T) {
	t.Parallel()
	rec := newDeliveryRecorder(t, http.StatusForbidden, `{"message":"Resource not accessible"}`)
	d := Delivery{Client: rec.server.Client(), Prefix: "test"}

	answer, ok := d.PostJSON(context.Background(), rec.server.URL, nil, map[string]string{"body": "hi"})
	if ok {
		t.Fatal("a 403 was reported as a delivery")
	}
	if !strings.Contains(string(answer), "not accessible") {
		t.Fatalf("the refusal body was dropped: %q", answer)
	}
}

// A transport that never reaches the platform is a failure, not a panic and
// not a hang. Fire-and-log means the caller carries on.
func TestPostJSONReportsATransportFailure(t *testing.T) {
	t.Parallel()
	rec := newDeliveryRecorder(t, http.StatusOK, "")
	url := rec.server.URL
	rec.server.Close() // nothing is listening now

	d := Delivery{Client: rec.server.Client(), Prefix: "test"}
	if _, ok := d.PostJSON(context.Background(), url, nil, map[string]string{"a": "b"}); ok {
		t.Fatal("a dead endpoint was reported as a delivery")
	}
}

// A payload that cannot be marshalled fails before anything is sent. It
// must not reach the platform as a half-encoded body.
func TestPostJSONRefusesAnUnmarshallablePayload(t *testing.T) {
	t.Parallel()
	rec := newDeliveryRecorder(t, http.StatusOK, "")
	d := Delivery{Client: rec.server.Client(), Prefix: "test"}

	if _, ok := d.PostJSON(context.Background(), rec.server.URL, nil, make(chan int)); ok {
		t.Fatal("an unmarshallable payload was reported as a delivery")
	}
	if bodies, _ := rec.seen(); len(bodies) != 0 {
		t.Fatalf("the platform saw %d requests for a payload that never encoded", len(bodies))
	}
}

// A cancelled context stops the delivery rather than blocking the caller.
func TestPostJSONHonoursContext(t *testing.T) {
	t.Parallel()
	rec := newDeliveryRecorder(t, http.StatusOK, "")
	d := Delivery{Client: rec.server.Client(), Prefix: "test"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := d.PostJSON(ctx, rec.server.URL, nil, map[string]string{"a": "b"}); ok {
		t.Fatal("a cancelled context still delivered")
	}
}

// BearerHeader spells the scheme the caller chose: Slack and GitHub want
// "Bearer", Discord wants "Bot", and getting it wrong is a 401 the adapter
// would log as an opaque refusal.
func TestBearerHeaderUsesTheGivenScheme(t *testing.T) {
	t.Parallel()
	if got := BearerHeader("Bot", "t0ken").Get("Authorization"); got != "Bot t0ken" {
		t.Fatalf("Authorization = %q, want %q", got, "Bot t0ken")
	}
	if got := BearerHeader("Bearer", "t0ken").Get("Authorization"); got != "Bearer t0ken" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer t0ken")
	}
}
