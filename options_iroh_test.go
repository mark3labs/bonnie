package bonnie

import (
	"testing"

	irohchannel "github.com/mark3labs/bonnie/channel/iroh"
	"github.com/mark3labs/bonnie/runtime"
)

func TestWithIroh(t *testing.T) {
	t.Parallel()
	cfg := defaults()
	WithIroh(irohchannel.Config{})(cfg)
	ch, err := cfg.channels[0](runtime.NewRunner(runtime.NewMemoryJournal(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name() != "iroh" || len(ch.Routes()) != 0 {
		t.Fatal("unexpected Iroh surface")
	}
}
