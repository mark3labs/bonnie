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
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		children, err := r.Children(ctx, parentID)
		if err != nil {
			return err
		}
		live := false
		for _, child := range children {
			if child.Background {
				continue
			}
			state, err := r.journal.State(ctx, child.ID)
			if err != nil {
				return err
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
