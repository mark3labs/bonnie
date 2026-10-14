package runtime

import (
	"context"
	"fmt"
	"time"
)

// WaitChildren waits for foreground children to settle. A parked child remains
// live until a human answers or cancellation stops it. RunScheduler must run
// separately so children can execute. Cancellation of this wait does not
// cancel children; use CancelOwned to request that action.
func (r *Runner) WaitChildren(ctx context.Context, parentID string) error {
	delay := 20 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	previous := map[string]RunState{}
	for {
		children, err := r.Children(ctx, parentID)
		if err != nil {
			return err
		}
		live := false
		changed := false
		states := map[string]RunState{}
		for _, child := range children {
			if child.Background {
				continue
			}
			state, err := r.journal.State(ctx, child.ID)
			if err != nil {
				return err
			}
			states[child.ID] = state
			if previous[child.ID] != state {
				changed = true
			}
			if state == RunFailed || state == RunCancelled || state == RunRetired {
				return fmt.Errorf("bonnie: child %s ended in %s", child.ID, state)
			}
			if state != RunCompleted {
				live = true
			}
		}
		if !live {
			return nil
		}
		if changed || len(states) != len(previous) {
			delay = 20 * time.Millisecond
		}
		previous = states
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			delay = min(delay*2, 500*time.Millisecond)
		}
	}
}
