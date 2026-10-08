package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Recovery must not repeat an external action whose completion is unknown.
// A new Kit and session share only a reopened journal.
func TestToolIntentRecoveryAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, safe := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsafe", true: "safe"}[safe], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			j, err := OpenSQLiteJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			s := NewSession("r", j)
			if err := appendToolRecord(ctx, s, RecordToolIntent, toolIntent{ID: "call-1", Name: "action", Args: `{}`, Safe: safe}); err != nil {
				t.Fatal(err)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			j, err = OpenSQLiteJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := j.Close(); err != nil {
					t.Error(err)
				}
			}()
			calls := 0
			tool := kit.NewTool("action", "action", func(context.Context, struct{}) (kit.ToolOutput, error) { calls++; return kit.TextResult("saved"), nil })
			model := fakemodel.New(fakemodel.Say("done"))
			factory := KitAgentWithSetup(true, func(_ context.Context, _ *kit.Kit, s *Session) error {
				if safe {
					s.SetReplaySafeTools("action")
				}
				return nil
			}, hermetic(), model.Option(), kit.WithTools(tool))
			r := NewRunner(j, factory)
			if _, err := r.Start(ctx, "r", Input{Text: "continue"}); err != nil {
				t.Fatal(err)
			}
			want := 0
			if safe {
				want = 1
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
			restored, err := Restore(ctx, "r", j)
			if err != nil {
				t.Fatal(err)
			}
			text := ""
			for _, msg := range restored.GetMessages() {
				text += messageText(msg)
				for _, part := range msg.Content {
					if result, ok := part.(kit.LLMToolResultPart); ok {
						value, _ := toolResultText(result.Output)
						text += value
						if e, ok := result.Output.(kit.LLMToolResultOutputContentError); ok && e.Error != nil {
							text += e.Error.Error()
						}
					}
				}
			}
			if !safe && !strings.Contains(text, "external effect is unknown") {
				t.Fatalf("missing interrupted result: %s", text)
			}
			// A third process must not replay a recovered call again.
			r = NewRunner(j, scriptedKit(fakemodel.New(fakemodel.Say("done")), kit.WithTools(tool)))
			if _, err := r.Start(ctx, "r", Input{Text: "next"}); err != nil {
				t.Fatal(err)
			}
			if calls != want {
				t.Fatalf("repeated recovered call: %d", calls)
			}
		})
	}
}

func startTestScheduler(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.RunScheduler(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("scheduler did not stop")
		}
	})
}

// Admission and request-ID deduplication survive closing and reopening SQLite.
func TestSubmissionQueueAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	j, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := fakeFactory()
	r := NewRunner(j, f)
	first, err := r.Submit(ctx, "r", "one", Input{Text: "first"}, BusyQueue)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Submit(ctx, "r", "two", Input{Text: "second"}, BusyQueue)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := r.Submit(ctx, "r", "three", Input{Text: "withdraw"}, BusyQueue)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AbortSubmission(ctx, "r", removed.ID); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	f, _ = fakeFactory()
	r = NewRunner(j, f)
	again, err := r.Submit(ctx, "r", "one", Input{Text: "duplicate"}, BusyQueue)
	if err != nil || again.ID != first.ID {
		t.Fatalf("duplicate=%+v err=%v", again, err)
	}
	startTestScheduler(t, r)
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, id := range []string{first.ID, second.ID} {
		item, err := r.WaitSubmission(wait, "r", id)
		if err != nil || item.State != SubmissionDone {
			t.Fatalf("settled=%+v err=%v", item, err)
		}
	}
	s, err := Restore(ctx, "r", j)
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, m := range s.GetMessages() {
		if string(m.Role) == "user" {
			users = append(users, messageText(m))
		}
	}
	if strings.Join(users, ",") != "first,second" {
		t.Fatalf("inputs=%v", users)
	}
}

// Child identity and admitted work survive a new runner, and background work
// is outside normal parent cancellation.
func TestOwnedChildrenAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	f, _ := fakeFactory()
	r := NewRunner(j, f)
	if err := j.Checkpoint(ctx, "parent", RunRunning); err != nil {
		t.Fatal(err)
	}
	child, err := r.SpawnChild(ctx, "parent", "stable", Input{Text: "work"}, false)
	if err != nil {
		t.Fatal(err)
	}
	bg, err := r.SpawnChild(ctx, "parent", "background", Input{Text: "later"}, true)
	if err != nil {
		t.Fatal(err)
	}
	f, _ = fakeFactory()
	r = NewRunner(j, f)
	again, err := r.SpawnChild(ctx, "parent", "stable", Input{Text: "duplicate"}, false)
	if err != nil || again.ID != child.ID {
		t.Fatalf("child=%+v err=%v", again, err)
	}
	if err := r.CancelOwned(ctx, "parent", false); err != nil {
		t.Fatal(err)
	}
	state, err := j.State(ctx, child.ID)
	if err != nil || state != RunCancelled {
		t.Fatalf("child state=%s err=%v", state, err)
	}
	items, err := r.Submissions(ctx, bg.ID)
	if err != nil || items[0].State != SubmissionQueued {
		t.Fatalf("background=%+v err=%v", items, err)
	}
	startTestScheduler(t, r)
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	item, err := r.WaitSubmission(wait, bg.ID, items[0].ID)
	if err != nil || item.State != SubmissionDone {
		t.Fatalf("background=%+v err=%v", item, err)
	}
}
