package runtime

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// compactingAgent is a fakeAgent that can compact: it writes one compaction
// entry that keeps nothing, the way a summary of everything would.
type compactingAgent struct {
	fakeAgent
	compacted int
}

func (c *compactingAgent) Compact(context.Context, *kit.CompactionOptions, string) (*kit.CompactionResult, error) {
	c.compacted++
	_, err := c.session.AppendCompaction("summary of everything so far", "", 100, 10, 2, nil, nil)
	return &kit.CompactionResult{}, err
}

func controlsRunner(t *testing.T) (*Runner, Journal, *compactingAgent) {
	t.Helper()
	agent := &compactingAgent{}
	agent.turns = []*kit.TurnResult{{Response: "one"}, {Response: "two"}, {Response: "three"}}
	j := NewMemoryJournal()
	r := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) {
		agent.session = s
		return agent, nil
	})
	return r, j, agent
}

// Retire closes the run: the state is terminal, the reason is journalled,
// and a later Start or Resume is refused — across a runner boundary, since
// the refusal reads the journal.
func TestRetireClosesTheRunForGood(t *testing.T) {
	t.Parallel()
	r, j, _ := controlsRunner(t)
	ctx := context.Background()

	if _, err := r.Start(ctx, "run-r", Input{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Retire(ctx, "run-r", "user asked for /new"); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if state, _ := j.State(ctx, "run-r"); state != RunRetired || !state.IsTerminal() {
		t.Fatalf("state = %q, want retired and terminal", state)
	}
	s, err := Restore(ctx, "run-r", j)
	if err != nil {
		t.Fatal(err)
	}
	notes := s.GetExtensionData(ExtRetired)
	if len(notes) != 1 || notes[0].Data != "user asked for /new" {
		t.Fatalf("retired note = %v", notes)
	}

	// A second runner over the same journal refuses to continue it.
	other := NewRunner(j, fakeFactoryOnly())
	if _, err := other.Start(ctx, "run-r", Input{Text: "again"}); !errors.Is(err, ErrRunRetired) {
		t.Fatalf("Start on a retired run = %v, want ErrRunRetired", err)
	}
	if _, err := other.Resume(ctx, "run-r", []InputResponse{{Text: "x"}}); !errors.Is(err, ErrRunRetired) {
		t.Fatalf("Resume on a retired run = %v, want ErrRunRetired", err)
	}
	// Retiring twice is a no-op; retiring nothing is not found.
	if err := r.Retire(ctx, "run-r", ""); err != nil {
		t.Fatalf("second Retire: %v", err)
	}
	if err := r.Retire(ctx, "nope", ""); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("Retire unknown = %v", err)
	}
}

func fakeFactoryOnly() AgentFactory {
	f, _ := fakeFactory(&kit.TurnResult{Response: "x"})
	return f
}

// Clear keeps the run and forgets the conversation: after it, a restore
// has no messages before the marker, the run ID is unchanged, the next turn
// starts from an empty window, and the run-level facts survive.
func TestClearForgetsTheConversationNotTheRun(t *testing.T) {
	t.Parallel()
	r, j, _ := controlsRunner(t)
	ctx := context.Background()

	if _, err := r.Start(ctx, "run-c", Input{Text: "first", Title: "kept", Origin: Origin{Channel: "slack", Kind: "dm"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Clear(ctx, "run-c"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	s, err := Restore(ctx, "run-c", j)
	if err != nil {
		t.Fatal(err)
	}
	if msgs := s.GetMessages(); len(msgs) != 0 {
		t.Fatalf("restored messages after clear = %d, want 0", len(msgs))
	}
	if s.Title() != "kept" || s.Origin().Channel != "slack" {
		t.Fatalf("clear lost the run facts: title %q origin %+v", s.Title(), s.Origin())
	}

	// The next turn builds on the empty window and its messages hang off
	// the root, not the cleared branch.
	if _, err := r.Start(ctx, "run-c", Input{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	s, err = Restore(ctx, "run-c", j)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range s.GetMessages() {
		texts = append(texts, messageText(m))
	}
	if len(texts) != 2 || texts[0] != "second" {
		t.Fatalf("messages after clear and a turn = %v, want [second two]", texts)
	}
	// Nothing was deleted: the journal still holds the first turn.
	recs, _ := j.Replay(ctx, "run-c")
	var sawFirst, sawClear bool
	for _, rec := range recs {
		sawFirst = sawFirst || (rec.Kind == RecordMessage && rec.Text == "first")
		sawClear = sawClear || rec.Kind == RecordClear
	}
	if !sawFirst || !sawClear {
		t.Fatalf("journal lost history (first=%v) or the marker (clear=%v)", sawFirst, sawClear)
	}
}

// Compact asks the agent for a summary and journals it; an agent that
// cannot compact is refused by name.
func TestCompactSummarisesOnDemand(t *testing.T) {
	t.Parallel()
	r, j, agent := controlsRunner(t)
	ctx := context.Background()

	if _, err := r.Start(ctx, "run-k", Input{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Compact(ctx, "run-k"); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if agent.compacted != 1 {
		t.Fatalf("agent compacted %d times, want 1", agent.compacted)
	}
	s, err := Restore(ctx, "run-k", j)
	if err != nil {
		t.Fatal(err)
	}
	if c := s.GetLastCompaction(); c == nil || c.Summary != "summary of everything so far" {
		t.Fatalf("last compaction = %+v", c)
	}

	plain := NewRunner(NewMemoryJournal(), fakeFactoryOnly())
	if _, err := plain.Start(ctx, "run-p", Input{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := plain.Compact(ctx, "run-p"); !errors.Is(err, ErrCompactionUnsupported) {
		t.Fatalf("Compact with a plain agent = %v, want ErrCompactionUnsupported", err)
	}
}
