package sandbox

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// Count handle cleanup separately from provider opens. Exec also identifies
// the handle shared by custom setup execution and the model's shell tool.
type setupSandbox struct {
	Sandbox
	closed int
	execs  int
	err    error
}

func (s *setupSandbox) Close() error {
	s.closed++
	return s.err
}

func (s *setupSandbox) Exec(context.Context, Command) (*Result, error) {
	s.execs++
	return &Result{}, nil
}

type setupProvider struct {
	*stubProvider
	handle *setupSandbox
}

func (p *setupProvider) Open(ctx context.Context, runID string) (Sandbox, error) {
	sb, err := p.stubProvider.Open(ctx, runID)
	if err != nil {
		return nil, err
	}
	p.handle.Sandbox = sb
	return p.handle, nil
}

func setupKitOptions(model *fakemodel.Model) []kit.Option {
	return []kit.Option{model.Option(), func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.Quiet = true, true, true, true, true, true
	}}
}

// Setup execution and shell must use one handle and write one open record.
func TestAgentWithSetupSharesSandbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := runtime.NewMemoryJournal()
	s := runtime.NewSession("shared-setup", journal)
	p := &setupProvider{stubProvider: newStubProvider("stub"), handle: &setupSandbox{}}
	model := fakemodel.New(fakemodel.Call("shell", `{"command":"echo model"}`), fakemodel.Say("done"))
	calls := 0
	factory := AgentWithSetup(p, false, func(gotCtx context.Context, k *kit.Kit, gotSession *runtime.Session, open Opener) error {
		calls++
		if gotCtx != ctx || gotSession != s || k.GetSessionManager() != s || p.opened != 0 {
			t.Fatal("setup received wrong context/session or opened sandbox early")
		}
		sb, err := open(ctx)
		if err != nil {
			return err
		}
		_, err = sb.Exec(ctx, Command{})
		return err
	}, setupKitOptions(model)...)
	a, err := factory(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	_, promptErr := a.PromptResult(ctx, "run shell")
	closeErr := a.Close()
	if promptErr != nil || closeErr != nil {
		t.Fatalf("prompt: %v; close: %v", promptErr, closeErr)
	}
	if calls != 1 || p.opened != 1 || p.handle.execs != 3 || p.handle.closed != 1 {
		t.Fatalf("setup=%d opens=%d execs=%d closes=%d", calls, p.opened, p.handle.execs, p.handle.closed)
	}
	recs, err := journal.Replay(ctx, s.RunID())
	if err != nil {
		t.Fatal(err)
	}
	opens := 0
	for _, rec := range recs {
		if rec.Kind == runtime.RecordSandbox {
			opens++
		}
	}
	if opens != 1 {
		t.Fatalf("sandbox records = %d, want 1", opens)
	}
}

// Cleanup must run even when setup fails after it opens a sandbox. A setup
// that never uses its opener must not start compute, even during cleanup.
func TestAgentWithSetupCleanup(t *testing.T) {
	t.Parallel()
	for _, openDuringSetup := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			p := &setupProvider{stubProvider: newStubProvider("stub"), handle: &setupSandbox{}}
			setupErr := errors.New("setup failed")
			closeErr := errors.New("sandbox close failed")
			if fail {
				p.handle.err = closeErr
			}
			factory := AgentWithSetup(p, true, func(ctx context.Context, _ *kit.Kit, _ *runtime.Session, open Opener) error {
				if openDuringSetup {
					if _, err := open(ctx); err != nil {
						return err
					}
				}
				if fail {
					return setupErr
				}
				return nil
			}, setupKitOptions(fakemodel.New())...)
			a, err := factory(context.Background(), runtime.NewSession("cleanup", runtime.NewMemoryJournal()))
			if fail {
				if a != nil || !errors.Is(err, setupErr) || errors.Is(err, closeErr) != openDuringSetup {
					t.Fatalf("agent = %v, error = %v", a, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := a.Close(); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if openDuringSetup {
				want = 1
			}
			if p.opened != want || p.handle.closed != want {
				t.Fatalf("opens=%d closes=%d, want %d", p.opened, p.handle.closed, want)
			}
		}
	}
}

func TestAgentWithSetupConstructionFailure(t *testing.T) {
	t.Parallel()
	p := &setupProvider{stubProvider: newStubProvider("stub"), handle: &setupSandbox{}}
	called := false
	opts := []kit.Option{func(o *kit.Options) {
		o.SkipConfig, o.NoSkills, o.Quiet = true, true, true
	}, kit.WithModel("invalid-provider/model")}
	factory := AgentWithSetup(p, false, func(context.Context, *kit.Kit, *runtime.Session, Opener) error {
		called = true
		return nil
	}, opts...)
	a, err := factory(context.Background(), runtime.NewSession("failure", runtime.NewMemoryJournal()))
	if a != nil || err == nil || called || p.opened != 0 {
		t.Fatalf("agent=%v error=%v setup=%v opens=%d", a, err, called, p.opened)
	}
}
