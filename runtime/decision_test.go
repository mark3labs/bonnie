package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// scriptedDecider answers each boolean question with p and counts calls.
type scriptedDecider struct {
	calls atomic.Int32
	p     float64
	err   error
	delay time.Duration
}

func (d *scriptedDecider) decide(_ context.Context, req kit.DecisionRequest) (*kit.DecisionResult, error) {
	d.calls.Add(1)
	time.Sleep(d.delay)
	if d.err != nil {
		return nil, d.err
	}
	res := &kit.DecisionResult{Model: "jev-1.14.0", Usage: kit.DecisionUsage{InputTokens: 42}}
	for _, q := range req.Questions {
		res.Answers = append(res.Answers, kit.DecisionAnswer{ID: q.ID, Type: q.Type, Probability: d.p})
	}
	return res, nil
}

func decisionRequest(text string) kit.DecisionRequest {
	return kit.DecisionRequest{
		Model:     "ollama/pplx-decider",
		State:     kit.DecisionState{Text: text},
		Questions: []kit.DecisionQuestion{kit.BooleanQuestion("risky", "Is the action risky?")},
	}
}

func decisionRecords(t *testing.T, j Journal, runID string) []DecisionRecord {
	t.Helper()
	recs, err := j.Replay(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []DecisionRecord
	for _, rec := range recs {
		if rec.Kind != RecordDecision {
			continue
		}
		if strings.Contains(string(rec.Payload), "secret state") {
			t.Fatalf("record keeps the state: %s", rec.Payload)
		}
		d, err := DecodeDecisionRecord(rec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// Each call is journalled, with its answers and
// usage, but not with the state it was about.
func TestDecideJournalsEachCall(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s := NewSession("run", j)
	d := &scriptedDecider{p: 0.9}
	for range 2 {
		res, err := s.Decide(context.Background(), d.decide, DecisionCall{Request: decisionRequest("secret state")})
		if err != nil {
			t.Fatal(err)
		}
		if a, err := res.Boolean("risky"); err != nil || a.Probability != 0.9 {
			t.Fatalf("answer = %+v, %v", a, err)
		}
	}
	if got := d.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2: a call without a key always asks", got)
	}
	recs := decisionRecords(t, j, "run")
	if len(recs) != 2 {
		t.Fatalf("records = %d", len(recs))
	}
	r := recs[0]
	if r.Source != DecisionSourceHost || r.Model != "ollama/pplx-decider" || r.AnsweredBy != "jev-1.14.0" ||
		r.Usage.InputTokens != 42 || len(r.Questions) != 1 || len(r.Answers) != 1 || r.InputSHA256 == "" || r.Error != "" {
		t.Fatalf("record = %+v", r)
	}
}

// A failed call is journalled with its error class only: the provider's
// body can hold sensitive data.
func TestDecideJournalsFailureClassOnly(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s := NewSession("run", j)
	d := &scriptedDecider{err: &kit.DecisionAPIError{StatusCode: 500, Message: "secret body", Body: "secret body"}}
	if _, err := s.Decide(context.Background(), d.decide, DecisionCall{Request: decisionRequest("x")}); err == nil {
		t.Fatal("no error")
	}
	recs, _ := j.Replay(context.Background(), "run")
	for _, rec := range recs {
		if strings.Contains(string(rec.Payload), "secret body") {
			t.Fatalf("record keeps the provider body: %s", rec.Payload)
		}
	}
	if got := decisionRecords(t, j, "run"); len(got) != 1 || got[0].Error != "http_500" || got[0].Answers != nil {
		t.Fatalf("records = %+v", got)
	}
}

// A key returns the stored answer, rejects different input, and does not
// keep a failure.
func TestDecideKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	s := NewSession("run", j)
	d := &scriptedDecider{p: 0.2}
	call := DecisionCall{Request: decisionRequest("x"), Key: "gate:1"}
	for range 3 {
		res, err := s.Decide(ctx, d.decide, call)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := res.Boolean("risky"); a.Probability != 0.2 {
			t.Fatalf("answer = %+v", a)
		}
	}
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	other := DecisionCall{Request: decisionRequest("y"), Key: "gate:1"}
	if _, err := s.Decide(ctx, d.decide, other); !errors.Is(err, ErrDecisionKeyReused) {
		t.Fatalf("error = %v, want ErrDecisionKeyReused", err)
	}
	// Provider options are input too: they can route to another model.
	routed := DecisionCall{Request: decisionRequest("x"), Key: "gate:1"}
	routed.Request.ProviderOptions = map[string]any{"gateway": map[string]any{"order": []string{"other"}}}
	if _, err := s.Decide(ctx, d.decide, routed); !errors.Is(err, ErrDecisionKeyReused) {
		t.Fatalf("error = %v, want ErrDecisionKeyReused for other provider options", err)
	}
	if got := len(decisionRecords(t, j, "run")); got != 1 {
		t.Fatalf("records = %d, want 1: a reused key writes no record", got)
	}

	failing := &scriptedDecider{err: kit.ErrDecisionRefused}
	failed := DecisionCall{Request: decisionRequest("z"), Key: "gate:2"}
	if _, err := s.Decide(ctx, failing.decide, failed); !errors.Is(err, kit.ErrDecisionRefused) {
		t.Fatalf("error = %v", err)
	}
	if _, err := s.Decide(ctx, d.decide, failed); err != nil {
		t.Fatalf("a failed call was reused: %v", err)
	}
	if got := d.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	if got := decisionRecords(t, j, "run"); len(got) != 3 || got[1].Error != "refused" {
		t.Fatalf("records = %+v", got)
	}
}

// A keyed decision is durable: a session in a new process, which shares
// only the journal on disk, gets the stored answer without a model call.
func TestDecideKeyAcrossProcessBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	call := DecisionCall{Request: decisionRequest("x"), Key: "gate"}

	first, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSession("run", first).Decide(ctx, (&scriptedDecider{p: 0.7}).decide, call); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	s, err := Restore(ctx, "run", second)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.GetMessages()); n != 0 {
		t.Fatalf("a decision record became %d messages", n)
	}
	d := &scriptedDecider{p: 0.1}
	res, err := s.Decide(ctx, d.decide, call)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := res.Boolean("risky"); a.Probability != 0.7 || res.Model != "jev-1.14.0" {
		t.Fatalf("result = %+v", res)
	}
	if d.calls.Load() != 0 {
		t.Fatal("the model was asked again after a restart")
	}
}

// Parallel calls with one key make one model call.
func TestDecideKeySerializesCalls(t *testing.T) {
	t.Parallel()
	s := NewSession("run", NewMemoryJournal())
	d := &scriptedDecider{p: 0.5, delay: 20 * time.Millisecond}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := s.Decide(context.Background(), d.decide, DecisionCall{Request: decisionRequest("x"), Key: "k"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

type appendFailJournal struct{ Journal }

func (appendFailJournal) Append(context.Context, Record) (int, error) {
	return 0, errors.New("disk full")
}

// An answer that is not durable is not returned.
func TestDecideJournalFailureReturnsNoResult(t *testing.T) {
	t.Parallel()
	s := NewSession("run", appendFailJournal{NewMemoryJournal()})
	res, err := s.Decide(context.Background(), (&scriptedDecider{p: 1}).decide, DecisionCall{Request: decisionRequest("x")})
	if err == nil || res != nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("result = %v, error = %v", res, err)
	}
}

// A tool's call ID reaches the record; a nil decider is a clear error.
func TestDecideToolCallIDAndNoDecider(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s := NewSession("run", j)
	tool := kit.NewTool("probe", "probe", func(ctx context.Context, _ struct{}) (kit.ToolOutput, error) {
		_, err := s.Decide(ctx, (&scriptedDecider{}).decide, DecisionCall{Request: decisionRequest("x")})
		return kit.TextResult("ok"), err
	})
	if _, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-7", Name: "probe", Input: "{}"}); err != nil {
		t.Fatal(err)
	}
	if got := decisionRecords(t, j, "run"); len(got) != 1 || got[0].ToolCallID != "call-7" {
		t.Fatalf("records = %+v", got)
	}
	if _, err := s.Decide(context.Background(), nil, DecisionCall{}); !errors.Is(err, ErrNoDecider) {
		t.Fatalf("error = %v", err)
	}
}
