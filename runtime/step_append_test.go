package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// stepMessages is one tool-calling step: an assistant message carrying a
// tool_use and the tool message carrying its result. It is the exact shape
// that used to be written as two independent records.
func stepMessages() []kit.LLMMessage {
	return []kit.LLMMessage{
		{
			Role: kit.LLMMessageRole("assistant"),
			Content: []kit.LLMMessagePart{
				kit.LLMTextPart{Text: "Reading the file."},
				kit.LLMToolCallPart{
					ToolCallID: "tc-step-1",
					ToolName:   "read_file",
					Input:      `{"path":"notes.txt"}`,
				},
			},
		},
		{
			Role: kit.LLMMessageRole("tool"),
			Content: []kit.LLMMessagePart{kit.LLMToolResultPart{
				ToolCallID: "tc-step-1",
				Output:     kit.LLMToolResultOutputContentText{Text: "first turn"},
			}},
		},
	}
}

// countingJournal records how a step reached the journal: one AppendStep
// call, or N Append calls. It is the evidence that Session routes a whole
// step through [StepJournal] rather than falling back.
type countingJournal struct {
	*MemoryJournal
	stepCalls   int
	appendCalls int
	lastBatch   []Record
}

func (c *countingJournal) AppendStep(ctx context.Context, recs []Record) ([]int, error) {
	c.stepCalls++
	c.lastBatch = recs
	return c.MemoryJournal.AppendStep(ctx, recs)
}

func (c *countingJournal) Append(ctx context.Context, rec Record) (int, error) {
	c.appendCalls++
	return c.MemoryJournal.Append(ctx, rec)
}

var (
	_ Journal     = (*countingJournal)(nil)
	_ StepJournal = (*countingJournal)(nil)
)

// noStepJournal hides AppendStep, so Session must take the per-record
// fallback. Embedding would promote the method back into the interface set,
// so every Journal method is declared by hand.
type noStepJournal struct {
	inner Journal
}

func (n *noStepJournal) Append(ctx context.Context, rec Record) (int, error) {
	return n.inner.Append(ctx, rec)
}
func (n *noStepJournal) Replay(ctx context.Context, runID string) ([]Record, error) {
	return n.inner.Replay(ctx, runID)
}
func (n *noStepJournal) Checkpoint(ctx context.Context, runID string, state RunState) error {
	return n.inner.Checkpoint(ctx, runID, state)
}
func (n *noStepJournal) State(ctx context.Context, runID string) (RunState, error) {
	return n.inner.State(ctx, runID)
}
func (n *noStepJournal) Runs(ctx context.Context, state RunState) ([]string, error) {
	return n.inner.Runs(ctx, state)
}
func (n *noStepJournal) Persisted() bool { return n.inner.Persisted() }
func (n *noStepJournal) Close() error    { return n.inner.Close() }

var _ Journal = (*noStepJournal)(nil)

// TestSessionAppendStepIsOneJournalWrite pins the dispatch: a step that Kit
// hands over through kit.StepAppender reaches the journal as ONE AppendStep
// call carrying both messages, never as two Append calls. Before Kit
// v0.106.0 there was no other way, and the crash window between the two
// writes is what runtime/repair.go exists to clean up.
func TestSessionAppendStepIsOneJournalWrite(t *testing.T) {
	t.Parallel()

	j := &countingJournal{MemoryJournal: NewMemoryJournal()}
	s := NewSession("step-one-write", j)

	ids, err := s.AppendStep(context.Background(), stepMessages())
	if err != nil {
		t.Fatalf("AppendStep: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d entry IDs, want one per message (2)", len(ids))
	}
	if j.stepCalls != 1 {
		t.Fatalf("AppendStep called %d times, want 1", j.stepCalls)
	}
	if j.appendCalls != 0 {
		t.Fatalf("Append called %d times, want 0: the step fell back to "+
			"per-record writes", j.appendCalls)
	}
	if len(j.lastBatch) != 2 {
		t.Fatalf("batch held %d records, want 2", len(j.lastBatch))
	}
	// The pair must land in the tree as a unit too.
	if got := len(s.GetMessages()); got != 2 {
		t.Fatalf("GetMessages returned %d messages, want 2", got)
	}
}

// TestSessionAppendStepFallbackCoversPlainJournals proves a journal without
// [StepJournal] still works. The fallback writes one record at a time, which
// has the same crash window the pre-v0.106 path had; the torn-write repair
// in Restore covers it, and the journal conformance suite says which side of
// that line an implementation stands on.
func TestSessionAppendStepFallbackCoversPlainJournals(t *testing.T) {
	t.Parallel()

	inner := NewMemoryJournal()
	j := &noStepJournal{inner: inner}
	s := NewSession("step-fallback", j)

	ids, err := s.AppendStep(context.Background(), stepMessages())
	if err != nil {
		t.Fatalf("AppendStep: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d entry IDs, want 2", len(ids))
	}
	recs, err := j.Replay(context.Background(), "step-fallback")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("journal holds %d records, want 2", len(recs))
	}
	if len(s.GetMessages()) != 2 {
		t.Fatalf("GetMessages returned %d messages, want 2", len(s.GetMessages()))
	}
}

// TestSessionAppendStepCrossesProcessBoundary is the durability claim, stated
// as a test. A full tool-calling step is committed by one AppendStep call,
// the journal is closed, and a second Session — sharing nothing but the
// journal directory — rebuilds the step complete: tool call and tool result
// both present. The pre-v0.106 path could not promise this; the torn write
// it left behind is what the repair had to drop.
func TestSessionAppendStepCrossesProcessBoundary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	j1, err := OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	s1 := NewSession("step-boundary", j1)
	if _, err := s1.AppendStep(context.Background(), stepMessages()); err != nil {
		t.Fatalf("AppendStep: %v", err)
	}
	if err := j1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A second process: a new journal handle over the same directory.
	j2, err := OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = j2.Close() }()
	s2, err := Restore(context.Background(), "step-boundary", j2)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var toolCalls, toolResults int
	for _, msg := range s2.GetMessages() {
		for _, part := range msg.Content {
			switch part.(type) {
			case kit.LLMToolCallPart:
				toolCalls++
			case kit.LLMToolResultPart:
				toolResults++
			}
		}
	}
	if toolCalls != 1 || toolResults != 1 {
		t.Fatalf("restored %d tool calls and %d tool results, want 1 of each: "+
			"the step did not survive intact", toolCalls, toolResults)
	}
	if err := s2.repairTail(context.Background(), mustReplay(t, j2, "step-boundary")); err != nil {
		t.Fatalf("repairTail on an intact step must be a no-op: %v", err)
	}
}

// mustReplay is a test helper that fails the test instead of returning an
// error a test would only forget to check.
func mustReplay(t *testing.T, j Journal, runID string) []Record {
	t.Helper()
	recs, err := j.Replay(context.Background(), runID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return recs
}

// TestFileJournalAppendStepIsAllOrNothing pins the atomicity contract at the
// journal level. A batch whose second record cannot be encoded fails whole:
// nothing is written, and the sequence counter is restored so the next write
// reuses no numbers and skips none.
func TestFileJournalAppendStepIsAllOrNothing(t *testing.T) {
	t.Parallel()

	j, err := OpenFileJournal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	defer func() { _ = j.Close() }()
	ctx := context.Background()

	step := []Record{
		{RunID: "step-atomic", Kind: RecordMessage, EntryID: "m1"},
		{RunID: "step-atomic", Kind: RecordMessage, EntryID: "m2",
			Payload: json.RawMessage("{not json")},
	}
	if _, err := j.AppendStep(ctx, step); err == nil {
		t.Fatal("a record that cannot be encoded must fail the batch")
	}
	if recs := mustReplay(t, j, "step-atomic"); len(recs) != 0 {
		t.Fatalf("%d records survived a failed batch, want 0", len(recs))
	}

	// The sequence counter must not have moved: the next record takes
	// sequence 1, not 3.
	seq, err := j.Append(ctx, Record{RunID: "step-atomic", Kind: RecordMessage})
	if err != nil {
		t.Fatalf("Append after failed batch: %v", err)
	}
	if seq != 1 {
		t.Fatalf("next sequence = %d, want 1: the failed batch leaked numbers", seq)
	}

	// A good batch commits every record with consecutive numbers.
	seqs, err := j.AppendStep(ctx, []Record{
		{RunID: "step-atomic-2", Kind: RecordMessage, EntryID: "m1"},
		{RunID: "step-atomic-2", Kind: RecordMessage, EntryID: "m2"},
	})
	if err != nil {
		t.Fatalf("AppendStep: %v", err)
	}
	if !slices.Equal(seqs, []int{1, 2}) {
		t.Fatalf("seqs = %v, want [1 2]", seqs)
	}
	if recs := mustReplay(t, j, "step-atomic-2"); len(recs) != 2 {
		t.Fatalf("%d records after a good batch, want 2", len(recs))
	}
}

// TestAppendStepRejectsMixedRuns guards the unit: a batch that spans two
// runs can be neither one file nor one transaction, so both journals refuse
// it rather than split it.
func TestAppendStepRejectsMixedRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mixed := []Record{
		{RunID: "run-a", Kind: RecordMessage},
		{RunID: "run-b", Kind: RecordMessage},
	}

	mem := NewMemoryJournal()
	if _, err := mem.AppendStep(ctx, mixed); err == nil {
		t.Fatal("memory journal accepted a batch that spans two runs")
	}

	fj, err := OpenFileJournal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	defer func() { _ = fj.Close() }()
	if _, err := fj.AppendStep(ctx, mixed); err == nil {
		t.Fatal("file journal accepted a batch that spans two runs")
	}
}

// TestSessionAppendStepSurvivesCancelledContext pins the cancellation
// contract from kit.StepAppender: Kit persists a completed step before it
// checks for cancellation, so ctx can arrive already cancelled. A session
// that aborted on that would silently discard finished work, because Kit
// ignores the error.
func TestSessionAppendStepSurvivesCancelledContext(t *testing.T) {
	t.Parallel()

	j := NewMemoryJournal()
	s := NewSession("step-cancelled", j)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the turn was interrupted AFTER the step completed

	if _, err := s.AppendStep(ctx, stepMessages()); err != nil {
		if errors.Is(err, context.Canceled) {
			t.Fatal("a cancelled context dropped a completed step: the exact " +
				"loss kit.StepAppender's contract forbids")
		}
		t.Fatalf("AppendStep: %v", err)
	}
	recs := mustReplay(t, j, "step-cancelled")
	if len(recs) != 2 {
		t.Fatalf("journal holds %d records, want 2: the completed step was lost", len(recs))
	}
}
