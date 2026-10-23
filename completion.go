package bonnie

import (
	"context"
	"fmt"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// RunScope gives trusted application code access to one managed execution.
// A new scope is created for each Start or Resume. Do not retain Exec after
// that execution ends. BONNIE owns sandbox cleanup.
type RunScope struct {
	RunID string
	// Session provides trusted setup code with durable state and tool recovery controls.
	Session *runtime.Session
	// Exec opens the sandbox lazily and uses the same handle as agent tools.
	Exec func(context.Context, sandbox.Command) (*sandbox.Result, error)
	// Decide asks the decision model of [WithDecisionModel] or
	// [WithDecider] and journals each call that a key does not answer from
	// the journal. It is nil when neither option is set. See
	// [runtime.Session.Decide].
	Decide func(context.Context, DecisionRequest) (*kit.DecisionResult, error)
}

// KitSetup registers SDK hooks after BONNIE installs its durable session and
// hooks, but before the first prompt. It runs again on Start or Resume.
// Callback code runs on the host; use scope.Exec for sandbox commands.
type KitSetup func(context.Context, *kit.Kit, RunScope) error

// WithKitSetup adds a managed setup callback. Callbacks run in registration
// order. Closure state is local to the instance, not durable. This option
// cannot be combined with WithAgentFactory.
func WithKitSetup(setup KitSetup) Option {
	return func(c *config) { c.kitSetup = append(c.kitSetup, setup) }
}

// CompletionCandidate is a normal response awaiting acceptance.
type CompletionCandidate = runtime.CompletionCandidate

// CompletionFeedback permits completion when ContinueWith is empty.
// Otherwise ContinueWith supplies the input for another model turn.
type CompletionFeedback = runtime.CompletionFeedback

// CompletionHook performs work before BONNIE publishes a final outcome.
// It is skipped on suspension and model failure. Errors fail the run.
// An interrupted hook can run again; external effects must tolerate retries.
type CompletionHook = runtime.CompletionHook

// CompletionHookFactory creates a hook for one managed execution. Closure
// state is not durable. BONNIE restores continuation state separately.
type CompletionHookFactory func(context.Context, RunScope) (CompletionHook, error)

// CompletionPolicy configures work before the final outcome.
type CompletionPolicy struct {
	NewHook CompletionHookFactory
	// MaxContinuations bounds additional model turns. Zero permits no extra
	// turns. Suspension and recovery do not reset the budget.
	MaxContinuations int
}

// ErrContinuationLimit reports an exhausted completion continuation budget.
var ErrContinuationLimit = runtime.ErrContinuationLimit

// WithCompletionHook installs a completion policy. A second policy, a nil
// factory, or a negative limit is a configuration error. This option cannot
// be combined with WithAgentFactory.
func WithCompletionHook(policy CompletionPolicy) Option {
	return func(c *config) {
		if c.completion != nil {
			c.completionDuplicate = true
		}
		c.completion = &policy
	}
}

// managedCompletion forwards optional agent capabilities through the wrapper.
type managedCompletion struct {
	runtime.Agent
	hook CompletionHook
}

func (a *managedCompletion) Complete(ctx context.Context, candidate runtime.CompletionCandidate) (runtime.CompletionFeedback, error) {
	return a.hook(ctx, candidate)
}

func (a *managedCompletion) PromptResultWithFiles(ctx context.Context, prompt string, files []kit.LLMFilePart) (*kit.TurnResult, error) {
	capable, ok := a.Agent.(runtime.FileAgent)
	if !ok {
		return nil, runtime.ErrFilesUnsupported
	}
	return capable.PromptResultWithFiles(ctx, prompt, files)
}

func (a *managedCompletion) ContinueResult(ctx context.Context) (*kit.TurnResult, error) {
	capable, ok := a.Agent.(runtime.ContinuationAgent)
	if !ok {
		return nil, runtime.ErrContinuationUnsupported
	}
	return capable.ContinueResult(ctx)
}

var _ runtime.ContinuationAgent = (*managedCompletion)(nil)

func (a *managedCompletion) Subscribe(listener kit.EventListener) func() {
	if capable, ok := a.Agent.(interface {
		Subscribe(kit.EventListener) func()
	}); ok {
		return capable.Subscribe(listener)
	}
	return func() {}
}

func (c *config) managedFactory(provider sandbox.Provider, opts []kit.Option) runtime.AgentFactory {
	return func(ctx context.Context, s *runtime.Session) (runtime.Agent, error) {
		var hook CompletionHook
		factory := sandbox.AgentWithSetup(provider, !c.noHumanInput,
			func(ctx context.Context, k *kit.Kit, s *runtime.Session, open sandbox.Opener) error {
				scope := RunScope{RunID: s.RunID(), Session: s, Exec: func(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
					sb, err := open(ctx)
					if err != nil {
						return nil, err
					}
					return sb.Exec(ctx, cmd)
				}}
				decide := c.runDecide(k, s)
				scope.Decide = hostDecide(decide)
				if c.decisionTool {
					k.AddTools(decideTool(decide))
					s.AddReplaySafeTools(DecideToolName)
				}
				factories := make([]ToolFactory, 0, len(c.toolFactories)+1)
				if discovered := Registered().ToolFactory; discovered != nil {
					factories = append(factories, discovered)
				}
				for _, entry := range c.toolFactories {
					factories = append(factories, entry.factory)
				}
				for _, factory := range factories {
					if factory == nil {
						return fmt.Errorf("bonnie: nil tool factory")
					}
					tools, err := factory(ctx, ToolScope{RunScope: scope, Kit: k})
					if err != nil {
						return fmt.Errorf("bonnie: create scoped tools: %w", err)
					}
					for _, tool := range tools {
						if tool == nil {
							return fmt.Errorf("bonnie: tool factory returned nil tool")
						}
					}
					k.AddTools(tools...)
				}
				for _, setup := range c.kitSetup {
					if setup == nil {
						return fmt.Errorf("bonnie: nil Kit setup callback")
					}
					if err := setup(ctx, k, scope); err != nil {
						return err
					}
				}
				if c.workspaceSkills != nil {
					if err := c.setupWorkspaceSkills(ctx, k, s, open); err != nil {
						return err
					}
				}
				if c.completion != nil {
					var err error
					hook, err = c.completion.NewHook(ctx, scope)
					if err != nil {
						return fmt.Errorf("bonnie: create completion hook: %w", err)
					}
					if hook == nil {
						return fmt.Errorf("bonnie: completion factory returned nil hook")
					}
				}
				return nil
			}, opts...)
		a, err := factory(ctx, s)
		if err != nil {
			return nil, err
		}
		if hook == nil {
			return a, nil
		}
		return &managedCompletion{Agent: a, hook: hook}, nil
	}
}
