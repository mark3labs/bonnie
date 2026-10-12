package iroh_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	goiroh "github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/key"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	irohchannel "github.com/mark3labs/bonnie/channel/iroh"
	"github.com/mark3labs/bonnie/channeltest"
	irohclient "github.com/mark3labs/bonnie/client/iroh"
	"github.com/mark3labs/bonnie/runtime"
)

func TestConformance(t *testing.T) {
	t.Parallel()
	channeltest.RunConformance(t, func(t *testing.T) *channeltest.Fixture {
		journal := runtime.NewMemoryJournal()
		agent := channeltest.NewScriptAgent()
		c, err := irohchannel.New(runtime.NewRunner(journal, agent.Factory()), irohchannel.Config{})
		if err != nil {
			t.Fatal(err)
		}
		return &channeltest.Fixture{Inbound: c, Agent: agent, Journal: journal}
	})
}

func loopbackOptions() []goiroh.Option {
	return []goiroh.Option{goiroh.WithBindAddr(netip.MustParseAddrPort("127.0.0.1:0")), goiroh.WithoutRelayTransports(), goiroh.WithoutNetReport()}
}

func TestWirePairing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ep, err := goiroh.Bind(ctx, loopbackOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ep.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	journal := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	runner := runtime.NewRunner(journal, agent.Factory())
	c, err := irohchannel.New(runner, irohchannel.Config{EndpointOptions: loopbackOptions()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "iroh" || len(c.Routes()) != 0 {
		t.Fatal("unexpected host surface")
	}
	if _, err := c.Ticket(); err == nil {
		t.Fatal("ticket before startup")
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := c.Start(ctx); err == nil {
		t.Fatal("second start succeeded")
	}
	ticket, err := c.Ticket()
	if err != nil {
		t.Fatal(err)
	}
	hc, err := irohclient.NewHTTPClient(ep, ticket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hc.CloseIdleConnections)

	request := func(method, path, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://iroh/bonnie/v1"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := res.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s: status %d want %d: %s", path, res.StatusCode, want, data)
		}
		return data
	}
	request("GET", "/health", "", 401)
	request("POST", "/runs", `{"text":"no access"}`, 401)
	if err := c.Pair(ep.ID()); err != nil {
		t.Fatal(err)
	}
	if err := c.Pair(ep.ID()); err != nil {
		t.Fatal(err)
	}
	request("GET", "/health", "", 200)
	data := request("POST", "/runs", `{"address":"test","text":"hello","auth":{"ID":"forged"}}`, 200)
	var result struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" {
		t.Fatalf("no run ID: %s", data)
	}
	id, err := c.From("test").RunID(ctx)
	if err != nil || id != result.RunID {
		t.Fatalf("address: %s, %v", id, err)
	}
	httpID, err := bonniehttp.New(runner).From("test").RunID(ctx)
	if err != nil || httpID == id {
		t.Fatalf("Iroh address leaked to HTTP: %s, %v", httpID, err)
	}
	records, err := journal.Replay(ctx, chat.AddressRun)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		if record.ExtType == chat.PrincipalExtType {
			payload := string(record.Payload)
			if strings.Contains(payload, "forged") {
				t.Fatal("trusted body identity")
			}
			if strings.Contains(payload, ep.ID().String()) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("missing verified peer identity")
	}
	request("GET", "/runs/"+id+"/snapshot", "", 200)
	request("POST", "/runs/"+id, `{"text":"again"}`, 200)
	request("POST", "/runs/"+id+"/cancel", `{}`, 200)
	request("GET", "/runs/missing", "", 404)
	// The existing NDJSON stream must flush headers and replay events over QUIC.
	streamCtx, stopStream := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(streamCtx, "GET", "http://iroh/bonnie/v1/runs/"+id+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatal("bad stream response")
	}
	line, err := bufio.NewReader(stream.Body).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(line) {
		t.Fatalf("invalid event: %s", line)
	}
	stopStream()
	if err := stream.Body.Close(); err != nil {
		t.Fatal(err)
	}
	c.Unpair(ep.ID())
	request("GET", "/runs/"+id, "", 401)
	request("GET", "/health", "", 401)
}

func TestValidationAndShutdown(t *testing.T) {
	t.Parallel()
	runner := runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory())
	if _, err := irohchannel.New(runner, irohchannel.Config{TurnPolicy: "bad"}); !errors.Is(err, channel.ErrUnknownTurnPolicy) {
		t.Fatal(err)
	}
	if _, err := irohchannel.New(runner, irohchannel.Config{Peers: []key.EndpointID{{}}}); err == nil {
		t.Fatal("empty peer accepted")
	}
	c, err := irohchannel.New(runner, irohchannel.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("started stopped channel")
	}
}

func TestFailedStartCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory())
	c, err := irohchannel.New(runner, irohchannel.Config{EndpointOptions: []goiroh.Option{goiroh.WithALPNs("not-bonnie")}})
	if err != nil {
		t.Fatal(err)
	}
	// A cancelled context must stop startup before the endpoint binds.
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if err := c.Start(cancelled); err == nil {
		t.Fatal("cancelled startup succeeded")
	}
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
