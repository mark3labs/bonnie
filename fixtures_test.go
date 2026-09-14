package bonnie

import (
	"context"
	"embed"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// testSeed stands in for the workspace files codegen embeds. A generated
// embed binds one directory (`//go:embed workspace`), so every path inside it
// begins with that directory's name — the element [seedFromEmbed] has to
// strip. The fixture keeps exactly that shape.
//
//go:embed testdata
var testSeed embed.FS

// stubAgent is an agent that is never asked to do anything: the tests that use
// it check which factory [Run] picks, not what a turn does.
type stubAgent struct{}

func (stubAgent) PromptResult(context.Context, string) (*kit.TurnResult, error) { return nil, nil }
func (stubAgent) InjectSteer(string)                                            {}
func (stubAgent) Close() error                                                  { return nil }

// stubFactory is a host-supplied agent factory, the shape [WithAgentFactory]
// takes.
func stubFactory(context.Context, *runtime.Session) (runtime.Agent, error) {
	return stubAgent{}, nil
}
