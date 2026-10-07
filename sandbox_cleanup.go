package bonnie

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

func (c *config) configureSandboxCleanup(provider sandbox.Provider) error {
	if c.sandboxCleanup == nil {
		return nil
	}
	p := c.sandboxCleanup
	for _, d := range []time.Duration{p.CompletedAfter, p.FailedAfter, p.CancelledAfter, p.RetiredAfter} {
		if d < 0 {
			return fmt.Errorf("bonnie: sandbox cleanup retention must not be negative")
		}
	}
	deleter, ok := provider.(sandbox.RunDeleter)
	if !ok {
		return fmt.Errorf("bonnie: sandbox %s cannot delete run workspaces", provider.Name())
	}
	if validator, ok := provider.(sandbox.RunCleanupValidator); ok {
		if err := validator.ValidateRunCleanup(); err != nil {
			return fmt.Errorf("bonnie: sandbox cleanup: %w", err)
		}
	}
	c.cleanupProvider = deleter
	return nil
}

// cleanupLoop uses the same Runner as the channels, so cleanup cannot race a
// turn. Cancellation stops the sweep before the journal is closed.
func (c *config) cleanupLoop(ctx context.Context, runner *runtime.Runner, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		err := runner.CleanupSandboxes(ctx, *c.sandboxCleanup, func(ctx context.Context, runID string) (bool, error) {
			deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return c.cleanupProvider.DeleteRun(deleteCtx, runID)
		})
		if err != nil && ctx.Err() == nil {
			slog.Error("bonnie: sandbox cleanup failed; will retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
