package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// seqID returns a run-ID generator that hands out "op-run-1", "op-run-2",
// and so on, so two different addresses cannot share a generated ID.
func seqID() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("op-run-%d", n)
	}
}

// principal returns an authenticated caller, which an idempotent start
// requires.
func principal(id string) *channel.Principal {
	return &channel.Principal{Authenticator: "test", Kind: "user", ID: id}
}

// Two starts with the same operation ID and principal return the same run,
// and the agent ran once. The same operation ID under another principal is
// a different run: the key is namespaced by the principal that owns it.
func TestOperationIDIsCreateOnce(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "once"}}},
		WithIDGenerator(seqID()))

	req := StartRequest{Text: "hi", OperationID: "order-4213", Auth: principal("alice")}
	firstResp, first := s.post(t, "/bonnie/v1/runs", req)
	secondResp, second := s.post(t, "/bonnie/v1/runs", req)
	if firstResp.StatusCode != http.StatusOK || secondResp.StatusCode != http.StatusOK {
		t.Fatalf("starts returned %d and %d, want 200", firstResp.StatusCode, secondResp.StatusCode)
	}
	if first.RunID != second.RunID || first.RunID == "" {
		t.Fatalf("retried start = %q then %q, want one run", first.RunID, second.RunID)
	}

	_, other := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi", OperationID: "order-4213", Auth: principal("bob")})
	if other.RunID == first.RunID {
		t.Fatal("another principal's operation ID resolved to alice's run")
	}
	// And without a principal it is refused: an idempotency key with no
	// owner is a way to read someone else's run.
	badResp, _ := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi", OperationID: "order-4213"})
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("anonymous operation_id = %d, want 400", badResp.StatusCode)
	}
}

// postRaw and getRaw are post and get without the body close: they return
// the status and the raw body, so an error case can read its code.
func (s *testServer) postRaw(t *testing.T, path string, body any) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	} else {
		buf.WriteString("{}")
	}
	resp, err := http.Post(s.URL+path, "application/json", &buf)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, string(b)
}

func (s *testServer) getRaw(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, string(b)
}

// Every non-2xx reply carries a stable code, whatever the status.
func TestEveryErrorCarriesAStableCode(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hi"}}},
		WithIDGenerator(func() string { return "with-cursor" }))

	cases := []struct {
		name   string
		do     func() (int, string)
		status int
		code   string
	}{
		{"unknown run", func() (int, string) { return s.postRaw(t, "/bonnie/v1/runs/no-such-run", SendRequest{Text: "hi"}) }, http.StatusNotFound, errNotFound},
		{"unknown run on get", func() (int, string) { return s.getRaw(t, "/bonnie/v1/runs/no-such-run") }, http.StatusNotFound, errNotFound},
		{"unbound address", func() (int, string) { return s.getRaw(t, "/bonnie/v1/addresses/never-bound") }, http.StatusNotFound, errNotFound},
		{"malformed body", func() (int, string) {
			resp, err := http.Post(s.URL+"/bonnie/v1/runs", "application/json", strings.NewReader("{not json"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			return resp.StatusCode, string(b)
		}, http.StatusBadRequest, errBadRequest},
		{"bad cursor", func() (int, string) {
			s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi"})
			return s.getRaw(t, "/bonnie/v1/runs/with-cursor/stream?cursor=abc")
		}, http.StatusBadRequest, errBadRequest},
		{"unknown policy", func() (int, string) {
			return s.postRaw(t, "/bonnie/v1/runs", StartRequest{Text: "hi", TurnPolicy: "nope"})
		}, http.StatusBadRequest, errUnknownPolicy},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := tc.do()
			if status != tc.status {
				t.Fatalf("status = %d, want %d (%s)", status, tc.status, body)
			}
			var got ErrorResponse
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("decode %q: %v", body, err)
			}
			if got.Code != tc.code {
				t.Fatalf("code = %q, want %q (error: %s)", got.Code, tc.code, got.Error)
			}
			if got.Error == "" {
				t.Fatal("the human-readable error is empty")
			}
		})
	}
}

// A 404 body of an unbound address goes through writeError's mapping and
// must carry the same code an unknown run does.
func TestAddressLookupCode(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})
	status, body := s.getRaw(t, "/bonnie/v1/addresses/never-bound")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
	var got ErrorResponse
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Code != errNotFound {
		t.Fatalf("code = %q, want %q", got.Code, errNotFound)
	}
}
