package workflow_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/workflow"
)

func ExampleDefine() {
	ctx := context.Background()
	invoice := workflow.Define("invoice", "1", func(ctx context.Context, amount int) (string, error) {
		receipt, err := workflow.Step(ctx, "charge", func(_ context.Context, attempt workflow.Attempt) (string, error) {
			// Send attempt.IdempotencyKey to the external payment API.
			return fmt.Sprintf("receipt for %d", amount), nil
		})
		if err != nil {
			return "", err
		}
		approved, err := workflow.Wait[bool](ctx, "approved")
		if err != nil {
			return "", err
		}
		if !approved {
			return "rejected", nil
		}
		return receipt, nil
	})
	// Use runtime.OpenSQLiteJournal for persistence across process restarts.
	journal := runtime.NewMemoryJournal()
	service, err := workflow.NewService(journal, invoice)
	if err != nil {
		panic(err)
	}
	handle, err := workflow.Start(ctx, service, invoice, "invoice-123", 50)
	if err != nil {
		panic(err)
	}
	// Signals can arrive before the workflow reaches Wait.
	if err := handle.Signal(ctx, "approved", "approval-event-123", true); err != nil {
		panic(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	receipt, err := handle.Result(ctx)
	cancel()
	if runErr := <-done; runErr != nil && !errors.Is(runErr, context.Canceled) {
		panic(runErr)
	}
	if err != nil {
		panic(err)
	}
	fmt.Println(receipt)
	// Output: receipt for 50
}
