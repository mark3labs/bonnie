package nats

import (
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Opt-in must not alter an existing stream or accept overlapping routes.
func TestTargetedConfiguration(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	resultStream(t, js)
	inputStream(t, js, defaultAckWait)
	cfg := jsConfig(nc, "one")
	cfg.TargetedTasks = true
	c, err := New(testRunner(fakemodel.New()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "targeted task routes") {
		t.Fatalf("Start = %v, want provisioning error", err)
	}
	info, err := js.StreamInfo("INPUT")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Config.Subjects) != 2 {
		t.Fatalf("stream changed: %v", info.Config.Subjects)
	}
	for _, subject := range []string{"tasks.worker", "tasks.worker.one", "tasks.worker.one.extra"} {
		bad := cfg
		bad.ResultSubject = subject
		if _, err := New(testRunner(fakemodel.New()), bad); err == nil {
			t.Fatalf("accepted result route %q", subject)
		}
	}
	cfg.Stream, cfg.Consumer, cfg.WorkerID, cfg.CreateStream = "", "", "", false
	if _, err := New(testRunner(fakemodel.New()), cfg); err == nil {
		t.Fatal("accepted targeted Core NATS")
	}
}
