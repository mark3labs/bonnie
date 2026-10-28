package natsapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestAllRoutesReuseHTTPResponses(t *testing.T) {
	a, b := broker(t, 0)
	c := bridge(t, a, "routes", Limits{})
	tr := transport(t, b, "routes")
	for _, route := range c.api.Routes() {
		path := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(route.Path, "{id}", "missing"), "{address}", "unbound"), "{submission}", "missing")
		t.Run(route.Method+" "+path, func(t *testing.T) {
			payload := "{}"
			if route.Method == "GET" {
				payload = ""
			}
			direct := httptest.NewRequest(route.Method, path, strings.NewReader(payload))
			direct.Header.Set("Authorization", "Bearer operator")
			expected := httptest.NewRecorder()
			c.handler.ServeHTTP(expected, direct)
			req, _ := http.NewRequestWithContext(t.Context(), route.Method, "http://logical"+path, strings.NewReader(payload))
			req.Header.Set("Authorization", "Bearer operator")
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != expected.Code {
				t.Fatalf("NATS %d HTTP %d", resp.StatusCode, expected.Code)
			}
			// Generated IDs differ for creation; all error and read bodies match.
			if expected.Code >= 400 || route.Method == "GET" {
				if string(body) != expected.Body.String() {
					t.Fatalf("body parity: %s != %s", body, expected.Body.String())
				}
			}
		})
	}
}
func TestExplicitSuspensionAndStaleAnswer(t *testing.T) {
	a, b := broker(t, 0)
	model := fakemodel.New(fakemodel.Call("ask_human", `{"question":"Which region?"}`), fakemodel.Say("done"))
	r := runtime.NewRunner(runtime.NewMemoryJournal(), runtime.KitAgent(model.Option(), func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}, kit.WithTools(runtime.AskTool())))
	c, err := New(r, Config{Conn: a, Subject: "suspend", Authenticator: auth})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Shutdown(context.Background()) }()
	cli := client.New("http://logical", client.WithHTTPClient(&http.Client{Transport: transport(t, b, "suspend")}), client.WithBearerToken("operator"))
	// The first real Kit in a process costs about two seconds under -race, and
	// more when the full suite loads the machine. A 3s budget failed Start with
	// "context deadline exceeded" in go test ./...; this bound only stops a hang.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	run, err := cli.Start(ctx, "question", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != runtime.RunWaiting || run.Suspend == nil {
		t.Fatal(run)
	}
	if _, err = cli.Send(ctx, run.ID, "not approval"); err == nil {
		t.Fatal("chat resumed suspended run")
	}
	if _, err = cli.RespondWith(ctx, run.ID, []runtime.InputResponse{{TurnID: "stale", Text: "wrong"}}); err == nil {
		t.Fatal("stale answer accepted")
	}
	done, err := cli.RespondWith(ctx, run.ID, []runtime.InputResponse{{TurnID: run.TurnID, Text: "eu-west"}})
	if err != nil {
		t.Fatal(err)
	}
	if done.ID != run.ID || done.State != runtime.RunCompleted {
		t.Fatal(done)
	}
}

type receiver struct{}

func (r *receiver) Receive(context.Context, any, string, channel.SendOptions) error {
	return nil
}

type registry struct{ r *receiver }

func (r registry) To(name string) (channel.Receiver, bool) { return r.r, name == "destination" }
func TestOutboundAndHostInfo(t *testing.T) {
	a, b := broker(t, 0)
	dest := &receiver{}
	c, err := New(runner(), Config{Conn: a, Subject: "host", Authenticator: auth})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.PrepareHost(registry{dest}, channel.HostInfo{Agent: "owner", Version: "test", Channels: []string{"nats-api", "destination"}}); err != nil {
		t.Fatal(err)
	}
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Shutdown(context.Background()) }()
	tr := transport(t, b, "host")
	cli := client.New("http://logical", client.WithHTTPClient(&http.Client{Transport: tr}), client.WithBearerToken("operator"))
	info, err := cli.Info(t.Context())
	if err != nil || info.Agent != "owner" {
		t.Fatal(info, err)
	}
	// Existing HTTP operations currently ignore outbound; ensure the host seam
	// accepts the registry without inventing a new handoff operation.
	if c.handler == nil {
		t.Fatal("missing prepared handler")
	}
}
