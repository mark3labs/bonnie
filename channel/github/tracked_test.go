package github_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/bonnie/channel/github"
	"github.com/mark3labs/bonnie/runtime"
)

func TestTrackedGitHubTargetRoundTripAndDeliveryPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{InstallationID: 99})
	target := github.Target{Owner: "octo", Repo: "repo", Number: 7, PullRequest: true}
	receipt, err := h.ch.PrepareDispatch(context.Background(), "dispatch-1", target)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := h.ch.PrepareDispatch(context.Background(), "dispatch-1", map[string]any{"owner": "octo", "repo": "repo", "number": 7, "pull_request": true})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		github.Target
		InstallationID int64 `json:"installation_id"`
	}
	if err := json.Unmarshal(receipt.Target, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Target != target || payload.InstallationID != 99 || restored.Address != receipt.Address {
		t.Fatalf("target receipt = %+v / %+v", payload, restored)
	}
	if err := h.ch.DeliverDispatch(context.Background(), receipt, &runtime.Run{Response: "result"}); err != nil {
		t.Fatal(err)
	}
	posts := h.fake.posts()
	if len(posts) != 1 || posts[0] != "/repos/octo/repo/issues/7/comments|result" {
		t.Fatalf("delivery = %v", posts)
	}
}
