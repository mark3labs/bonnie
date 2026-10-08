package bonnie

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/log"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// A second scope has a new runner and factory. Only the journal supplies history.
func TestScopedRuntimeDurability(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var previous *runtime.Runner
	for turn := range 2 {
		a := New(WithJournal(root), WithContextFiles(""), WithAgentFactory(func(ctx context.Context, s *runtime.Session) (runtime.Agent, error) {
			messages := s.GetMessages()
			if len(messages) != turn*2 {
				t.Fatalf("turn %d has %d messages: %v", turn, len(messages), messages)
			}
			return &scopedAgent{session: s}, nil
		}))
		if err := a.WithRuntime(t.Context(), func(ctx context.Context, rt *Runtime) error {
			if rt.Runner() == previous || rt.Runner().Journal() != rt.Journal() || !rt.Journal().Persisted() {
				t.Fatal("scope did not construct a new durable runner")
			}
			previous = rt.Runner()
			run, err := rt.Runner().Start(ctx, "durable", runtime.Input{Text: "hello"})
			if err == nil && (run.State != runtime.RunCompleted || run.Response != "answer") {
				t.Fatalf("run = %+v", run)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := New(WithJournal(root)).WithJournal(t.Context(), func(ctx context.Context, j runtime.Journal) error {
		state, err := j.State(ctx, "durable")
		if err == nil && state != runtime.RunCompleted {
			t.Fatalf("state = %s", state)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type scopedAgent struct {
	stubAgent
	session *runtime.Session
}

func (a *scopedAgent) PromptResult(_ context.Context, prompt string) (*kit.TurnResult, error) {
	if _, err := a.session.AppendMessage(kit.LLMMessage{Role: kit.LLMRoleUser, Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: prompt}}}); err != nil {
		return nil, err
	}
	_, err := a.session.AppendMessage(kit.LLMMessage{Role: kit.LLMRoleAssistant, Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "answer"}}})
	return &kit.TurnResult{Response: "answer"}, err
}

// Inspection must work even when execution configuration cannot apply.
func TestScopedJournalInspectionOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	provider := &selectionProvider{t: t, name: "unused", availableErr: errors.New("must not check provider")}
	for _, factory := range []bool{false, true} {
		a := New(WithJournal(filepath.Join(root, "journal")), WithInstructions(filepath.Join(root, "missing")),
			WithContextFiles(filepath.Join(root, "context")), WithSkills(filepath.Join(root, "skills")),
			WithSandbox(provider), WithModel("invalid-model"))
		if factory {
			a.Configure(WithAgentFactory(func(context.Context, *runtime.Session) (runtime.Agent, error) {
				t.Fatal("inspection called factory")
				return stubAgent{}, nil
			}))
		}
		if err := a.WithJournal(t.Context(), func(ctx context.Context, j runtime.Journal) error {
			_, err := j.Runs(ctx, "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if provider.availableCalls != 0 {
		t.Fatal("inspection checked provider")
	}
	for _, name := range []string{"context", "skills"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspection created %s: %v", name, err)
		}
	}
}

// Nil callbacks and cancelled contexts must fail before setup has side effects.
func TestScopedOperationsRejectBeforeSetup(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"journal", "runtime"} {
		for _, nilCallback := range []bool{false, true} {
			t.Run(operation+map[bool]string{false: "/cancelled", true: "/nil"}[nilCallback], func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				a := New(WithJournal(filepath.Join(root, "journal")), WithContextFiles(filepath.Join(root, "context")))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if !nilCallback {
					cancel()
				}
				var err error
				if operation == "journal" {
					var fn func(context.Context, runtime.Journal) error
					if !nilCallback {
						fn = func(context.Context, runtime.Journal) error { t.Fatal("callback called"); return nil }
					}
					err = a.WithJournal(ctx, fn)
				} else {
					var fn func(context.Context, *Runtime) error
					if !nilCallback {
						fn = func(context.Context, *Runtime) error { t.Fatal("callback called"); return nil }
					}
					err = a.WithRuntime(ctx, fn)
				}
				if err == nil || (!nilCallback && !errors.Is(err, context.Canceled)) {
					t.Fatalf("error = %v", err)
				}
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Fatalf("setup created files: %v, %v", entries, err)
				}
			})
		}
	}
}

// Both scopes close their journal even when the callback fails or cancels.
func TestScopedOperationsCleanup(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"journal", "runtime"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			a := New(WithJournal(t.TempDir()), WithAgentFactory(stubFactory), WithContextFiles(""))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New("callback failed")
			var closed runtime.Journal
			callback := func(got context.Context, j runtime.Journal) error {
				if got != ctx {
					t.Fatal("callback lost context")
				}
				closed = j
				cancel()
				return want
			}
			var err error
			if operation == "journal" {
				err = a.WithJournal(ctx, callback)
			} else {
				err = a.WithRuntime(ctx, func(ctx context.Context, rt *Runtime) error { return callback(ctx, rt.Journal()) })
			}
			if !errors.Is(err, want) || !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
			// Deliberate out-of-scope access verifies cleanup, not supported use.
			if _, err := closed.Runs(context.Background(), ""); err == nil {
				t.Fatal("journal stayed open")
			}
		})
	}
}

// Cleanup errors must not replace callback errors.
func TestScopedCleanupJoinsErrors(t *testing.T) {
	t.Parallel()
	callbackErr, closeErr := errors.New("callback"), errors.New("close")
	j := &scopedCloseJournal{closeErr: closeErr}
	err := closeJournal(j, callbackErr)
	if !errors.Is(err, callbackErr) || !errors.Is(err, closeErr) || j.calls != 1 {
		t.Fatalf("error = %v, closes = %d", err, j.calls)
	}
}

type scopedCloseJournal struct {
	runtime.Journal
	closeErr error
	calls    int
}

func (j *scopedCloseJournal) Close() error { j.calls++; return j.closeErr }

// Scoped execution does not construct channels, touch a listener, or schedule submissions.
func TestScopedRuntimeNoServerOrScheduler(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
	}()
	a := New(WithJournal(t.TempDir()), WithContextFiles(""), WithAgentFactory(func(context.Context, *runtime.Session) (runtime.Agent, error) {
		t.Fatal("scheduler called factory")
		return stubAgent{}, nil
	}), WithAddr("invalid"), WithListener(ln), WithChannel(func(*runtime.Runner) (Channel, error) {
		t.Fatal("scope constructed a channel")
		return nil, nil
	}))
	if err := a.WithRuntime(t.Context(), func(ctx context.Context, rt *Runtime) error {
		_, err := rt.Runner().Submit(ctx, "queued", "request", runtime.Input{Text: "wait"}, runtime.BusyQueue)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.WithRuntime(t.Context(), func(ctx context.Context, rt *Runtime) error {
		items, err := rt.Runner().Submissions(ctx, "queued")
		if err == nil && (len(items) != 1 || items[0].State != runtime.SubmissionQueued) {
			t.Fatalf("submissions = %+v", items)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A still-bound listener cannot be bound again. Scopes must not close it.
	probe, err := net.Listen("tcp", ln.Addr().String())
	if err == nil {
		if closeErr := probe.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("scope closed the configured listener")
	}
}

// Configure applies to scoped execution, including journal, seed, factory and logging.
func TestScopedRuntimeConfiguration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var output bytes.Buffer
	a := New(WithJournal(filepath.Join(root, "unused")), WithContextFiles(""))
	a.Configure(WithJournal(filepath.Join(root, "selected")), WithContextFiles(filepath.Join(root, "seed")),
		WithActivityLogger(NewActivityLogger(log.New(&output))), WithAgentFactory(func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
			return &scopedAgent{session: s}, nil
		}))
	if err := a.WithRuntime(t.Context(), func(ctx context.Context, rt *Runtime) error {
		_, err := rt.Runner().Start(ctx, "configured", runtime.Input{Text: "hello"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"selected/journal.db", "seed"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "unused")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused journal exists: %v", err)
	}
	if !strings.Contains(output.String(), "configured") {
		t.Fatalf("activity logger not applied: %s", &output)
	}
}

// Runtime preparation retains Run's validation and fails before journal opening.
func TestScopedRuntimePreparationFailure(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "journal")
	a := New(WithJournal(root), WithContextFiles(""), WithAgentFactory(stubFactory), WithModel("conflict"))
	err := a.WithRuntime(t.Context(), func(context.Context, *Runtime) error { t.Fatal("callback called"); return nil })
	if err == nil || !strings.Contains(err.Error(), "WithModel") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal opened on setup failure: %v", err)
	}
}

// Inspection must not read even a malformed dotenv file. Runtime must read it.
func TestScopedJournalDoesNotLoadDotenv(t *testing.T) {
	// Chdir changes process state, so this test cannot run in parallel.
	t.Chdir(t.TempDir())
	if err := os.Mkdir(DefaultDotenv, 0o755); err != nil {
		t.Fatal(err)
	}
	a := New(WithJournal("journal"), WithContextFiles(""), WithAgentFactory(stubFactory))
	if err := a.WithJournal(t.Context(), func(ctx context.Context, j runtime.Journal) error {
		_, err := j.Runs(ctx, "")
		return err
	}); err != nil {
		t.Fatalf("inspection read dotenv: %v", err)
	}
	if err := a.WithRuntime(t.Context(), func(context.Context, *Runtime) error {
		t.Fatal("runtime ignored invalid dotenv")
		return nil
	}); err == nil || !strings.Contains(err.Error(), DefaultDotenv) {
		t.Fatalf("runtime error = %v", err)
	}
}

// Cancellation while a provider check succeeds must prevent journal opening.
func TestScopedRuntimeCancellationDuringPreparation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	provider := &scopedCancellingProvider{cancel: cancel}
	provider.t, provider.name = t, "cancel"
	a := New(WithJournal(filepath.Join(root, "journal")), WithInstructions(""), WithSkills(""),
		WithContextFiles(""), WithSandbox(provider))
	err := a.WithRuntime(ctx, func(context.Context, *Runtime) error { t.Fatal("callback called"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "journal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal opened after cancellation: %v", err)
	}
}

type scopedCancellingProvider struct {
	selectionProvider
	cancel context.CancelFunc
}

func (p *scopedCancellingProvider) Available(context.Context) error {
	p.cancel()
	return nil
}

// The shared runner option helper must retain the completion continuation limit.
func TestScopedRunnerCompletionLimit(t *testing.T) {
	t.Parallel()
	c := resolve(WithJournal(t.TempDir()), WithCompletionHook(CompletionPolicy{MaxContinuations: 1}))
	rt, err := c.openRuntime(func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		agent := &scopedCompletionAgent{}
		agent.session = s
		return agent, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeJournal(rt.Journal(), nil); err != nil {
			t.Error(err)
		}
	}()
	_, err = rt.Runner().Start(t.Context(), "completion", runtime.Input{Text: "hello"})
	if err != nil {
		t.Fatalf("configured continuation was refused: %v", err)
	}
}

type scopedCompletionAgent struct {
	scopedAgent
	checks int
}

func (a *scopedCompletionAgent) Complete(context.Context, runtime.CompletionCandidate) (runtime.CompletionFeedback, error) {
	a.checks++
	if a.checks == 1 {
		return runtime.CompletionFeedback{ContinueWith: "continue"}, nil
	}
	return runtime.CompletionFeedback{}, nil
}
