package runtime

import (
	"context"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestInterruptPreservesRunAndStartContinuesWithoutPromptDuplication(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	first := &fakeAgent{started: make(chan struct{}), block: make(chan struct{})}
	second := &fakeAgent{turns: []*kit.TurnResult{{Response: "continued"}}}
	calls := 0
	factory := func(_ context.Context, s *Session) (Agent, error) {
		calls++
		if calls == 1 {
			first.session = s
			return first, nil
		}
		second.session = s
		return second, nil
	}
	r := NewRunner(j, factory)
	done := make(chan *Run, 1)
	go func() {
		run, _ := r.Start(context.Background(), "interrupt", Input{Text: "original prompt"})
		done <- run
	}()
	<-first.started
	if err := r.Interrupt("interrupt"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got == nil || got.State != RunInterrupted {
			t.Fatalf("run = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt did not stop run")
	}
	recs, err := j.Replay(context.Background(), "interrupt")
	if err != nil {
		t.Fatal(err)
	}
	before := len(recs)
	continued, err := r.Start(context.Background(), "interrupt", Input{Text: "original prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if continued.State != RunCompleted {
		t.Fatalf("state = %s", continued.State)
	}
	recs, err = j.Replay(context.Background(), "interrupt")
	if err != nil {
		t.Fatal(err)
	}
	prompts := 0
	for _, rec := range recs {
		if rec.Kind == RecordMessage && rec.Role == "user" && rec.Text == "original prompt" {
			prompts++
		}
	}
	if prompts != 1 {
		t.Fatalf("original prompt records = %d (records before resume %d)", prompts, before)
	}
	if continued.Response != "continued" {
		t.Fatalf("response = %q", continued.Response)
	}
}
