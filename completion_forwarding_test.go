package bonnie

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

type completionContinuingAgent struct {
	runtime.Agent
	ctx    context.Context
	result *kit.TurnResult
	err    error
}

func (a *completionContinuingAgent) ContinueResult(ctx context.Context) (*kit.TurnResult, error) {
	a.ctx = ctx
	return a.result, a.err
}

// Recovery must preserve the context, result, and error through the completion wrapper.
func TestManagedCompletionForwardsContinuation(t *testing.T) {
	t.Parallel()
	failure := errors.New("continuation failed")
	for _, continuationErr := range []error{nil, failure} {
		base := &completionContinuingAgent{result: &kit.TurnResult{Response: "continued"}, err: continuationErr}
		var agent runtime.Agent = &managedCompletion{Agent: base}
		continuing, ok := agent.(runtime.ContinuationAgent)
		if !ok {
			t.Fatal("completion wrapper lost ContinuationAgent")
		}
		ctx := t.Context()
		result, err := continuing.ContinueResult(ctx)
		if result != base.result || err != continuationErr || base.ctx != ctx {
			t.Fatalf("continuation result = %v, error = %v, context = %v", result, err, base.ctx)
		}
	}
}

// Unsupported continuation must fail without an empty text prompt.
func TestManagedCompletionContinuationUnsupported(t *testing.T) {
	t.Parallel()
	a := &managedCompletion{Agent: struct{ runtime.Agent }{}}
	result, err := a.ContinueResult(t.Context())
	if result != nil || !errors.Is(err, runtime.ErrContinuationUnsupported) {
		t.Fatalf("continuation result = %v, error = %v", result, err)
	}
}
