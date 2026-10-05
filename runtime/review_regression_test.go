package runtime

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// A failed write must not leave a tree entry that Branch can select.
func TestSessionAppendFailureDoesNotPublishEntry(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"message", "step", "compaction", "extension", "model", "summary"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			j := &reviewFailJournal{MemoryJournal: NewMemoryJournal()}
			s := NewSession("r", j)
			root, err := s.AppendMessage(kit.NewLLMUserMessage("root"))
			if err != nil {
				t.Fatal(err)
			}
			j.fail = true
			var id string
			switch method {
			case "message":
				id, err = s.AppendMessage(kit.NewLLMUserMessage("bad"))
			case "step":
				var ids []string
				ids, err = s.AppendStep(t.Context(), []kit.LLMMessage{kit.NewLLMUserMessage("bad")})
				id = ids[0]
			case "compaction":
				id, err = s.AppendCompaction("bad", root, 1, 1, 0, nil, nil)
			case "extension":
				id, err = s.AppendExtensionData("bad", "bad")
			case "model":
				id, err = s.AppendModelChange("bad", "bad")
			case "summary":
				id, err = s.AppendBranchSummary(root, "bad")
			}
			if err == nil {
				t.Fatal("write succeeded")
			}
			if s.leaf != root {
				t.Fatal("failed write changed branch")
			}
			if err := s.Branch(id); !errors.Is(err, ErrEntryNotFound) {
				t.Fatalf("branch to failed entry: %v", err)
			}
			if s.LastMessageSeq() != 1 {
				t.Fatal("failed write changed anchor")
			}
		})
	}
}

type reviewFailJournal struct {
	*MemoryJournal
	fail bool
}

func (j *reviewFailJournal) Append(ctx context.Context, rec Record) (int, error) {
	if j.fail {
		return 0, errors.New("write failed")
	}
	return j.MemoryJournal.Append(ctx, rec)
}

func (j *reviewFailJournal) AppendStep(ctx context.Context, recs []Record) ([]int, error) {
	if j.fail {
		return nil, errors.New("write failed")
	}
	return j.MemoryJournal.AppendStep(ctx, recs)
}

// A concurrent turn must not combine old state with newer suspension records.
func TestRunnerSnapshotUsesReplayState(t *testing.T) {
	t.Parallel()
	j := &reviewStaleStateJournal{MemoryJournal: NewMemoryJournal()}
	if err := j.Checkpoint(t.Context(), "r", RunWaiting); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(t.Context(), Record{RunID: "r", Kind: RecordSuspend, Payload: encodeSuspend(SuspendRequest{Prompt: "approve?"})}); err != nil {
		t.Fatal(err)
	}
	got, err := NewRunner(j, nil).Snapshot(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RunWaiting || got.Suspend == nil || got.Suspend.Prompt != "approve?" {
		t.Fatalf("snapshot = %+v", got)
	}
}

type reviewStaleStateJournal struct{ *MemoryJournal }

func (j *reviewStaleStateJournal) State(context.Context, string) (RunState, error) {
	return RunRunning, nil
}

// Branch must not write ahead of an append that has selected its parent.
// The tree lock covers the durable write, so replay sees the same order.
func TestSessionBranchWaitsForAppendCommit(t *testing.T) {
	t.Parallel()
	j := &reviewBlockedJournal{MemoryJournal: NewMemoryJournal(), entered: make(chan struct{}), release: make(chan struct{})}
	s := NewSession("r", j)
	root, err := s.AppendMessage(kit.NewLLMUserMessage("root"))
	if err != nil {
		t.Fatal(err)
	}
	j.block = true
	done := make(chan error, 1)
	go func() {
		_, err := s.AppendMessage(kit.NewLLMUserMessage("next"))
		done <- err
	}()
	<-j.entered
	unlocked := s.mu.TryLock()
	if unlocked {
		s.mu.Unlock()
	}
	close(j.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if unlocked {
		t.Fatal("append released tree lock before its durable write")
	}
	if err := s.Branch(root); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(t.Context(), "r", j)
	if err != nil {
		t.Fatal(err)
	}
	if restored.leaf != s.leaf || len(restored.GetMessages()) != 1 {
		t.Fatal("replay disagrees with selected branch")
	}
}

type reviewBlockedJournal struct {
	*MemoryJournal
	block            bool
	entered, release chan struct{}
}

func (j *reviewBlockedJournal) Append(ctx context.Context, rec Record) (int, error) {
	if j.block {
		j.block = false
		close(j.entered)
		<-j.release
	}
	return j.MemoryJournal.Append(ctx, rec)
}
