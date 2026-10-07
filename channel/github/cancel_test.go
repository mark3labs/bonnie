package github_test

import (
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/github"
	"github.com/mark3labs/bonnie/runtime"
)

// A signed comment cancels only its issue conversation, not a model prompt.
func TestCancelCommentStopsTurn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{})
	release, active := h.agent.HoldOpen()
	defer release()
	done := make(chan *runtime.Run, 1)
	go func() {
		run, err := h.ch.From(github.AddressIssue("octo", "repo", 42)).Send(t.Context(), "work", channel.SendOptions{})
		if err != nil {
			t.Error(err)
		}
		done <- run
	}()
	<-active
	h.deliver(t, "cancel-1", issueComment("U1", "@my-agent /cancel"))
	select {
	case run := <-done:
		if run == nil || run.State != runtime.RunCancelled {
			t.Fatalf("boundary: %+v", run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop turn")
	}
	if h.agent.Calls() != 0 {
		t.Fatalf("model calls: %d", h.agent.Calls())
	}
}
