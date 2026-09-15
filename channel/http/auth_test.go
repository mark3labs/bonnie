package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// headerAuth is a deployment's verifier in miniature: it reads a credential
// off the request and MINTS the principal from it. A real one checks a
// bearer token's signature or an OIDC assertion; what matters for the
// contract is that the identity comes from the transport, not from the body
// the caller wrote.
func headerAuth(r *http.Request) (*channel.Principal, error) {
	id := r.Header.Get("X-Test-User")
	if id == "" {
		return nil, ErrUnauthenticated
	}
	return &channel.Principal{Authenticator: "test-header", Kind: "user", ID: id}, nil
}

// postAs posts with a credential the authenticator understands.
func (s *testServer) postAs(t *testing.T, user, path string, body any) (*http.Response, RunResponse) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	} else {
		buf.WriteString("{}")
	}
	req, err := http.NewRequest(http.MethodPost, s.URL+path, &buf)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out RunResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// A request that carries no credential the verifier accepts is refused
// before it reaches a handler, so an unauthenticated caller cannot start a
// run, read one, or open a stream.
func TestAuthenticatorRefusesUnauthenticated(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hi"}}},
		WithAuthenticator(headerAuth))

	resp, _ := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous start = %d, want 401", resp.StatusCode)
	}
	// The refusal carries the stable code, so a client can act on it
	// without parsing prose.
	status, body := s.postRaw(t, "/bonnie/v1/runs", StartRequest{Text: "hi"})
	if status != http.StatusUnauthorized || !strings.Contains(body, `"code":"unauthenticated"`) {
		t.Fatalf("refusal = %d %s, want 401 with the unauthenticated code", status, body)
	}
	// No run was created by the refused request.
	if ids, _ := s.runner.Journal().Runs(t.Context(), ""); len(ids) != 0 {
		t.Fatalf("journal holds %v, want nothing: a refused request must not create a run", ids)
	}
}

// Health answers without a credential. A deployment probe must not need one,
// and the route reports that the process is up and nothing about any run.
func TestHealthStaysPublicUnderAnAuthenticator(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{}, WithAuthenticator(headerAuth))

	resp, err := http.Get(s.URL + "/bonnie/v1/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d, want 200 without a credential", resp.StatusCode)
	}

	// Info is not public: it names the agent and the mounted channels.
	infoResp, err := http.Get(s.URL + "/bonnie/v1/info")
	if err != nil {
		t.Fatalf("GET info: %v", err)
	}
	defer func() { _ = infoResp.Body.Close() }()
	if infoResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("info = %d, want 401", infoResp.StatusCode)
	}
}

// The verified principal wins over the body's, and the body's is dropped
// rather than merged. A caller that can reach the API must not be able to
// relabel itself by writing an auth field.
func TestAuthenticatorOverridesTheBodyPrincipal(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hi"}}},
		WithAuthenticator(headerAuth))

	resp, run := s.postAs(t, "alice", "/bonnie/v1/runs", StartRequest{
		Text: "hi",
		Auth: &channel.Principal{Authenticator: "forged", Kind: "user", ID: "root"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start = %d, want 200", resp.StatusCode)
	}

	// The journal records who the verifier proved, not who the body claimed.
	recs, err := s.runner.Journal().Replay(t.Context(), chat.AddressRun)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var found bool
	for _, rec := range recs {
		if !strings.Contains(rec.Text, run.RunID) && !strings.Contains(string(rec.Payload), run.RunID) {
			continue
		}
		payload := string(rec.Payload)
		if strings.Contains(payload, "forged") || strings.Contains(payload, `"root"`) {
			t.Fatalf("the forged principal reached the journal: %s", payload)
		}
		if strings.Contains(payload, "alice") {
			found = true
		}
	}
	if !found {
		t.Fatal("the verified principal never reached the journal")
	}
}

// A verifier that breaks is not proof the caller is an impostor: the answer
// is 500, not 401, so a legitimate client is not sent off to re-authenticate
// against a fault it cannot fix. The detail stays on the server.
func TestBrokenAuthenticatorIsNotA401(t *testing.T) {
	t.Parallel()
	boom := errors.New("the token service is unreachable at /etc/secrets/key")
	s := newTestServer(t, &stubAgent{}, WithAuthenticator(
		func(*http.Request) (*channel.Principal, error) { return nil, boom }))

	status, body := s.postRaw(t, "/bonnie/v1/runs", StartRequest{Text: "hi"})
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a broken verifier", status)
	}
	if strings.Contains(body, "/etc/secrets") {
		t.Fatalf("the verifier's error text reached the client: %s", body)
	}
}

// An authenticator may admit an anonymous caller by returning no principal
// and no error. The request proceeds unattributed, which is what an open
// read-only deployment wants — but it still cannot claim an idempotency
// namespace, because there is no owner to namespace it by.
func TestAnonymousVerifiedCallerCannotClaimAnOperationID(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hi"}}},
		WithAuthenticator(func(*http.Request) (*channel.Principal, error) { return nil, nil }))

	if resp, _ := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous start = %d, want 200: this verifier admits anonymous callers", resp.StatusCode)
	}
	resp, _ := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi", OperationID: "order-1"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("anonymous operation_id = %d, want 400", resp.StatusCode)
	}
}

// Without an authenticator the channel proves no identity, so it refuses the
// one route whose correctness depends on identity. This is the defect the
// authenticator exists to fix: the idempotency key is namespaced by the
// caller, and a self-asserted caller can claim any namespace.
func TestOperationIDNeedsAVerifiedPrincipal(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hi"}}})

	status, body := s.postRaw(t, "/bonnie/v1/runs", StartRequest{
		Text:        "hi",
		OperationID: "order-4213",
		Auth:        &channel.Principal{Authenticator: "self", Kind: "user", ID: "alice"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without an authenticator", status)
	}
	if !strings.Contains(body, "authenticator") {
		t.Fatalf("the refusal must name what is missing: %s", body)
	}

	// A plain start still works: only the idempotent one needs proof.
	if resp, _ := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("plain start = %d, want 200", resp.StatusCode)
	}
}

// An unmapped fault answers with the stable code and nothing else. The text
// of a journal or driver error has carried file paths and SQL to whoever
// could reach the API.
func TestInternalErrorsAreOpaque(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{err: errors.New("dial tcp 10.0.0.5:5432: /var/lib/bonnie/journal.db is locked")})

	status, body := s.postRaw(t, "/bonnie/v1/runs", StartRequest{Text: "hi"})
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if strings.Contains(body, "/var/lib") || strings.Contains(body, "10.0.0.5") {
		t.Fatalf("the internal error text reached the client: %s", body)
	}
	if !strings.Contains(body, `"code":"internal"`) {
		t.Fatalf("body = %s, want the stable internal code", body)
	}
}

// A mapped sentinel keeps its own wording: "run not found" is what the
// caller needs in order to act, and clients have always read it.
func TestMappedErrorsKeepTheirText(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})

	status, body := s.postRaw(t, "/bonnie/v1/runs/no-such-run", SendRequest{Text: "hi"})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if !strings.Contains(body, `"code":"run_not_found"`) {
		t.Fatalf("body = %s, want the stable not-found code", body)
	}
	if !strings.Contains(body, runtime.ErrRunNotFound.Error()) {
		t.Fatalf("body = %s, want the sentinel's own text", body)
	}
}
