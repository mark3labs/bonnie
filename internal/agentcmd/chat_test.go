package agentcmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/bonnie/internal/tui"
)

func TestChatURL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		":9090":          "http://127.0.0.1:9090",
		"localhost:9000": "http://localhost:9000",
		"http://x:1":     "http://x:1",
		"https://x:1":    "https://x:1",
		"http://":        "http://",
		"https://":       "https://",
		"":               "http://127.0.0.1:8080",
		":0":             "http://127.0.0.1:0",
	} {
		if got := ChatURL(in); got != want {
			t.Errorf("ChatURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChatCommandFlags(t *testing.T) {
	t.Parallel()
	cmd := NewChatCommand(":9090")
	for name, want := range map[string]string{"addr": ":9090", "run": "tui-default", "token": ""} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil || flag.DefValue != want {
			t.Errorf("flag %s = %+v, want default %q", name, flag, want)
		}
	}
	if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
		t.Fatal("chat accepted a positional argument")
	}
}

// The command passes its context, conversation address, and bearer token to
// the terminal client without opening a terminal or starting a server.
func TestChatCommandClient(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", "test-token"} {
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := ""
				if token != "" {
					want = "Bearer " + token
				}
				if got := r.Header.Get("Authorization"); got != want {
					t.Errorf("Authorization = %q, want %q", got, want)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"run_id":"test-run","cursor":0}`)
			}))
			defer server.Close()

			type contextKey struct{}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "value"))
			defer cancel()
			wantErr := errors.New("terminal stopped")
			called := false
			cmd := newChatCommand("unused:1", func(gotCtx context.Context, c tui.Client, address string) error {
				called = true
				if address != "conversation" || gotCtx.Value(contextKey{}) != "value" {
					t.Fatalf("address or context not passed: %q", address)
				}
				if _, _, err := c.Ensure(gotCtx, address); err != nil {
					t.Fatalf("Ensure: %v", err)
				}
				cancel()
				<-gotCtx.Done()
				return wantErr
			})
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs([]string{"--addr", server.URL, "--run", "conversation", "--token", token})
			if err := cmd.ExecuteContext(ctx); !errors.Is(err, wantErr) {
				t.Fatalf("ExecuteContext = %v, want %v", err, wantErr)
			}
			if !called {
				t.Fatal("terminal runner not called")
			}
		})
	}
}
