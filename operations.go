package bonnie

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mark3labs/bonnie/runtime"
)

// Runtime gives access to a journal and its configured runner during
// [Agent.WithRuntime]. Do not use the Runtime, journal, or runner after the
// callback returns. Wait for all operations and goroutines before returning.
// No server, channels, scheduler, or cleanup loop starts automatically.
// The caller must start, cancel, and wait for Runner.RunScheduler if needed.
// Only one scheduler may own the journal at a time.
type Runtime struct {
	journal runtime.Journal
	runner  *runtime.Runner
}

// Journal returns the journal owned by this callback scope. Do not close it.
func (r *Runtime) Journal() runtime.Journal { return r.journal }

// Runner returns the configured runner owned by this callback scope.
func (r *Runtime) Runner() *runtime.Runner { return r.runner }

// WithJournal opens the configured journal for inspection during fn, then
// closes it. It does not load dotenv, prompts, skills, or context files, build
// an agent factory, check a provider, or start a server or background work.
// Do not close the journal or use it after fn returns. Wait for all journal
// operations before returning. A nil fn is an error and opens no resources.
// Callback, cancellation, and close errors are returned together.
func (a *Agent) WithJournal(ctx context.Context, fn func(context.Context, runtime.Journal) error) (scopeErr error) {
	if fn == nil {
		return errors.New("bonnie: WithJournal requires a callback")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("bonnie: WithJournal: %w", err)
	}
	journal, err := runtime.OpenSQLiteJournal(a.cfg.journal)
	if err != nil {
		return err
	}
	defer func() { scopeErr = closeJournal(journal, scopeErr) }()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("bonnie: WithJournal: %w", err)
	}
	return errors.Join(fn(ctx, journal), ctx.Err())
}

// WithRuntime prepares the same agent factory and runner as [Agent.Run],
// calls fn, then closes the journal. It loads dotenv and resolves and seeds
// the agent's files, but does not start a server, channels, scheduler, or
// background cleanup. The caller manages any scheduler explicitly.
// Wait for all operations before fn returns, and keep no resource references
// after the callback. A nil fn is an error and opens no resources.
// Callback, cancellation, and close errors are returned together.
func (a *Agent) WithRuntime(ctx context.Context, fn func(context.Context, *Runtime) error) (scopeErr error) {
	if fn == nil {
		return errors.New("bonnie: WithRuntime requires a callback")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("bonnie: WithRuntime: %w", err)
	}
	prepared, err := a.cfg.prepareRuntime(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("bonnie: WithRuntime: %w", err)
	}
	rt, err := a.cfg.openRuntime(prepared.factory)
	if err != nil {
		return err
	}
	defer func() { scopeErr = closeJournal(rt.journal, scopeErr) }()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("bonnie: WithRuntime: %w", err)
	}
	return errors.Join(fn(ctx, rt), ctx.Err())
}

type runtimePreparation struct {
	factory      runtime.AgentFactory
	contextFiles string
	skills       string
	dotenv       bool
}

// prepareRuntime resolves the tree and factory before the journal opens.
func (c *config) prepareRuntime(ctx context.Context) (*runtimePreparation, error) {
	// Load a .env before anything reads the environment: the provider key the
	// agent factory needs and the channel credentials used by Run both come
	// from os.Getenv, so the file has to fill the gaps first. An exported
	// variable still wins over the file.
	dotenv, err := loadDotenv()
	if err != nil {
		return nil, err
	}

	var prompt, skills string
	if c.factory == nil {
		prompt, err = c.systemPrompt()
		if err != nil {
			return nil, err
		}
		skills, err = c.skillsDir()
		if err != nil {
			return nil, err
		}
	}
	contextFiles, err := c.contextFilesDir()
	if err != nil {
		return nil, err
	}
	if c.sharedDirectorySet {
		contextFiles = ""
	}

	// A built binary has no tree beside it, so the contextFiles seed files come
	// from the copies codegen embedded. Seeding never overwrites: a file the
	// model already wrote is the agent's work, not the author's input.
	if contextFiles != "" {
		if err := os.MkdirAll(contextFiles, 0o755); err != nil {
			return nil, fmt.Errorf("bonnie: contextFiles: %w", err)
		}
		if err := seedFromEmbed(Registered().ContextFiles, contextFiles); err != nil {
			return nil, err
		}
	}

	factory, err := c.agentFactory(ctx, contextFiles, c.kitOptions(prompt, skills))
	if err != nil {
		return nil, err
	}

	return &runtimePreparation{factory: factory, contextFiles: contextFiles, skills: skills, dotenv: dotenv}, nil
}

// runnerOptions keeps serving and scoped execution configured alike.
func (c *config) runnerOptions() []runtime.RunnerOption {
	opts := []runtime.RunnerOption{runtime.WithActivityLogger(c.activityLogger)}
	if c.completion != nil {
		opts = append(opts, runtime.WithCompletionLimit(c.completion.MaxContinuations))
	}
	return opts
}

// openRuntime constructs the journal and runner without background work.
func (c *config) openRuntime(factory runtime.AgentFactory) (*Runtime, error) {
	journal, err := runtime.OpenSQLiteJournal(c.journal)
	if err != nil {
		return nil, err
	}
	return &Runtime{journal: journal, runner: runtime.NewRunner(journal, factory, c.runnerOptions()...)}, nil
}

// closeJournal keeps a cleanup failure beside the operation's error.
func closeJournal(journal runtime.Journal, operationErr error) error {
	if err := journal.Close(); err != nil {
		return errors.Join(operationErr, fmt.Errorf("bonnie: close journal: %w", err))
	}
	return operationErr
}
