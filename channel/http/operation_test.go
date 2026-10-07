package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// A completed retry must not repeat a tool, also in a second process.
func TestOperationRetryNeverRepeatsTool(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	var tools atomic.Int32
	tool := kit.NewTool("act", "Record a side effect", func(context.Context, struct{}) (kit.ToolOutput, error) {
		tools.Add(1)
		return kit.TextResult("done"), nil
	})
	model := fakemodel.New(fakemodel.Call("act", `{}`), fakemodel.Say("finished"))
	hermetic := func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents = true, true, true, true, true
		o.DisableCoreTools, o.Quiet = true, true
	}
	r := runtime.NewRunner(j, runtime.KitAgent(hermetic, model.Option(), kit.WithExtraTools(tool)))
	c := New(r, WithAuthenticator(headerAuth))
	server := httptest.NewServer(c.Handler())
	first := &testServer{Server: server, runner: r, channel: c}
	req := StartRequest{OperationID: "deploy", Text: "act once"}
	resp, run := first.postAs(t, "alice", "/bonnie/v1/runs", req)
	if resp.StatusCode != http.StatusOK || run.Response != "finished" {
		t.Fatalf("start = %d %+v", resp.StatusCode, run)
	}
	req.Text = "a changed retry must not run"
	_, retry := first.postAs(t, "alice", "/bonnie/v1/runs", req)
	if retry.RunID != run.RunID || retry.Response != run.Response {
		t.Fatalf("retry = %+v", retry)
	}
	server.Close()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	agent := &stubAgent{}
	second := newTestServerOn(t, next, agent, WithAuthenticator(headerAuth))
	_, recovered := second.postAs(t, "alice", "/bonnie/v1/runs", req)
	if recovered.RunID != run.RunID || recovered.Response != "finished" {
		t.Fatalf("recovered = %+v", recovered)
	}
	streamServer := httptest.NewServer(New(second.runner).Handler())
	t.Cleanup(streamServer.Close)
	events := readStream(t, &testServer{Server: streamServer}, "/bonnie/v1/runs/"+run.RunID+"/stream", 5)
	if events[len(events)-1].State != runtime.RunCompleted {
		t.Fatalf("recovered events = %+v", events)
	}
	if tools.Load() != 1 || len(model.Requests()) != 2 || agent.calls() != 0 {
		t.Fatalf("tool calls = %d, model calls = %d, second agent calls = %d", tools.Load(), len(model.Requests()), agent.calls())
	}
}

// Identity fields cannot borrow one another's separators or kind.
func TestOperationIdentityAndAddressIsolation(t *testing.T) {
	t.Parallel()
	principals := []*channel.Principal{
		{Authenticator: "a/b", Kind: "user", ID: "c"},
		{Authenticator: "a", Kind: "user", ID: "b/c"},
		{Authenticator: "a", Kind: "service", ID: "b/c"},
	}
	keys := map[string]bool{}
	for _, p := range principals {
		for _, op := range []string{"d/e", "", "e"} {
			key := operationAddress(p, op)
			if keys[key] {
				t.Fatalf("identity collision: %q", key)
			}
			keys[key] = true
		}
	}
	s := newTestServer(t, &stubAgent{}, WithIDGenerator(seqID()), WithAuthenticator(headerAuth))
	_, operation := s.postAs(t, "alice", "/bonnie/v1/runs", StartRequest{OperationID: "op", Text: "one"})
	for _, address := range []string{"operation/test-header/alice/op", operationAddress(&channel.Principal{Authenticator: "test-header", Kind: "user", ID: "alice"}, "op")} {
		_, ordinary := s.postAs(t, "alice", "/bonnie/v1/runs", StartRequest{Address: address, Text: "two"})
		if ordinary.RunID == operation.RunID {
			t.Fatalf("ordinary address accessed operation: %q", address)
		}
	}
	resp, _ := s.postAs(t, "alice", "/bonnie/v1/runs", StartRequest{Address: "addr", OperationID: "op", Text: "bad"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("address plus operation = %d", resp.StatusCode)
	}
}

// In-flight retries report state without steering or queueing another turn.
func TestOperationRetryWhileActive(t *testing.T) {
	t.Parallel()
	release, active := make(chan struct{}), make(chan struct{})
	agent := &stubAgent{block: release, started: active}
	s := newTestServer(t, agent, WithAuthenticator(headerAuth))
	req := StartRequest{OperationID: "active", Text: "work"}
	done := make(chan RunResponse, 1)
	go func() { _, run := s.postAs(t, "alice", "/bonnie/v1/runs", req); done <- run }()
	<-active
	resp, retry := s.postAs(t, "alice", "/bonnie/v1/runs", req)
	close(release)
	first := <-done
	if resp.StatusCode != http.StatusOK || retry.State != runtime.RunRunning || retry.RunID != first.RunID {
		t.Fatalf("retry = %d %+v", resp.StatusCode, retry)
	}
	if agent.calls() != 1 || len(agent.steered()) != 0 {
		t.Fatalf("calls = %d, steers = %v", agent.calls(), agent.steered())
	}
}

// Failure is not permission to repeat a side effect. A retry reports failed.
func TestOperationRetryAfterFailure(t *testing.T) {
	t.Parallel()
	model := fakemodel.New() // Every model call fails; each call is recorded.
	hermetic := func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents = true, true, true, true, true
		o.DisableCoreTools, o.Quiet = true, true
	}
	j := runtime.NewMemoryJournal()
	r := runtime.NewRunner(j, runtime.KitAgent(hermetic, model.Option()))
	c := New(r, WithAuthenticator(headerAuth))
	server := httptest.NewServer(c.Handler())
	t.Cleanup(server.Close)
	s := &testServer{Server: server, runner: r, channel: c}
	req := StartRequest{OperationID: "failed", Text: "work"}
	resp, _ := s.postAs(t, "alice", "/bonnie/v1/runs", req)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first = %d", resp.StatusCode)
	}
	calls := len(model.Requests())
	resp, retry := s.postAs(t, "alice", "/bonnie/v1/runs", req)
	if resp.StatusCode != http.StatusOK || retry.State != runtime.RunFailed {
		t.Fatalf("retry = %d %+v", resp.StatusCode, retry)
	}
	if calls == 0 || len(model.Requests()) != calls {
		t.Fatalf("model ran again: %d -> %d", calls, len(model.Requests()))
	}
}

// A durable admission left pending by a crash does not execute on retry.
func TestOperationPendingRetryDoesNotStart(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	s := newTestServerOn(t, j, &stubAgent{}, WithAuthenticator(headerAuth))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Test-User", "alice")
	p, err := headerAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.channel.core.Addresses().Resolve(t.Context(), operationAddress(p, "pending"), s.channel.core.NewID)
	if err != nil {
		t.Fatal(err)
	}
	second := newTestServerOn(t, j, &stubAgent{}, WithAuthenticator(headerAuth))
	resp, retry := second.postAs(t, "alice", "/bonnie/v1/runs", StartRequest{OperationID: "pending", Text: "work"})
	if resp.StatusCode != http.StatusOK || retry.State != runtime.RunPending || retry.RunID != id || second.agent.calls() != 0 {
		t.Fatalf("pending retry = %d %+v", resp.StatusCode, retry)
	}
}

// Reset by run ID frees all ordinary aliases, but not the operation key.
func TestResetFreesAliasesAndKeepsOperation(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{}, WithIDGenerator(seqID()), WithAuthenticator(headerAuth))
	req := StartRequest{OperationID: "reset", Text: "one"}
	_, first := s.postAs(t, "alice", "/bonnie/v1/runs", req)
	for _, address := range []string{"one", "two"} {
		if err := s.channel.Rebind(t.Context(), address, first.RunID); err != nil {
			t.Fatal(err)
		}
	}
	resp, _ := s.postAs(t, "alice", "/bonnie/v1/runs/"+first.RunID+"/reset", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %d", resp.StatusCode)
	}
	for _, address := range []string{"one", "two"} {
		if _, bound, err := s.channel.core.Lookup(t.Context(), address); err != nil || bound {
			t.Fatalf("alias %s still bound: %v", address, err)
		}
		_, next := s.postAs(t, "alice", "/bonnie/v1/runs", StartRequest{Address: address, Text: "new"})
		if next.RunID == first.RunID {
			t.Fatalf("alias %s reused retired run", address)
		}
	}
	_, retry := s.postAs(t, "alice", "/bonnie/v1/runs", req)
	if retry.RunID != first.RunID || retry.State != runtime.RunRetired {
		t.Fatalf("reset operation retry = %+v", retry)
	}
	if s.agent.calls() != 3 {
		t.Fatalf("reset retry ran agent: calls = %d", s.agent.calls())
	}
}

// A waiting operation keeps its suspension on retry in another Runner.
func TestOperationWaitingRetryAcrossRunner(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	first := newTestServerOn(t, j, &stubAgent{turns: []*kit.TurnResult{{
		Response: "Approval needed", HaltedByTool: "request_approval",
		FinalValue: runtime.SuspendRequest{Kind: runtime.SuspendApproval, Prompt: "Proceed?"},
	}}}, WithAuthenticator(headerAuth))
	req := StartRequest{OperationID: "approval", Text: "act"}
	_, parked := first.postAs(t, "alice", "/bonnie/v1/runs", req)
	if parked.State != runtime.RunWaiting {
		t.Fatalf("parked = %+v", parked)
	}
	second := newTestServerOn(t, j, &stubAgent{}, WithAuthenticator(headerAuth))
	resp, retry := second.postAs(t, "alice", "/bonnie/v1/runs", req)
	if resp.StatusCode != http.StatusOK || retry.RunID != parked.RunID || retry.Suspend == nil || retry.Suspend.Prompt != "Proceed?" {
		t.Fatalf("waiting retry = %d %+v", resp.StatusCode, retry)
	}
	if first.agent.calls() != 1 || second.agent.calls() != 0 {
		t.Fatalf("calls = %d / %d", first.agent.calls(), second.agent.calls())
	}
}

// Reset must not free another channel's binding or an alias moved to a new run.
func TestResetPreservesOtherBindings(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	s := newTestServerOn(t, j, &stubAgent{}, WithIDGenerator(seqID()))
	_, first := s.post(t, "/bonnie/v1/runs", StartRequest{Address: "old", Text: "one"})
	_, other := s.post(t, "/bonnie/v1/runs", StartRequest{Address: "other", Text: "two"})
	if err := s.channel.Rebind(t.Context(), "moved", other.RunID); err != nil {
		t.Fatal(err)
	}
	foreign := chat.NewCore(s.runner, "slack", channel.PolicyQueue)
	if err := foreign.Bind(t.Context(), "foreign", first.RunID); err != nil {
		t.Fatal(err)
	}
	resp, _ := s.post(t, "/bonnie/v1/runs/"+first.RunID+"/reset", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %d", resp.StatusCode)
	}
	id, bound, err := s.channel.core.Lookup(t.Context(), "moved")
	if err != nil || !bound || id != other.RunID {
		t.Fatalf("moved binding = %q %v %v", id, bound, err)
	}
	id, bound, err = foreign.Lookup(t.Context(), "foreign")
	if err != nil || !bound || id != first.RunID {
		t.Fatalf("foreign binding = %q %v %v", id, bound, err)
	}
}
