package bonnie

import (
	"context"
	"flag"
	"path/filepath"
	"testing"
)

// Host flags and repeated registration must not panic or change values.
func TestServeFlagRegistration(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet("host", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:1234", "host address")
	registerServeFlags(fs)
	registerServeFlags(fs)
	if *addr != "127.0.0.1:1234" || fs.Lookup("model") == nil {
		t.Fatal("host flags changed")
	}
}

// A host factory must not require instructions or skills that it does not use.
func TestCustomFactorySkipsInstructions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New(WithAgentFactory(stubFactory), WithInstructions(filepath.Join(root, "missing")),
		WithSkills(filepath.Join(root, "missing-skills")), WithContextFiles(""), WithJournal(root), WithAddr("127.0.0.1:0")).Run(ctx)
	if err != nil {
		t.Fatalf("Run with custom factory: %v", err)
	}
}
