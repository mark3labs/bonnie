package bonnie

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

func TestRunSandboxCleanupValidation(t *testing.T) {
	t.Parallel()
	policy := WithRunSandboxCleanup(SandboxCleanupPolicy{CompletedAfter: time.Hour})
	for name, opts := range map[string][]Option{
		"persistent":          {policy, WithSharedDirectory(t.TempDir())},
		"persistent-reversed": {WithSharedDirectory(t.TempDir()), policy},
		"factory":             {policy, WithAgentFactory(stubFactory)},
		"shared":              {policy, WithSandbox(sandbox.Local(sandbox.WithLocalSharedDirectory()))},
		"negative":            {WithRunSandboxCleanup(SandboxCleanupPolicy{CompletedAfter: -time.Second})},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := resolve(opts...).agentFactory(context.Background(), "", nil); err == nil {
				t.Fatal("invalid cleanup configuration accepted")
			}
		})
	}
	cfg := resolve(policy)
	if _, err := cfg.agentFactory(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.cleanupProvider == nil {
		t.Fatal("default provider not retained for cleanup")
	}
}

// The startup sweep uses the selected provider root, keeps the provider root,
// writes a durable receipt, and stops when its caller cancels the context.
func TestRunSandboxCleanupLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	provider := sandbox.Local(sandbox.WithLocalRoot(root))
	cfg := resolve(WithSandbox(provider), WithRunSandboxCleanup(SandboxCleanupPolicy{CompletedAfter: time.Nanosecond}))
	if err := cfg.configureSandboxCleanup(provider); err != nil {
		t.Fatal(err)
	}
	sb, err := provider.Open(ctx, "task")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(ctx, "clone.txt", []byte("repo")); err != nil {
		t.Fatal(err)
	}
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	j := runtime.NewMemoryJournal()
	if _, err := j.Append(ctx, runtime.Record{RunID: "task", Kind: runtime.RecordState, State: runtime.RunCompleted, Timestamp: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	runner := runtime.NewRunner(j, nil)
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); cfg.cleanupLoop(loopCtx, runner, time.Millisecond) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(5 * time.Second)
	for {
		recs, err := j.Replay(ctx, "task")
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) > 1 && recs[len(recs)-1].Kind == runtime.RecordSandboxDeleted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("startup sweep did not complete")
		case <-time.After(time.Millisecond):
		}
	}
	exists, err := provider.SandboxExists(ctx, "task")
	if err != nil || exists {
		t.Fatalf("contextFiles exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(filepath.Clean(root)); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupUnsupportedProvider(t *testing.T) {
	t.Parallel()
	cfg := resolve(WithRunSandboxCleanup(SandboxCleanupPolicy{}))
	if err := cfg.configureSandboxCleanup(sandbox.Seeded(cleanupNoDeleteProvider{}, t.TempDir())); err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("error = %v", err)
	}
}

type cleanupNoDeleteProvider struct{}

func (cleanupNoDeleteProvider) Name() string                    { return "no-delete" }
func (cleanupNoDeleteProvider) Available(context.Context) error { return nil }
func (cleanupNoDeleteProvider) Open(context.Context, string) (sandbox.Sandbox, error) {
	panic("must not open")
}
