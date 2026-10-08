package bonnie

import (
	"bufio"
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The option defaults to off, and an explicit false must disable it.
func TestWithWebUI(t *testing.T) {
	t.Parallel()
	if resolve().webUI || !resolve(WithWebUI(true)).webUI || resolve(WithWebUI(true), WithWebUI(false)).webUI {
		t.Fatal("incorrect web option resolution")
	}
}

// An absent flag preserves the option; explicit flags override it.
func TestServeWebFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		option bool
		args   []string
		want   bool
	}{
		{"default", false, nil, false},
		{"option", true, nil, true},
		{"enable", false, []string{"--web"}, true},
		{"disable", true, []string{"--web=false"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Stop at validation, before opening any runtime resources.
			a := New(WithWebUI(tc.option), WithAgentFactory(stubFactory), WithModel("conflict"), WithContextFiles(""))
			cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
			cmd.SetArgs(tc.args)
			if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "WithAgentFactory") {
				t.Fatalf("validation = %v", err)
			}
			if a.cfg.webUI != tc.want {
				t.Fatalf("web = %v, want %v", a.cfg.webUI, tc.want)
			}
		})
	}
}

// Both web mount paths reach the handler, without removing the HTTP API.
func TestWebUIMount(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			t.Parallel()
			base, _ := serveForTest(t, WithWebUI(enabled))
			var health struct {
				OK bool `json:"ok"`
			}
			getJSON(t, base+"/bonnie/v1/health", &health)
			if !health.OK {
				t.Fatal("HTTP channel is not healthy")
			}
			for _, path := range []string{"/web", "/web/"} {
				resp, err := http.Get(base + path)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				want := http.StatusNotFound
				if enabled {
					want = http.StatusOK
				}
				if resp.StatusCode != want {
					t.Fatalf("GET %s = %d, want %d", path, resp.StatusCode, want)
				}
			}
		})
	}
}

// A real web live connection must end with clean EOF when the root server
// stops. It must not hold shutdown open until the default 30-second timeout.
func TestWebLiveCleanShutdown(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- New(
			WithAgentFactory(stubFactory), WithJournal(t.TempDir()),
			WithInstructions(""), WithContextFiles(""), WithListener(ln),
			WithWebUI(true), WithShutdownTimeout(5*time.Second), Quiet(),
		).Run(ctx)
	}()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if !stopped {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("agent did not stop")
			}
		}
	})
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	base := "http://" + ln.Addr().String()
	page, err := client.Get(base + "/web/")
	if err != nil {
		t.Fatal(err)
	}
	cookies := page.Cookies()
	_ = page.Body.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/web/live?view=runs", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("live response = %s, %q", response.Status, response.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "event: datastar-patch-elements\n" {
		t.Fatalf("initial live event = %q, %v", line, err)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		ended <- err
	}()
	cancel()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("open web live connection blocked shutdown")
	}
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("live connection did not end with clean EOF: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("live connection did not end")
	}
}
