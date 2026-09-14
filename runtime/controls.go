package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// The session controls: what a person can do to a conversation besides talk
// to it. eve exposes the same three — reset, clear, compact — on every
// channel; BONNIE puts them on the runner so every transport shares one
// implementation.

// ExtRetired is the extension-data type of the note [Runner.Retire] writes:
// the reason the run was closed.
const ExtRetired = "bonnie.retired"

// ErrCompactionUnsupported is returned by [Runner.Compact] when the agent
// the factory built cannot compact. [*kit.Kit] can; a test agent usually
// cannot.
var ErrCompactionUnsupported = errors.New("bonnie: the agent cannot compact")

// Compactor is the optional interface an [Agent] implements to summarise
// its context on demand. [*kit.Kit] satisfies it. The signature is Kit's,
// so a BONNIE host that already has a Kit does nothing to opt in.
type Compactor interface {
	Compact(ctx context.Context, opts *kit.CompactionOptions, customInstructions string) (*kit.CompactionResult, error)
}

var _ Compactor = (*kit.Kit)(nil)

// Retire closes a run for good. An active turn is cancelled first; the
// reason is journalled as extension data; the state becomes [RunRetired],
// and every later [Runner.Start] or [Runner.Resume] is refused with
// [ErrRunRetired]. The history stays readable.
//
// Retire does not touch any address map: a channel that owns the address
// unbinds it after the run is retired, so a message that arrives between
// the two still finds a run that refuses it rather than one that answers.
// Retiring an unknown run returns [ErrRunNotFound]; retiring a retired run
// is a no-op.
func (r *Runner) Retire(ctx context.Context, runID, reason string) error {
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return err
	}
	if state == RunRetired {
		return nil
	}
	// A turn in flight must not outlive the retirement. Cancel is
	// cooperative and asynchronous; the state checkpoint below wins the
	// race because the turn's own terminal checkpoint is refused by the
	// journal's write order only in spirit — so acquire the run and hold it
	// while the state is written.
	_ = r.Cancel(runID)
	turnCtx, _, err := r.acquireWhenFree(ctx, runID)
	if err != nil {
		return err
	}
	defer r.release(runID)

	s, err := Restore(turnCtx, runID, r.journal)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "reset"
	}
	if _, err := s.AppendExtensionData(ExtRetired, reason); err != nil {
		return err
	}
	return r.checkpoint(context.WithoutCancel(ctx), runID, RunRetired)
}

// Clear drops the run's conversation from the model's context while
// keeping the run ID, its address, its workspace, and its journal. The
// next turn starts from an empty window. It refuses while a turn is
// active with [ErrRunActive], and refuses a retired run.
func (r *Runner) Clear(ctx context.Context, runID string) error {
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return err
	}
	if state == RunRetired {
		return fmt.Errorf("%w: %s", ErrRunRetired, runID)
	}
	turnCtx, _, err := r.acquire(ctx, runID)
	if err != nil {
		return err
	}
	defer r.release(runID)

	s, err := Restore(turnCtx, runID, r.journal)
	if err != nil {
		return err
	}
	return s.Clear(context.WithoutCancel(turnCtx))
}

// Compact asks the agent to summarise the run's older messages now, without
// a user message. The summary is journalled as a compaction entry, so the
// next turn is built on it. It refuses while a turn is active with
// [ErrRunActive], refuses a retired run, and returns
// [ErrCompactionUnsupported] when the agent cannot compact.
func (r *Runner) Compact(ctx context.Context, runID string) error {
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return err
	}
	if state == RunRetired {
		return fmt.Errorf("%w: %s", ErrRunRetired, runID)
	}
	turnCtx, act, err := r.acquire(ctx, runID)
	if err != nil {
		return err
	}
	defer r.release(runID)

	s, err := Restore(turnCtx, runID, r.journal)
	if err != nil {
		return err
	}
	agent, err := r.factory(turnCtx, s)
	if err != nil {
		return err
	}
	defer func() { _ = agent.Close() }()
	act.setAgent(agent)

	c, ok := agent.(Compactor)
	if !ok {
		return fmt.Errorf("%w: %T", ErrCompactionUnsupported, agent)
	}
	_, err = c.Compact(turnCtx, nil, "")
	return err
}

// acquireWhenFree is acquire that waits for a cancelled turn to let go. It
// polls because the turn's release is what frees the slot, and the runner
// does not hand out a signal for that; the wait is bounded by ctx.
func (r *Runner) acquireWhenFree(ctx context.Context, runID string) (context.Context, *activeTurn, error) {
	for {
		turnCtx, act, err := r.acquire(ctx, runID)
		if !errors.Is(err, ErrRunActive) {
			return turnCtx, act, err
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// refuseRetired is the guard Start and Resume share.
func (r *Runner) refuseRetired(ctx context.Context, runID string) error {
	state, err := r.journal.State(ctx, runID)
	if errors.Is(err, ErrRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == RunRetired {
		return fmt.Errorf("%w: %s", ErrRunRetired, runID)
	}
	return nil
}
