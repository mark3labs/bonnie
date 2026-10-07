package bonnie

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// Disable all host discovery. These tests must not use a user's Kit settings.
func completionKitOptions(model *fakemodel.Model) []kit.Option {
	return []kit.Option{model.Option(), func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills = true, true, true
		o.NoExtensions, o.NoAgents, o.Quiet = true, true, true
	}}
}

// Setup, model tools, and completion checks must share one sandbox handle.
// Only the accepted response is final, and usage includes every model step.
func TestManagedCompletionSharesSandbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := runtime.NewMemoryJournal()
	model := fakemodel.New(
		fakemodel.Call("shell", `{"command":"cat setup.txt; printf model > model.txt"}`),
		fakemodel.Say("draft"), fakemodel.Say("accepted"),
	)
	var order []string
	var candidates []CompletionCandidate
	cfg := resolve(
		WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		WithKit(completionKitOptions(model)...), WithoutHumanInput(),
		WithKitSetup(func(ctx context.Context, k *kit.Kit, scope RunScope) error {
			order = append(order, "setup one")
			if scope.RunID != "completion-shared" || k.GetSessionManager() == nil {
				return fmt.Errorf("wrong run scope or missing durable session")
			}
			res, err := scope.Exec(ctx, sandbox.Shell("printf setup > setup.txt"))
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("setup command failed: %+v", res)
			}
			return nil
		}),
		WithKitSetup(func(context.Context, *kit.Kit, RunScope) error {
			order = append(order, "setup two")
			return nil
		}),
		WithCompletionHook(CompletionPolicy{MaxContinuations: 1, NewHook: func(_ context.Context, scope RunScope) (CompletionHook, error) {
			order = append(order, "factory")
			return func(ctx context.Context, c CompletionCandidate) (CompletionFeedback, error) {
				candidates = append(candidates, c)
				res, err := scope.Exec(ctx, sandbox.Shell("cat setup.txt model.txt"))
				if err != nil {
					return CompletionFeedback{}, err
				}
				if res.ExitCode != 0 || res.Stdout != "setupmodel" {
					return CompletionFeedback{}, fmt.Errorf("completion used wrong contextFiles: %+v", res)
				}
				if c.ContinuationsUsed == 0 {
					return CompletionFeedback{ContinueWith: "correct the draft"}, nil
				}
				return CompletionFeedback{}, nil
			}, nil
		}}),
	)
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	runner := runtime.NewRunner(journal, factory, runtime.WithCompletionLimit(cfg.completion.MaxContinuations))
	run, err := runner.Start(ctx, "completion-shared", runtime.Input{Text: "make a draft"})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != runtime.RunCompleted || run.Response != "accepted" {
		t.Fatalf("outcome = %+v", run)
	}
	if run.Usage == nil || run.Usage.InputTokens != 3 || run.Usage.OutputTokens != 3 || run.Usage.TotalTokens != 6 {
		t.Fatalf("usage = %+v, want all three model steps", run.Usage)
	}
	if !reflect.DeepEqual(order, []string{"setup one", "setup two", "factory"}) {
		t.Fatalf("callback order = %v", order)
	}
	want := []CompletionCandidate{{Response: "draft"}, {Response: "accepted", ContinuationsUsed: 1}}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("candidates = %+v, want %+v", candidates, want)
	}
	requests := model.Requests()
	if len(requests) != 3 || !strings.Contains(requests[2].Text(), "correct the draft") {
		t.Fatalf("continuation requests = %+v", requests)
	}
	records, err := journal.Replay(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	opens := 0
	for _, record := range records {
		if record.Kind == runtime.RecordSandbox {
			opens++
		}
	}
	if opens != 1 {
		t.Fatalf("sandbox open records = %d, want one shared handle", opens)
	}
}

// Invalid policies must fail before an execution starts. A custom factory
// owns its agent and cannot silently ignore managed callbacks.
func TestCompletionConfiguration(t *testing.T) {
	t.Parallel()
	policy := CompletionPolicy{NewHook: func(context.Context, RunScope) (CompletionHook, error) {
		return func(context.Context, CompletionCandidate) (CompletionFeedback, error) {
			return CompletionFeedback{}, nil
		}, nil
	}}
	cases := []struct {
		name string
		opts []Option
		want string
	}{
		{"duplicate", []Option{WithCompletionHook(policy), WithCompletionHook(policy)}, "only be set once"},
		{"nil factory", []Option{WithCompletionHook(CompletionPolicy{})}, "factory"},
		{"negative limit", []Option{WithCompletionHook(CompletionPolicy{NewHook: policy.NewHook, MaxContinuations: -1})}, "non-negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := append([]Option{WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir())))}, tc.opts...)
			factory, err := resolve(opts...).agentFactory(context.Background(), "", nil)
			if factory != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("factory = %v, error = %v, want %q", factory, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{"WithKitSetup", WithKitSetup(func(context.Context, *kit.Kit, RunScope) error { return nil })},
		{"WithCompletionHook", WithCompletionHook(policy)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, opts := range [][]Option{{WithAgentFactory(stubFactory), tc.opt}, {tc.opt, WithAgentFactory(stubFactory)}} {
				_, err := resolve(opts...).agentFactory(context.Background(), "", nil)
				if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), "WithAgentFactory") {
					t.Fatalf("conflict error = %v", err)
				}
			}
		})
	}
}

// A nil callback, a failed factory, and a nil hook are execution errors,
// not permission to run without the requested check.
func TestManagedCompletionSetupErrors(t *testing.T) {
	t.Parallel()
	failure := errors.New("callback failed")
	for _, tc := range []struct {
		name string
		opt  Option
		want string
		wrap bool
	}{
		{"nil setup", WithKitSetup(nil), "nil Kit setup", false},
		{"setup error", WithKitSetup(func(context.Context, *kit.Kit, RunScope) error { return failure }), "callback failed", true},
		{"factory error", WithCompletionHook(CompletionPolicy{NewHook: func(context.Context, RunScope) (CompletionHook, error) { return nil, failure }}), "create completion hook", true},
		{"nil hook", WithCompletionHook(CompletionPolicy{NewHook: func(context.Context, RunScope) (CompletionHook, error) { return nil, nil }}), "nil hook", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			model := fakemodel.New(fakemodel.Say("must not run"))
			cfg := resolve(tc.opt, WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))))
			factory, err := cfg.agentFactory(context.Background(), "", completionKitOptions(model))
			if err != nil {
				t.Fatal(err)
			}
			a, err := factory(context.Background(), runtime.NewSession("setup-error", runtime.NewMemoryJournal()))
			if a != nil || err == nil || !strings.Contains(err.Error(), tc.want) || (tc.wrap && !errors.Is(err, failure)) {
				t.Fatalf("agent = %v, error = %v", a, err)
			}
			if len(model.Requests()) != 0 {
				t.Fatal("model ran after setup failure")
			}
		})
	}
}

// A real Kit agent lets us check the root wrapper's optional interfaces.
func TestManagedCompletionForwardsFilesAndEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	model := fakemodel.New(fakemodel.Say("file received"))
	factory := runtime.KitAgent(completionKitOptions(model)...)
	base, err := factory(ctx, runtime.NewSession("files", runtime.NewMemoryJournal()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := base.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	var a runtime.Agent = &managedCompletion{Agent: base}
	files, ok := a.(runtime.FileAgent)
	if !ok {
		t.Fatal("completion wrapper lost FileAgent")
	}
	events, ok := a.(interface {
		Subscribe(kit.EventListener) func()
	})
	if !ok {
		t.Fatal("completion wrapper lost Subscribe")
	}
	var count atomic.Int64
	unsubscribe := events.Subscribe(func(kit.Event) { count.Add(1) })
	defer unsubscribe()
	file := kit.LLMFilePart{MediaType: "text/plain", Data: []byte("attached content")}
	res, err := files.PromptResultWithFiles(ctx, "read this", []kit.LLMFilePart{file})
	if err != nil || res == nil || res.Response != "file received" {
		t.Fatalf("file result = %+v, error = %v", res, err)
	}
	found := false
	for _, request := range model.Requests() {
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if got, ok := part.(kit.LLMFilePart); ok && reflect.DeepEqual(got, file) {
					found = true
				}
			}
		}
	}
	if !found || count.Load() == 0 {
		t.Fatalf("file forwarded = %v, events = %d", found, count.Load())
	}
}

// Zero permits acceptance but must refuse an additional model turn. The root
// sentinel must remain usable with errors.Is on the runner's wrapped error.
func TestManagedCompletionZeroLimit(t *testing.T) {
	t.Parallel()
	for _, continueTurn := range []bool{false, true} {
		t.Run(fmt.Sprint(continueTurn), func(t *testing.T) {
			t.Parallel()
			model := fakemodel.New(fakemodel.Say("candidate"))
			cfg := resolve(WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))), WithCompletionHook(CompletionPolicy{
				NewHook: func(context.Context, RunScope) (CompletionHook, error) {
					return func(context.Context, CompletionCandidate) (CompletionFeedback, error) {
						if continueTurn {
							return CompletionFeedback{ContinueWith: "try again"}, nil
						}
						return CompletionFeedback{}, nil
					}, nil
				},
			}))
			factory, err := cfg.agentFactory(context.Background(), "", completionKitOptions(model))
			if err != nil {
				t.Fatal(err)
			}
			run, err := runtime.NewRunner(nil, factory).Start(context.Background(), "zero", runtime.Input{Text: "answer"})
			if continueTurn {
				if !errors.Is(err, ErrContinuationLimit) || run.State != runtime.RunFailed || run.Response != "" {
					t.Fatalf("outcome = %+v, error = %v", run, err)
				}
			} else if err != nil || run.State != runtime.RunCompleted || run.Response != "candidate" {
				t.Fatalf("outcome = %+v, error = %v", run, err)
			}
			if len(model.Requests()) != 1 {
				t.Fatal("zero limit allowed another model request")
			}
		})
	}
}

// The managed sandbox and completion wrappers must both forward files and
// events to Kit. Use the configured factory to test the full wrapper chain.
func TestManagedCompletionSandboxForwardsFilesAndEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	model := fakemodel.New(fakemodel.Say("file received"), fakemodel.Say("unsubscribed"))
	cfg := resolve(WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))), WithCompletionHook(CompletionPolicy{
		NewHook: func(context.Context, RunScope) (CompletionHook, error) {
			return func(context.Context, CompletionCandidate) (CompletionFeedback, error) {
				return CompletionFeedback{}, nil
			}, nil
		},
	}))
	factory, err := cfg.agentFactory(ctx, "", completionKitOptions(model))
	if err != nil {
		t.Fatal(err)
	}
	base, err := factory(ctx, runtime.NewSession("optional", runtime.NewMemoryJournal()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := base.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	files, ok := base.(runtime.FileAgent)
	if !ok {
		t.Fatal("managed sandbox lost FileAgent")
	}
	events, ok := base.(interface {
		Subscribe(kit.EventListener) func()
	})
	if !ok {
		t.Fatal("managed sandbox lost Subscribe")
	}
	var count atomic.Int64
	unsubscribe := events.Subscribe(func(kit.Event) { count.Add(1) })
	defer unsubscribe()
	file := kit.LLMFilePart{MediaType: "text/plain", Data: []byte("file")}
	res, err := files.PromptResultWithFiles(ctx, "read this", []kit.LLMFilePart{file})
	if err != nil || res == nil || res.Response != "file received" {
		t.Fatalf("result = %+v, error = %v", res, err)
	}
	requests := model.Requests()
	found := false
	for _, request := range requests {
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if got, ok := part.(kit.LLMFilePart); ok && reflect.DeepEqual(got, file) {
					found = true
				}
			}
		}
	}
	before := count.Load()
	if len(requests) != 1 || !found || before == 0 {
		t.Fatalf("requests = %d, file forwarded = %v, events = %d", len(requests), found, before)
	}
	unsubscribe()
	if _, err := base.PromptResult(ctx, "next turn"); err != nil {
		t.Fatal(err)
	}
	if got := count.Load(); got != before {
		t.Fatalf("events after unsubscribe = %d, want %d", got, before)
	}
}
