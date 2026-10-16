package chat_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

// A run the host continued after a restart answers on each surface of the
// channel that the run serves, in the channel-local form the adapter's
// deliver function takes, and on no other channel's surface.
func TestDeliverRecoveredFindsTheRunsSurfaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := runtime.NewRunner(runtime.NewMemoryJournal(), nil)
	slack := chat.NewCore(r, "slack", "")
	discord := chat.NewCore(r, "discord", "")
	for _, bind := range []struct {
		core           *chat.Core
		address, runID string
	}{
		{slack, "C1/111", "run-x"},
		{slack, "C2/222", "other"},
		{slack, "C3/333", "run-x"},
		{discord, "D1", "run-x"},
	} {
		if err := bind.core.Bind(ctx, bind.address, bind.runID); err != nil {
			t.Fatal(err)
		}
	}
	want := &runtime.Run{ID: "run-x", State: runtime.RunCompleted, Response: "done"}
	failure := errors.New("failed")
	var got []string
	err := slack.DeliverRecovered(ctx, "run-x", want, failure, func(address string, run *runtime.Run, err error) {
		if run != want || err != failure {
			t.Errorf("deliver(%q) = %+v, %v", address, run, err)
		}
		got = append(got, address)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"C1/111", "C3/333"}) {
		t.Fatalf("addresses = %q", got)
	}
	got = nil
	if err := slack.DeliverRecovered(ctx, "unbound", want, nil, func(address string, _ *runtime.Run, _ error) {
		got = append(got, address)
	}); err != nil || len(got) != 0 {
		t.Fatalf("unbound run delivered to %q, %v", got, err)
	}
}
