package bonnie

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// squatter is a channel that wants a route inside the framework's namespace.
type squatter struct{ path string }

func (s squatter) Name() string { return "squatter" }
func (s squatter) Routes() []channel.Route {
	return []channel.Route{{Method: http.MethodGet, Path: s.path, Handler: func(http.ResponseWriter, *http.Request, channel.Inbound, channel.Outbound) {}}}
}
func (squatter) From(string) channel.SessionRef   { return nil }
func (squatter) Attach(string) channel.SessionRef { return nil }

// serveForTest runs an agent on a free port and returns its base URL and
// the directory its journal is written to. The agent stops when the test
// ends.
func serveForTest(t *testing.T, opts ...Option) (string, string) {
	t.Helper()
	journalDir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- New(append([]Option{
			WithAgentFactory(stubFactory),
			WithJournal(journalDir),
			WithInstructions(""),
			WithWorkspace(""),
			WithListener(ln),
			Quiet(),
		}, opts...)...).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not stop")
		}
	})
	return "http://" + ln.Addr().String(), journalDir
}

// A channel that mounts under /bonnie/ is refused before the listener opens,
// with an error that names the channel and the path. The mux would panic on
// an exact duplicate and silently accept a near miss; neither tells an
// operator what to fix.
func TestReservedNamespaceIsRefused(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	agent := New(
		WithAgentFactory(stubFactory),
		WithJournal(t.TempDir()),
		WithInstructions(""),
		WithWorkspace(""),
		WithListener(ln),
		Quiet(),
		WithChannel(func(*runtime.Runner) (Channel, error) { return squatter{path: "/bonnie/x"}, nil }),
	)
	err = agent.Run(context.Background())
	if !errors.Is(err, channel.ErrReservedPath) {
		t.Fatalf("Run = %v, want channel.ErrReservedPath", err)
	}
	for _, want := range []string{"squatter", "/bonnie/x"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
}

// Health answers before any run exists; info names every mounted channel.
func TestHealthAndInfoServeUnderThePrefix(t *testing.T) {
	t.Parallel()
	base, _ := serveForTest(t,
		WithName("test-agent"),
		WithChannel(func(*runtime.Runner) (Channel, error) { return squatter{path: "/elsewhere"}, nil }),
	)

	var health struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	getJSON(t, base+"/bonnie/v1/health", &health)
	if !health.OK || health.Status != "ready" {
		t.Fatalf("health = %+v, want ok/ready", health)
	}

	var info struct {
		Agent    string   `json:"agent"`
		Version  string   `json:"version"`
		Channels []string `json:"channels"`
	}
	getJSON(t, base+"/bonnie/v1/info", &info)
	if info.Agent != "test-agent" || info.Version == "" {
		t.Fatalf("info = %+v, want the agent name and a version", info)
	}
	if strings.Join(info.Channels, ",") != "http,squatter" {
		t.Fatalf("channels = %v, want [http squatter]", info.Channels)
	}

	// Nothing answers at the root any more.
	resp, err := http.Get(base + "/runs/nope")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /runs/nope = %d, want 404: the unprefixed routes are gone", resp.StatusCode)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d", url, resp.StatusCode)
			}
			if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
				t.Fatalf("decode %s: %v", url, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
