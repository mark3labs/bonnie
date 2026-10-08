package bonnie

import (
	"context"
	"testing"

	"github.com/mark3labs/bonnie/runtime"
)

// Replay policy copies the caller's list and reaches trusted per-run setup.
func TestReplaySafeToolOption(t *testing.T) {
	t.Parallel()
	names := []string{"lookup"}
	option := WithReplaySafeTools(names...)
	names[0] = "shell"
	c := &config{}
	option(c)
	if len(c.kitSetup) != 1 {
		t.Fatal("missing managed setup")
	}
	s := runtime.NewSession("r", runtime.NewMemoryJournal())
	if err := c.kitSetup[0](context.Background(), nil, RunScope{RunID: "r", Session: s}); err != nil {
		t.Fatal(err)
	}
}
