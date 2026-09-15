package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channel/github"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// A hand-off must bind the address an inbound comment on the same surface
// resolves to, or the first reply starts a second run and the
// conversation splits in two. GitHub numbers issues and pull requests in
// one sequence, and their addresses differ, so the target says which.
func TestReceiveBindsTheAddressAnInboundCommentResolvesTo(t *testing.T) {
	cases := []struct {
		name    string
		target  github.Target
		address string
		kind    string
	}{
		{
			name:    "an issue",
			target:  github.Target{Owner: "octo", Repo: "repo", Number: 42},
			address: github.AddressIssue("octo", "repo", 42),
			kind:    chat.KindIssue,
		},
		{
			name:    "a pull request",
			target:  github.Target{Owner: "octo", Repo: "repo", Number: 7, PullRequest: true},
			address: github.AddressPullRequest("octo", "repo", 7),
			kind:    chat.KindPullRequest,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, github.Config{InstallationID: 99})
			h.agent.Say(&kit.TurnResult{Response: "the digest"})

			err := h.ch.Receive(context.Background(), c.target, "summarise this", channel.SendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return len(h.fake.posts()) > 0 })

			id, ok, _ := refRunID(h.ch, c.address)
			if !ok {
				t.Fatalf("the hand-off did not bind %s", c.address)
			}
			sess, err := runtime.Restore(context.Background(), id, h.journal)
			if err != nil {
				t.Fatal(err)
			}
			if o := sess.Origin(); o.Channel != "github" || o.Kind != c.kind {
				t.Fatalf("origin = %+v, want github/%s", o, c.kind)
			}
			// Both surfaces take their comments on the issue endpoint.
			if got := h.fake.posts()[0]; !strings.Contains(got, "/comments|the digest") {
				t.Fatalf("delivered %q", got)
			}
		})
	}
}

// The reply to a hand-off continues the run the hand-off started. This is
// the acceptance criterion the address choice above exists to serve: a PR
// comment is addressed as a PR, so a hand-off that bound the issue
// address would be answered by a second run.
func TestACommentContinuesTheRunAHandOffStarted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{InstallationID: 99})
	h.agent.Say(&kit.TurnResult{Response: "the digest"})

	target := github.Target{Owner: "octo", Repo: "repo", Number: 7, PullRequest: true}
	if err := h.ch.Receive(context.Background(), target, "summarise this PR", channel.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(h.fake.posts()) > 0 })
	started, ok, _ := refRunID(h.ch, github.AddressPullRequest("octo", "repo", 7))
	if !ok {
		t.Fatal("the hand-off bound no PR address")
	}

	// A comment on that PR, with no mention: the agent is in this thread
	// already, so the bound address must carry it into the same run.
	h.agent.Say(&kit.TurnResult{Response: "the follow-up"})
	h.deliver(t, "d1", prTimelineComment("U1", "and in one sentence?"))
	waitFor(t, func() bool { return len(h.fake.posts()) > 1 })

	continued, _, _ := refRunID(h.ch, github.AddressPullRequest("octo", "repo", 7))
	if continued != started {
		t.Fatalf("the comment ran as %s, want the hand-off's run %s", continued, started)
	}
	if calls := h.agent.Calls(); calls != 2 {
		t.Fatalf("the agent ran %d turns, want 2 in one conversation", calls)
	}
}

// A hand-off with no installation has no token to post with. It says so
// rather than starting a run whose reply can never be delivered.
func TestReceiveNeedsAnInstallation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{})
	err := h.ch.Receive(context.Background(),
		github.Target{Owner: "octo", Repo: "repo", Number: 42}, "hi", channel.SendOptions{})
	if err == nil {
		t.Fatal("the hand-off started without an installation")
	}
	if !strings.Contains(err.Error(), "GITHUB_INSTALLATION_ID") {
		t.Fatalf("error = %v, want the variable that fixes it", err)
	}
	if calls := h.agent.Calls(); calls != 0 {
		t.Fatalf("the agent ran %d turns for a hand-off that could not deliver", calls)
	}
}

// A target of the wrong shape is refused before anything is bound.
func TestReceiveRefusesATargetThatIsNotARepositoryItem(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{InstallationID: 99})
	bad := []any{
		"octo/repo#42",
		github.Target{Owner: "octo", Repo: "repo"},
		github.Target{Repo: "repo", Number: 42},
		github.Target{Owner: "octo", Number: 42},
	}
	for _, target := range bad {
		if err := h.ch.Receive(context.Background(), target, "hi", channel.SendOptions{}); err == nil {
			t.Fatalf("Receive accepted %#v as a target", target)
		}
	}
}
