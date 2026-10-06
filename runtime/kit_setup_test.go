package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Setup must see the managed session and tools before the first prompt.
// A fresh build must run setup again, not reuse the previous Kit instance.
func TestKitAgentWithSetup(t *testing.T) {
	t.Parallel()
	for _, humanInput := range []bool{true, false} {
		ctx := context.Background()
		journal := NewMemoryJournal()
		s := NewSession("setup", journal)
		model := fakemodel.New(fakemodel.Say("first"), fakemodel.Say("second"))
		calls := 0
		var previous *kit.Kit
		factory := KitAgentWithSetup(humanInput, func(gotCtx context.Context, k *kit.Kit, gotSession *Session) error {
			calls++
			if gotCtx != ctx || gotSession != s || k.GetSessionManager() != s {
				t.Fatal("setup did not receive the factory context and managed session")
			}
			if k == previous {
				t.Fatal("factory reused the previous Kit")
			}
			previous = k
			for _, name := range []string{"ask_human", "request_approval"} {
				if slices.Contains(k.GetToolNames(), name) != humanInput {
					t.Fatalf("tool %s does not match humanInput=%v", name, humanInput)
				}
			}
			return nil
		}, hermetic(), model.Option())
		for range 2 {
			a, err := factory(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			_, promptErr := a.PromptResult(ctx, "hello")
			closeErr := a.Close()
			if promptErr != nil || closeErr != nil {
				t.Fatalf("prompt: %v; close: %v", promptErr, closeErr)
			}
		}
		if calls != 2 {
			t.Fatalf("setup calls = %d, want 2", calls)
		}
		recs, err := journal.Replay(ctx, s.RunID())
		if err != nil {
			t.Fatal(err)
		}
		steps := 0
		for _, rec := range recs {
			if rec.Kind == RecordStep {
				steps++
			}
		}
		if steps != 2 {
			t.Fatalf("checkpoints = %d, want 2", steps)
		}
	}
}

// A session close probe makes Kit cleanup observable without a live provider.
type setupCloseSession struct {
	*Session
	closed int
	err    error
}

func (s *setupCloseSession) Close() error {
	s.closed++
	return s.err
}

func TestKitAgentSetupFailureClosesKit(t *testing.T) {
	t.Parallel()
	setupErr := errors.New("setup failed")
	closeErr := errors.New("close failed")
	s := NewSession("failure", NewMemoryJournal())
	probe := &setupCloseSession{Session: s, err: closeErr}
	factory := KitAgentWithSetup(false, func(_ context.Context, k *kit.Kit, _ *Session) error {
		k.SetSessionManager(probe)
		return setupErr
	}, hermetic(), fakemodel.New().Option())
	a, err := factory(context.Background(), s)
	if a != nil || !errors.Is(err, setupErr) {
		t.Fatalf("agent = %v, error = %v", a, err)
	}
	if probe.closed != 1 {
		t.Fatalf("Kit session closes = %d, want 1", probe.closed)
	}
}

func TestKitAgentConstructionFailureSkipsSetup(t *testing.T) {
	t.Parallel()
	called := false
	factory := KitAgentWithSetup(false, func(context.Context, *kit.Kit, *Session) error {
		called = true
		return nil
	}, hermetic(), kit.WithModel("invalid-provider/model"))
	a, err := factory(context.Background(), NewSession("failure", NewMemoryJournal()))
	if a != nil || err == nil || called {
		t.Fatalf("agent = %v, error = %v, setup called = %v", a, err, called)
	}
}
