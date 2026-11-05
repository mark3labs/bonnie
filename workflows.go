package bonnie

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/workflow"
)

// startWorkflows starts no loop when workflows are absent. It shares the
// host journal; only the host closes that journal after this loop stops.
func (c *config) startWorkflows(ctx context.Context, j runtime.Journal) (context.Context, func() error, error) {
	if len(c.workflows) == 0 {
		if len(c.workflowSetup) != 0 {
			return nil, nil, errors.New("bonnie: workflow setup requires WithWorkflows")
		}
		return ctx, func() error { return nil }, nil
	}
	// Validate all callbacks before any setup can start work or publish the
	// service to application handlers.
	for _, setup := range c.workflowSetup {
		if setup == nil {
			return nil, nil, errors.New("bonnie: nil workflow setup")
		}
	}
	service, err := workflow.NewService(j, c.workflows...)
	if err != nil {
		return nil, nil, err
	}
	loopCtx, cancel := context.WithCancel(ctx)
	for _, setup := range c.workflowSetup {
		if err := setup(loopCtx, service); err != nil {
			cancel()
			return nil, nil, errors.Join(fmt.Errorf("bonnie: workflow setup: %w", err), service.Shutdown(context.Background()))
		}
	}
	done := make(chan error, 1)
	go func() { err := service.Run(loopCtx); done <- err; cancel() }()
	return loopCtx, func() error {
		cancel()
		err := <-done
		if errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bonnie: workflows: %w", err)
		}
		return nil
	}, nil
}
