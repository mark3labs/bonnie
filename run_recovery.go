package bonnie

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// recoverRuns continues each run that a previous process left without a stop
// reason, and sends each result to the channel the conversation lives on.
//
// A run continues here unless another owner continues it:
//
//   - a channel that implements [channel.RecoveryOwner] (NATS JetStream gets
//     its message again from the broker);
//   - a scheduled dispatch when the schedule engine runs, because the engine
//     continues the dispatch and owns its delivery outbox;
//   - work in the submission inbox, which [runtime.Runner.RunScheduler] owns
//     ([runtime.Runner.Continue] refuses it).
//
// Each owner holds the run while it checks the state, so a race between two
// of them ends in [runtime.ErrRunActive] for one, never in a second turn.
func (c *config) recoverRuns(ctx context.Context, runner *runtime.Runner, channels []Channel, scheduled bool) {
	byName := make(map[string]Channel, len(channels))
	for _, ch := range channels {
		byName[ch.Name()] = ch
	}
	err := runner.Recover(ctx, runtime.RecoverOptions{
		Skip: func(run runtime.RecoveredRun) bool {
			if scheduled && run.Trigger != nil && run.Trigger.DispatchID != "" {
				return true
			}
			owner, ok := byName[run.Origin.Channel].(channel.RecoveryOwner)
			return ok && owner.OwnsRecovery()
		},
		Done: func(run runtime.RecoveredRun) {
			c.reportRecovered(run)
			receiver, ok := byName[run.Origin.Channel].(channel.Recoverer)
			if !ok {
				// The HTTP channel and the web UI read the run from the
				// journal; there is nothing to send.
				return
			}
			if err := receiver.DeliverRecovered(context.WithoutCancel(ctx), run.ID, run.Run, run.Err); err != nil {
				fmt.Fprintf(os.Stderr, "bonnie: deliver recovered run %s: %v\n", run.ID, err)
			}
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "bonnie: recover runs: %v\n", err)
	}
}

// reportRecovered tells the operator what happened to a continued run. An
// error is always written; a normal outcome is quiet under [Quiet].
func (c *config) reportRecovered(run runtime.RecoveredRun) {
	if run.Err != nil {
		fmt.Fprintf(os.Stderr, "bonnie: continue run %s: %v\n", run.ID, run.Err)
		return
	}
	if c.quiet || run.Run == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "bonnie: continued interrupted run %s: %s\n", run.ID, run.Run.State)
}
