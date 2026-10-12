package bonnie

import (
	"context"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// ToolScope gives a tool factory access to the current agent and run.
// BONNIE creates a new scope on each Start or Resume. Do not retain it after
// that execution ends. Tool code runs on the host; use Exec for sandbox commands.
// Use Kit.Subagent, not Kit.Prompt, for model calls from an active tool.
// Child calls are not durable BONNIE runs and can repeat after interruption.
type ToolScope struct {
	RunScope
	// Kit is the agent bound to this execution's durable session.
	Kit *kit.Kit
}

// ToolFactory builds tools for one managed execution, before its first prompt.
// Closure state is not durable. Factories run again on Start or Resume.
// An error or a nil returned tool stops construction. An empty tool slice is valid.
type ToolFactory func(context.Context, ToolScope) ([]kit.Tool, error)

// WithToolFactory adds tools built with access to the current agent and run.
// Factories run in registration order, after discovered tool factories and
// before WithKitSetup callbacks. A nil factory stops agent construction.
// It cannot be combined with WithAgentFactory.
func WithToolFactory(factory ToolFactory) Option {
	return func(c *config) { c.toolFactories = append(c.toolFactories, factory) }
}
