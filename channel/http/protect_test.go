package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// Protect uses exactly the API verifier semantics and carries the verified
// principal into the handler. An anonymous verifier result is also valid.
func TestProtect(t *testing.T) {
	t.Parallel()
	principal := &channel.Principal{ID: "operator"}
	for _, tc := range []struct {
		name      string
		auth      Authenticator
		status    int
		principal *channel.Principal
		verified  bool
	}{
		{name: "open", status: 200},
		{name: "verified", auth: func(*http.Request) (*channel.Principal, error) { return principal, nil }, status: 200, principal: principal, verified: true},
		{name: "anonymous", auth: func(*http.Request) (*channel.Principal, error) { return nil, nil }, status: 200, verified: true},
		{name: "denied", auth: func(*http.Request) (*channel.Principal, error) { return nil, ErrUnauthenticated }, status: 401},
		{name: "fault", auth: func(*http.Request) (*channel.Principal, error) { return nil, errors.New("fault") }, status: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := runtime.NewMemoryJournal()
			defer func() {
				if err := j.Close(); err != nil {
					t.Error(err)
				}
			}()
			c := New(runtime.NewRunner(j, nil), WithAuthenticator(tc.auth))
			called := false
			h := c.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				p, verified := c.verifiedPrincipal(r)
				if p != tc.principal || verified != tc.verified {
					t.Errorf("principal = %v, verified = %v", p, verified)
				}
				w.WriteHeader(200)
			}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/web/", nil))
			if w.Code != tc.status || called != (tc.status == 200) {
				t.Fatalf("status %d, called %v", w.Code, called)
			}
		})
	}
}
