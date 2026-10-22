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
	return func(c *config) {
		c.toolFactories = append(c.toolFactories, namedToolFactory{factory: factory, fromDefault: c.applyingDefaults})
	}
}

// WithNamedToolFactory adds a tool factory that a later option with the same
// name replaces in place. A package that ships a default factory, such as
// [github.com/mark3labs/bonnie/selfmod], uses the name, so an explicit option in
// main.go replaces a default that `bonnie dev` registered and the model does
// not see the same tool twice. An empty name is the same as [WithToolFactory].
func WithNamedToolFactory(name string, factory ToolFactory) Option {
	return func(c *config) {
		if name != "" {
			for i, entry := range c.toolFactories {
				if entry.name == name {
					c.toolFactories[i] = namedToolFactory{name: name, factory: factory, fromDefault: c.applyingDefaults}
					return
				}
			}
		}
		c.toolFactories = append(c.toolFactories, namedToolFactory{name: name, factory: factory, fromDefault: c.applyingDefaults})
	}
}

// namedToolFactory is one registered tool factory. Name is empty for an
// anonymous factory, which no later option can replace.
type namedToolFactory struct {
	name    string
	factory ToolFactory
	// fromDefault marks a factory from [RegisterDefaults]. It is dropped,
	// not refused, beside [WithAgentFactory].
	fromDefault bool
}
