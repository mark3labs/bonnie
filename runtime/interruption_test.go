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

// A second Runner restores unsaved input from the latest turn, even when an
// older turn used the same text. Caller-supplied files remain part of the input.
func TestInterruptedRecoveryUsesLatestTurnAndFilesAcrossRunner(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	ctx := context.Background()
	firstFactory, _ := fakeFactory(&kit.TurnResult{Response: "first done"})
	if _, err := NewRunner(j, firstFactory).Start(ctx, "repeat", Input{Text: "same prompt"}); err != nil {
		t.Fatal(err)
	}

	blocked := &fakeAgent{started: make(chan struct{}), block: make(chan struct{})}
	blockingRunner := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) {
		blocked.session = s
		return blocked, nil
	})
	done := make(chan *Run, 1)
	go func() {
		run, _ := blockingRunner.Start(ctx, "repeat", Input{Text: "same prompt"})
		done <- run
	}()
	<-blocked.started
	if err := blockingRunner.Interrupt("repeat"); err != nil {
		t.Fatal(err)
	}
	select {
	case run := <-done:
		if run == nil || run.State != RunInterrupted {
			t.Fatalf("interrupted run = %#v", run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt did not stop run")
	}

	file := kit.LLMFilePart{MediaType: "text/plain", Data: []byte("attachment")}
	recovered := &fileRecordingAgent{fakeAgent: &fakeAgent{turns: []*kit.TurnResult{{Response: "recovered"}}}}
	runner := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) {
		recovered.session = s
		return recovered, nil
	})
	got, err := runner.Start(ctx, "repeat", Input{Text: "same prompt", Files: []kit.LLMFilePart{file}})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RunCompleted || got.Response != "recovered" {
		t.Fatalf("recovered run = %#v", got)
	}
	if len(recovered.files) != 1 || string(recovered.files[0].Data) != "attachment" {
		t.Fatalf("recovery files = %#v", recovered.files)
	}
	recs, err := j.Replay(ctx, "repeat")
	if err != nil {
		t.Fatal(err)
	}
	var prompts int
	for _, rec := range recs {
		if rec.Kind == RecordMessage && rec.Role == "user" && rec.Text == "same prompt" {
			prompts++
		}
	}
	if prompts != 2 {
		t.Fatalf("prompt records = %d, want one per turn", prompts)
	}
}

type fileRecordingAgent struct {
	*fakeAgent
	files []kit.LLMFilePart
}

func (f *fileRecordingAgent) PromptResultWithFiles(ctx context.Context, msg string, files []kit.LLMFilePart) (*kit.TurnResult, error) {
	f.files = append([]kit.LLMFilePart(nil), files...)
	return f.PromptResult(ctx, msg)
}
