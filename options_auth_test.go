package bonnie

import (
	"io"
	"net/http"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
)

// An authenticator passed to New must reach the framework's own HTTP
// channel. The option existed on channel/http before it existed here, and a
// tree-based agent — the way most hosts run BONNIE — could not reach it at
// all: Serve built the channel with a fixed option list. A security control
// a host cannot switch on is not a control.
func TestHTTPAuthenticatorReachesTheChannel(t *testing.T) {
	t.Parallel()

	var calls int
	base, _ := serveForTest(t,
		WithAgentFactory(stubFactory),
		WithHTTPAuthenticator(func(r *http.Request) (*channel.Principal, error) {
			calls++
			if r.Header.Get("X-Token") != "good" {
				return nil, bonniehttp.ErrUnauthenticated
			}
			return &channel.Principal{Authenticator: "test", Kind: "user", ID: "alice"}, nil
		}),
	)

	// Health stays public: a deployment probe must not need a credential.
	resp, err := http.Get(base + "/bonnie/v1/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d, want 200 without a credential", resp.StatusCode)
	}

	// Everything else is verified.
	infoResp, err := http.Get(base + "/bonnie/v1/info")
	if err != nil {
		t.Fatalf("GET info: %v", err)
	}
	body, _ := io.ReadAll(infoResp.Body)
	_ = infoResp.Body.Close()
	if infoResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("info without a credential = %d (%s), want 401", infoResp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/bonnie/v1/info", nil)
	req.Header.Set("X-Token", "good")
	okResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET info with a credential: %v", err)
	}
	_ = okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("info with a credential = %d, want 200", okResp.StatusCode)
	}

	if calls == 0 {
		t.Fatal("the authenticator never ran: the option did not reach the channel")
	}
}

// Without the option the HTTP API authenticates nobody, which is what a
// loopback `bonnie dev` needs. The default is stated in a test so a change
// to it cannot pass unnoticed.
func TestNoAuthenticatorLeavesTheAPIOpen(t *testing.T) {
	t.Parallel()
	base, _ := serveForTest(t, WithAgentFactory(stubFactory))

	resp, err := http.Get(base + "/bonnie/v1/info")
	if err != nil {
		t.Fatalf("GET info: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info = %d, want 200 with no authenticator configured", resp.StatusCode)
	}
}
