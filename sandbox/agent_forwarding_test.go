package sandbox

import (
	"context"
	"errors"
	"reflect"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

type forwardingAgent struct {
	runtime.Agent
	ctx          context.Context
	prompt       string
	files        []kit.LLMFilePart
	result       *kit.TurnResult
	err          error
	listener     kit.EventListener
	unsubscribed bool
}

func (a *forwardingAgent) PromptResultWithFiles(ctx context.Context, prompt string, files []kit.LLMFilePart) (*kit.TurnResult, error) {
	a.ctx, a.prompt, a.files = ctx, prompt, files
	return a.result, a.err
}

func (a *forwardingAgent) Subscribe(listener kit.EventListener) func() {
	a.listener = listener
	return func() { a.unsubscribed = true }
}

// File forwarding must preserve the arguments, result, and error. Event
// forwarding must pass the listener and return the agent's cleanup function.
func TestAgentWithSandboxCloseForwardsFilesAndEvents(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	failure := errors.New("prompt failed")
	for _, promptErr := range []error{nil, failure} {
		base := &forwardingAgent{result: &kit.TurnResult{Response: "received"}, err: promptErr}
		a := &agentWithSandboxClose{Agent: base}
		files := []kit.LLMFilePart{{MediaType: "text/plain", Data: []byte("attached content")}}
		result, err := a.PromptResultWithFiles(ctx, "read this", files)
		if result != base.result || err != promptErr {
			t.Fatalf("result = %+v, error = %v", result, err)
		}
		if base.ctx != ctx || base.prompt != "read this" || !reflect.DeepEqual(base.files, files) {
			t.Fatal("file prompt arguments changed")
		}
		called := false
		unsubscribe := a.Subscribe(func(kit.Event) { called = true })
		if base.listener == nil {
			t.Fatal("listener not forwarded")
		}
		base.listener(kit.TurnStartEvent{})
		if !called {
			t.Fatal("forwarded listener not called")
		}
		unsubscribe()
		if !base.unsubscribed {
			t.Fatal("unsubscribe not forwarded")
		}
	}
}

// An agent without optional capabilities must refuse files and return a safe
// event cleanup function. The wrapper must not call its text prompt instead.
func TestAgentWithSandboxCloseOptionalFallbacks(t *testing.T) {
	t.Parallel()
	a := &agentWithSandboxClose{Agent: struct{ runtime.Agent }{}}
	result, err := a.PromptResultWithFiles(context.Background(), "read this", []kit.LLMFilePart{{MediaType: "text/plain", Data: []byte("file")}})
	if result != nil || !errors.Is(err, runtime.ErrFilesUnsupported) {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	unsubscribe := a.Subscribe(func(kit.Event) { t.Error("unsupported listener called") })
	unsubscribe()
	unsubscribe()
}
