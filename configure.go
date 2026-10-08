package bonnie

// Configure applies options in order to the agent's current configuration.
// It uses the same option rules as [New]: replacement options override earlier
// values, and additive options append or merge. Validation stays in [Agent.Run];
// Configure does not read files, open resources, or start the agent.
//
// Use it before Run, for example in a [WithCommand] pre-run hook to apply parsed
// flags. Configure is not safe for concurrent use. Do not call it while Run or
// Serve is running, except in Serve's command setup or pre-run hooks. It does
// not support changes to a live agent. Built-in Serve flags still apply before
// Run and override the matching options when those flags have a value.
func (a *Agent) Configure(opts ...Option) {
	for _, opt := range opts {
		opt(a.cfg)
	}
}
