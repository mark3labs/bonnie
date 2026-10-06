package runtime

// ActivityLogger receives live run events from all channels. Logging is off
// unless WithActivityLogger is set. Replayed events are not logged again.
//
// LogActivity is called synchronously, outside the event bus lock. It must be
// safe for concurrent use and return promptly. A slow logger delays the run.
// Events can contain sensitive prompts, tool arguments, results, and responses.
// The logger must not change Event.Data, which is shared with subscribers.
type ActivityLogger interface {
	LogActivity(Event)
}

// WithActivityLogger logs live events from every run through logger. Nil
// disables logging. It does not change journal records or event streams.
func WithActivityLogger(logger ActivityLogger) RunnerOption {
	return func(r *Runner) { r.activityLogger = logger }
}
