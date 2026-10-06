package nats

import (
	"testing"

	"github.com/mark3labs/bonnie/runtime"
)

// The root selects JetStream, but does not grant stream creation permission.
// Existing explicit subjects continue to select Core NATS unless Stream is set.
func TestRootSubjectDefaults(t *testing.T) {
	t.Parallel()
	nc, _ := jsServer(t)
	r := runtime.NewRunner(runtime.NewMemoryJournal(), nil)
	c, err := New(r, Config{Conn: nc, RootSubject: "however.long.i.want", WorkerID: "one", Subject: "custom.tasks"})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Subject != "custom.tasks" || c.cfg.EventSubject != "however.long.i.want.events" || c.cfg.Stream != DefaultInputStreamName("custom.tasks") || c.cfg.CreateStream {
		t.Fatalf("config %+v", c.cfg)
	}
	if err := c.Start(t.Context()); err == nil {
		t.Fatal("created streams without permission")
	}
	legacy, err := New(r, Config{Conn: nc, Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.cfg.Stream != "" || legacy.cfg.EventSubject != "" {
		t.Fatalf("legacy config %+v", legacy.cfg)
	}
	for _, root := range []string{"a.*", "a..b", "a.>", " a", "a."} {
		if _, err := New(r, Config{Conn: nc, RootSubject: root, WorkerID: "one"}); err == nil {
			t.Fatalf("accepted root %q", root)
		}
	}
}
