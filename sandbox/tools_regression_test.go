package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/mark3labs/bonnie/runtime"
)

// The listing path must use the backend's directory mapping, not argv or a shell.
func TestListFilesUsesDirectoryNamespace(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "sub dir", "/workspace/sub dir", "-la", "dir; touch injected", "/tmp"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			sb := &listingSandbox{stubSandbox: &stubSandbox{id: "listing"}}
			tool := listFilesTool(func(context.Context) (Sandbox, error) { return sb, nil })
			data, err := json.Marshal(map[string]string{"path": input})
			if err != nil {
				t.Fatal(err)
			}
			res, err := tool.Run(context.Background(), fantasy.ToolCall{Input: string(data)})
			if err != nil || res.IsError {
				t.Fatalf("list_files: %v, %+v", err, res)
			}
			want := Command{Args: []string{"ls", "-la", "--", "."}, Dir: Resolve(input)}
			if !reflect.DeepEqual(sb.command, want) {
				t.Fatalf("command = %+v, want %+v", sb.command, want)
			}
		})
	}
}

type listingSandbox struct {
	*stubSandbox
	command Command
}

func (s *listingSandbox) Exec(_ context.Context, cmd Command) (*Result, error) {
	s.command = cmd
	return &Result{Stdout: "file.txt\n"}, nil
}

// A model can list the default work directory and paths with shell metacharacters
// on both host-mapped backends. No path can become a shell command.
func TestListFilesHostMappedBackends(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"local", "landlock"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p Provider = Local(WithLocalRoot(t.TempDir()), WithLocalCleanup())
			if name == "landlock" {
				p = landlockProvider(t)
			}
			sb := openSandbox(t, p, "list-files")
			ctx := testCtx(t)
			for _, file := range []string{"root-marker.txt", "sub dir/nested-marker.txt", "dir; touch injected/quoted-marker.txt", "-la/option-marker.txt"} {
				if err := sb.WriteFile(ctx, file, []byte("test")); err != nil {
					t.Fatal(err)
				}
			}
			tool := listFilesTool(func(context.Context) (Sandbox, error) { return sb, nil })
			for _, tc := range []struct{ input, want string }{
				{`{}`, "root-marker.txt"},
				{`{"path":"/workspace"}`, "root-marker.txt"},
				{`{"path":"sub dir"}`, "nested-marker.txt"},
				{`{"path":"/workspace/sub dir"}`, "nested-marker.txt"},
				{`{"path":"dir; touch injected"}`, "quoted-marker.txt"},
				{`{"path":"-la"}`, "option-marker.txt"},
			} {
				res, err := tool.Run(ctx, fantasy.ToolCall{Input: tc.input})
				if err != nil || res.IsError || !strings.Contains(res.Content, tc.want) {
					t.Fatalf("list_files(%s): %v, %+v", tc.input, err, res)
				}
			}
			if _, err := sb.ReadFile(ctx, "injected"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("shell marker: %v, want ErrNotFound", err)
			}
		})
	}
}

type retryOpenProvider struct {
	*stubProvider
	open func(context.Context, string) (Sandbox, error)
}

func (p *retryOpenProvider) Open(ctx context.Context, runID string) (Sandbox, error) {
	return p.open(ctx, runID)
}

// A canceled open must not poison the cache. Other callers wait safely, and
// only the first successful open and its journal record are retained.
func TestLazyOpenerConcurrentRetryAfterCanceledOpen(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	j := runtime.NewMemoryJournal()
	s := runtime.NewSession("retry-canceled", j)
	started := make(chan struct{})
	var attempts atomic.Int32
	base := newStubProvider("retry")
	p := &retryOpenProvider{stubProvider: base}
	p.open = func(ctx context.Context, runID string) (Sandbox, error) {
		if attempts.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return base.Open(ctx, runID)
	}
	open := LazyOpener(p, s)
	leaderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	leader := make(chan error, 1)
	go func() {
		_, err := open(leaderCtx)
		leader <- err
	}()
	<-started

	// A caller whose context expires must not wait for the active open.
	waitCtx, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, err := open(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting open: %v, want deadline exceeded", err)
	}
	const callers = 24
	results := make(chan Sandbox, callers)
	failures := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			sb, err := open(ctx)
			results <- sb
			failures <- err
		})
	}
	cancel()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("first open: %v, want canceled", err)
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	var first Sandbox
	for sb := range results {
		if first == nil {
			first = sb
		}
		if sb != first {
			t.Fatal("concurrent callers received different handles")
		}
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("open attempts = %d, want 2", got)
	}
	recs, err := j.Replay(ctx, s.RunID())
	if err != nil || len(recs) != 1 || recs[0].Kind != runtime.RecordSandbox {
		t.Fatalf("journal: %v, %+v", err, recs)
	}
}

// Seeded.Open can fail after the backend opens. LazyOpener must retry the
// seed, not retain that error or expose the work directory before the seed lands.
func TestLazyOpenerRetriesTransientSeedFailure(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	seed := filepath.Join(t.TempDir(), "seed")
	j := runtime.NewMemoryJournal()
	s := runtime.NewSession("retry-seed", j)
	p := Seeded(Local(WithLocalRoot(t.TempDir())), seed)
	open := LazyOpener(p, s)
	if sb, err := open(ctx); err == nil || sb != nil {
		t.Fatalf("missing seed: sandbox %v, error %v", sb, err)
	}
	if recs, err := j.Replay(ctx, s.RunID()); (err != nil && !errors.Is(err, runtime.ErrRunNotFound)) || len(recs) != 0 {
		t.Fatalf("failed open journal: %v, %+v", err, recs)
	}
	if err := os.Mkdir(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "seed.txt"), []byte("seed content"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := open(ctx)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	data, err := sb.ReadFile(ctx, "seed.txt")
	if err != nil || string(data) != "seed content" {
		t.Fatalf("seed: %q, %v", data, err)
	}
	if err := os.RemoveAll(seed); err != nil {
		t.Fatal(err)
	}
	if cached, err := open(ctx); err != nil || cached != sb {
		t.Fatalf("cached open: %v, %v", cached, err)
	}
}
