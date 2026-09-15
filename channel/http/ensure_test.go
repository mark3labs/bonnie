package http

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// The run ID `POST /bonnie/v1/addresses/{address}` hands back must name a run
// the ID-addressed routes can already reach.
//
// The route exists so a client can subscribe before it speaks: a turn's
// reasoning and tool events are live-only, so a client that learns its run ID
// from the reply to its first message has already missed them. That only
// works if the ID is usable the moment it is issued. It was not — the binding
// was journalled but the run was not, so every ID-addressed route answered
// 404 until the first turn happened to write a record, and the route handed
// out an ID whose whole purpose was to be used immediately.
func TestEnsureAddressYieldsAUsableRun(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hello"}}},
		WithIDGenerator(func() string { return "ensure-me" }))

	resp, ensured := s.post(t, "/bonnie/v1/addresses/desk-1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ensure = %d, want 200", resp.StatusCode)
	}
	if ensured.RunID != "ensure-me" {
		t.Fatalf("run_id = %q, want the bound run", ensured.RunID)
	}

	// Read the run the route just named.
	getResp, got := s.get(t, "/bonnie/v1/runs/"+ensured.RunID)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET the ensured run = %d, want 200: the route handed out an ID that does not resolve", getResp.StatusCode)
	}
	if got.State != runtime.RunPending {
		t.Fatalf("state = %q, want %q: a run that exists but has not run a turn", got.State, runtime.RunPending)
	}

	// Subscribe to it, which is what the route is for.
	streamResp, err := http.Get(s.URL + "/bonnie/v1/runs/" + ensured.RunID + "/stream?cursor=0")
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = streamResp.Body.Close() }()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("stream on the ensured run = %d, want 200", streamResp.StatusCode)
	}

	// And a message addressed to that exact ID lands on that same run, with
	// no second run created behind it.
	sendResp, sent := s.post(t, "/bonnie/v1/runs/"+ensured.RunID, SendRequest{Text: "hi"})
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("send to the ensured run = %d, want 200", sendResp.StatusCode)
	}
	if sent.RunID != ensured.RunID {
		t.Fatalf("send answered for %q, want %q", sent.RunID, ensured.RunID)
	}
	if sent.State != runtime.RunCompleted {
		t.Fatalf("state after send = %q, want completed", sent.State)
	}
}

// Ensure is idempotent: an address that already owns a run returns that run
// and creates nothing. The birth record is written once, so a client that
// calls the route on every reconnect does not grow the journal.
func TestEnsureAddressIsIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{}, WithIDGenerator(seqID()))

	_, first := s.post(t, "/bonnie/v1/addresses/desk-2", nil)
	_, second := s.post(t, "/bonnie/v1/addresses/desk-2", nil)
	if first.RunID != second.RunID {
		t.Fatalf("ensure returned %q then %q, want one run", first.RunID, second.RunID)
	}

	recs, err := s.runner.Journal().Replay(t.Context(), first.RunID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("the run holds %d records, want 1: the birth record must be written once", len(recs))
	}
	if recs[0].Kind != runtime.RecordState || recs[0].State != runtime.RunPending {
		t.Fatalf("record = %+v, want one pending state record", recs[0])
	}
}

// A pending run is a real run to every reader: it lists, it reports its
// state, and it is not mistaken for one that finished.
func TestPendingRunIsListedAndNotTerminal(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{}, WithIDGenerator(func() string { return "listed-me" }))

	s.post(t, "/bonnie/v1/addresses/desk-3", nil) //nolint:errcheck // asserted below

	ids, err := s.runner.Journal().Runs(t.Context(), runtime.RunPending)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	var found bool
	for _, id := range ids {
		if id == "listed-me" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending runs = %v, want the ensured run among them", ids)
	}
	if runtime.RunPending.IsTerminal() {
		t.Fatal("pending must not be terminal: the run has not run yet")
	}
}

// The cursor the route reports is the run's current journal position, so a
// client that opens its stream at that cursor sees what happens next and not
// the birth record it already knows about.
func TestEnsureAddressReportsItsCursor(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{}, WithIDGenerator(func() string { return "cursor-at" }))

	_, ensured := s.post(t, "/bonnie/v1/addresses/desk-4", nil)
	if ensured.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1: the birth record is the run's first position", ensured.Cursor)
	}

	// The wire carries it, so a client does not have to guess.
	raw, _ := json.Marshal(ensured)
	var onWire map[string]any
	if err := json.Unmarshal(raw, &onWire); err != nil {
		t.Fatal(err)
	}
	if _, ok := onWire["cursor"]; !ok {
		t.Fatalf("the response omits cursor: %s", raw)
	}
}
