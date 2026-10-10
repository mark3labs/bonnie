package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

const journalID = runtime.ReservedRunPrefix + "schedule"

// Engine owns schedule work in one process. The host must enforce exclusive
// journal ownership. New does not start a clock or open resources.
type Engine struct {
	runner       *runtime.Runner
	journal      runtime.Journal
	destinations map[string]channel.TrackedReceiver
	defs         map[string]Definition
	schedules    map[string]cronSchedule
	mu           sync.Mutex
	wg           sync.WaitGroup
	active       map[string]bool
	closed       bool
	started      bool
	cancel       context.CancelFunc
	workCtx      context.Context
}
type cronSchedule struct {
	schedule interface{ Next(time.Time) time.Time }
	location *time.Location
}

// New validates job definitions and destination capabilities.
func New(r *runtime.Runner, destinations map[string]channel.TrackedReceiver, definitions ...Definition) (*Engine, error) {
	if r == nil {
		return nil, errors.New("bonnie: schedule: runner is nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{runner: r, journal: r.Journal(), destinations: destinations, defs: map[string]Definition{}, schedules: map[string]cronSchedule{}, active: map[string]bool{}, workCtx: ctx, cancel: cancel}
	for _, d := range definitions {
		if d.Name == "" || strings.ContainsAny(d.Name, "/\\") {
			cancel()
			return nil, errors.New("bonnie: schedule: name must be non-empty without slashes")
		}
		if _, ok := e.defs[d.Name]; ok {
			cancel()
			return nil, fmt.Errorf("bonnie: schedule: duplicate name %q", d.Name)
		}
		modes := 0
		if d.Prompt != "" {
			modes++
		}
		if d.Prepare != nil {
			modes++
		}
		if d.Run != nil {
			modes++
		}
		if modes != 1 {
			cancel()
			return nil, fmt.Errorf("bonnie: schedule %q: exactly one of Prompt, Prepare, and Run is required", d.Name)
		}
		if d.CatchUp != "" && d.CatchUp != "skip" && d.CatchUp != "latest" {
			cancel()
			return nil, fmt.Errorf("bonnie: schedule: unknown catch-up %q", d.CatchUp)
		}
		if d.Overlap != "" && d.Overlap != "skip" && d.Overlap != "allow" {
			cancel()
			return nil, fmt.Errorf("bonnie: schedule: unknown overlap %q", d.Overlap)
		}
		if err := e.validateDestination(d.Destination); err != nil {
			cancel()
			return nil, err
		}
		s, loc, err := parseSchedule(d.Cron, d.TimeZone)
		if err != nil {
			cancel()
			return nil, err
		}
		e.defs[d.Name] = d
		e.schedules[d.Name] = cronSchedule{s, loc}
	}
	return e, nil
}
func (e *Engine) validateDestination(d Destination) error {
	if d.Channel != "" && e.destinations[d.Channel] == nil {
		return fmt.Errorf("bonnie: schedule: destination %q does not support tracked dispatch", d.Channel)
	}
	if _, err := json.Marshal(d.Target); err != nil {
		return fmt.Errorf("bonnie: schedule: target: %w", err)
	}
	return nil
}

// List returns definitions in name order. Prompts and callbacks are not serialized.
func (e *Engine) List() []Definition {
	out := make([]Definition, 0, len(e.defs))
	for _, d := range e.defs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// History returns the latest durable snapshot of each occurrence.
func (e *Engine) History(ctx context.Context, name string) ([]Occurrence, error) {
	recs, err := e.journal.Replay(ctx, journalID)
	if errors.Is(err, runtime.ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	latest := map[string]Occurrence{}
	for _, r := range recs {
		if r.ExtType != "schedule.occurrence" {
			continue
		}
		var o Occurrence
		if err := json.Unmarshal(r.Payload, &o); err != nil {
			return nil, fmt.Errorf("bonnie: schedule: decode occurrence: %w", err)
		}
		if name == "" || o.Fire.Name == name {
			latest[o.Fire.ID] = o
		}
	}
	out := make([]Occurrence, 0, len(latest))
	for _, o := range latest {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fire.ScheduledAt.Before(out[j].Fire.ScheduledAt) })
	return out, nil
}
func (e *Engine) save(ctx context.Context, o Occurrence) error {
	data, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("bonnie: schedule: encode occurrence: %w", err)
	}
	_, err = e.journal.Append(ctx, runtime.Record{RunID: journalID, Kind: runtime.RecordExtensionData, ExtType: "schedule.occurrence", Timestamp: time.Now().UTC(), Payload: data})
	return err
}
func stableID(name string, at time.Time) string {
	sum := sha256.Sum256([]byte(name + "\x00" + at.UTC().Format(time.RFC3339Nano)))
	return "schedule-" + hex.EncodeToString(sum[:16])
}

// Trigger accepts work durably before returning. Repeated IDs return the saved
// occurrence; an ID cannot name two different fires. Execution is asynchronous.
func (e *Engine) Trigger(ctx context.Context, name, id string, at time.Time, kind string) (Occurrence, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Occurrence{}, errors.New("bonnie: schedule: engine is closed")
	}
	d, ok := e.defs[name]
	if !ok {
		return Occurrence{}, fmt.Errorf("bonnie: schedule: unknown schedule %q", name)
	}
	if kind != "cron" && kind != "manual" && kind != "external" {
		return Occurrence{}, errors.New("bonnie: schedule: invalid trigger kind")
	}
	if id == "" {
		id = stableID(name, at)
	}
	hist, err := e.History(ctx, "")
	if err != nil {
		return Occurrence{}, err
	}
	for _, o := range hist {
		if o.Fire.ID == id {
			if o.Fire.Name != name || !o.Fire.ScheduledAt.Equal(at) || o.Fire.Kind != kind {
				return Occurrence{}, errors.New("bonnie: schedule: occurrence ID conflict")
			}
			return o, nil
		}
	}
	o := Occurrence{Fire: Fire{Name: name, ID: id, ScheduledAt: at.UTC(), Kind: kind}, Revision: d.Revision, State: Pending}
	if d.Overlap != "allow" {
		for _, old := range hist {
			if old.Fire.Name == name && unfinished(old) {
				o.State = Skipped
				o.Error = "previous occurrence has unfinished work"
				break
			}
		}
	}
	if err := e.save(ctx, o); err != nil {
		return Occurrence{}, err
	}
	if o.State != Skipped {
		e.launchLocked(d, o)
	}
	return o, nil
}
func unfinished(o Occurrence) bool {
	return o.State == Pending || o.State == Running || o.State == Waiting
}
func (e *Engine) launchLocked(d Definition, o Occurrence) {
	// Limit active executions. Accepted excess work stays pending in the journal.
	if e.closed || e.active[o.Fire.ID] || len(e.active) >= 16 {
		return
	}
	e.active[o.Fire.ID] = true
	e.wg.Go(func() {
		defer func() { e.mu.Lock(); delete(e.active, o.Fire.ID); e.mu.Unlock() }()
		if err := e.execute(e.workCtx, d, o); err != nil {
			log.Printf("bonnie: schedule %s: %v", d.Name, err)
		}
	})
}
func (e *Engine) fail(ctx context.Context, o Occurrence, err error) error {
	o.State = Failed
	o.Error = err.Error()
	return e.save(ctx, o)
}
func (e *Engine) execute(ctx context.Context, d Definition, o Occurrence) error {
	if !o.Prepared {
		if d.Run != nil {
			if err := d.Run(ctx, o.Fire); err != nil {
				return e.fail(ctx, o, err)
			}
			// Save success only after the callback returns. Recovery can repeat
			// the callback with the same Fire.ID if this write does not finish.
			o.Prepared = true
			o.State = Completed
			return e.save(ctx, o)
		}
		dispatches := []Dispatch{{Key: "default", Input: runtime.Input{Text: d.Prompt}, Destination: d.Destination}}
		var err error
		if d.Prepare != nil {
			dispatches, err = d.Prepare(ctx, o.Fire)
		}
		if err != nil {
			return e.fail(ctx, o, err)
		}
		keys := map[string]bool{}
		for i, dp := range dispatches {
			if dp.Key == "" {
				dp.Key = fmt.Sprint(i)
			}
			if keys[dp.Key] {
				return e.fail(ctx, o, errors.New("bonnie: schedule: duplicate dispatch key"))
			}
			keys[dp.Key] = true
			if dp.Destination.Channel == "" && dp.Destination.Target == nil {
				dp.Destination = d.Destination
			}
			if err := e.validateDestination(dp.Destination); err != nil {
				return e.fail(ctx, o, err)
			}
			if len(dp.Input.Files) > 0 {
				return e.fail(ctx, o, errors.New("bonnie: schedule: files are unsupported"))
			}
			if dp.Auth == nil {
				dp.Auth = &channel.Principal{Authenticator: "app", Kind: "runtime", ID: "bonnie:app"}
			}
			o.Work = append(o.Work, Work{Dispatch: dp})
		}
		o.Prepared = true
		o.State = Running
		if len(o.Work) == 0 {
			o.State = Skipped
		}
		if err := e.save(ctx, o); err != nil {
			return err
		}
	}
	waiting := false
	changed := false
	for i := range o.Work {
		w := &o.Work[i]
		dp := w.Dispatch
		did := stableID(o.Fire.ID+"/"+dp.Key, o.Fire.ScheduledAt)
		receiver := e.destinations[dp.Destination.Channel]
		if !w.Reserved {
			if receiver == nil {
				w.Receipt = channel.DispatchReceipt{DispatchID: did, RunID: did}
			} else {
				receipt, err := receiver.PrepareDispatch(ctx, did, dp.Destination.Target)
				if err != nil {
					return e.fail(ctx, o, err)
				}
				w.Receipt = receipt
			}
			w.Reserved = true
			if err := e.save(ctx, o); err != nil {
				return err
			}
		}
		trigger := &runtime.Trigger{ScheduleName: d.Name, OccurrenceID: o.Fire.ID, DispatchID: did, ScheduledAt: o.Fire.ScheduledAt, Kind: o.Fire.Kind}
		if w.Result == nil {
			// A saved final state after this dispatch's trigger closes the crash gap
			// between model completion and saving the schedule snapshot.
			recs, err := e.journal.Replay(ctx, w.Receipt.RunID)
			if err != nil && !errors.Is(err, runtime.ErrRunNotFound) {
				return err
			}
			seen, boundary := false, false
			var completedRecords []runtime.Record
			for index, rec := range recs {
				if rec.Kind == runtime.RecordTrigger {
					var t *runtime.Trigger
					if err := json.Unmarshal(rec.Payload, &t); err != nil {
						return err
					}
					seen = t != nil && t.DispatchID == did
					boundary = false
				}
				if seen && rec.Kind == runtime.RecordState && rec.State != runtime.RunRunning && rec.State != runtime.RunPending {
					boundary = true
					completedRecords = recs[:index+1]
					break
				}
			}
			if boundary {
				w.Result, err = snapshotRecords(ctx, w.Receipt.RunID, completedRecords)
			} else {
				if receiver == nil {
					if err := chat.NewAddressMap(e.journal).NotePrincipal(ctx, w.Receipt.RunID, dp.Auth); err != nil {
						return err
					}
					input := dp.Input
					input.Trigger = trigger
					w.Result, err = e.runner.Start(ctx, w.Receipt.RunID, input)
				} else {
					w.Result, err = receiver.RunDispatch(ctx, w.Receipt, dp.Input.Text, channel.SendOptions{Auth: dp.Auth, Context: dp.Input.Context, Title: dp.Input.Title, Trigger: trigger, TurnPolicy: channel.PolicyQueue})
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return err
				}
				return e.fail(ctx, o, err)
			}
			if w.Result == nil {
				return e.fail(ctx, o, errors.New("bonnie: schedule: receiver returned no run"))
			}
			if w.Result.State == runtime.RunFailed || w.Result.State == runtime.RunCancelled {
				return e.fail(ctx, o, errors.New("bonnie: schedule: agent execution failed"))
			}
			w.Result.Err = nil
			if err := e.save(ctx, o); err != nil {
				return err
			}
		}
		if !w.Delivered {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if receiver != nil {
				if err := receiver.DeliverDispatch(ctx, w.Receipt, w.Result); err != nil {
					w.Error = err.Error()
					w.Attempts++
					delay := time.Second * time.Duration(1<<min(w.Attempts, 8))
					w.RetryAt = time.Now().Add(delay)
					o.State = Running
					return e.save(ctx, o)
				}
			}
			w.Delivered = true
			w.Error = ""
			if err := e.save(ctx, o); err != nil {
				return err
			}
		}
		if w.Result.State == runtime.RunWaiting {
			current, err := e.runner.Snapshot(ctx, w.Receipt.RunID)
			if err != nil {
				return err
			}
			if current.State == runtime.RunWaiting || current.State == runtime.RunRunning {
				waiting = true
			} else {
				w.Result = current
				w.Result.Err = nil
				changed = true
			}
		}
	}
	state := Completed
	if len(o.Work) == 0 {
		state = Skipped
	} else if waiting {
		state = Waiting
	}
	if state == o.State && !changed {
		return nil
	}
	o.State = state
	return e.save(ctx, o)
}

// Reconcile resumes accepted work and retries deliveries without a fresh model
// turn. Host callbacks whose results were not saved can run again with the same
// Fire.ID. Definitions removed or changed by Revision are not executed again.
func (e *Engine) Reconcile(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	hist, err := e.History(ctx, "")
	if err != nil {
		return err
	}
	for _, o := range hist {
		if !unfinished(o) {
			continue
		}
		d, ok := e.defs[o.Fire.Name]
		if !ok || o.Revision != d.Revision {
			if err := e.fail(ctx, o, errors.New("bonnie: schedule: definition removed or revision changed")); err != nil {
				return err
			}
			continue
		}
		retryLater := false
		for _, w := range o.Work {
			if !w.Delivered && time.Now().Before(w.RetryAt) {
				retryLater = true
			}
		}
		if !retryLater {
			e.launchLocked(d, o)
		}
	}
	return nil
}

// Tick checks due times in the current minute. A latest catch-up uses the most
// recent recorded cron time. Manual fires never advance the cron cursor.
func (e *Engine) Tick(ctx context.Context, at time.Time) error {
	if err := e.Reconcile(ctx); err != nil {
		return err
	}
	for _, d := range e.List() {
		if err := e.tickDefinition(ctx, d, at); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) tickDefinition(ctx context.Context, d Definition, at time.Time) error {
	cs := e.schedules[d.Name]
	last := at.Truncate(time.Minute).Add(-time.Minute)
	hist, err := e.History(ctx, d.Name)
	if err != nil {
		return err
	}
	if d.CatchUp == "latest" {
		var latest time.Time
		for _, o := range hist {
			if o.Fire.Kind == "cron" && o.Fire.ScheduledAt.After(latest) {
				latest = o.Fire.ScheduledAt
			}
		}
		if !latest.IsZero() && latest.Before(last) {
			last = latest
		}
	}
	next := cs.schedule.Next(last.In(cs.location))
	var due time.Time
	for !next.IsZero() && !next.After(at) {
		due = next
		next = cs.schedule.Next(next)
	}
	if !due.IsZero() {
		if _, err := e.Trigger(ctx, d.Name, stableID(d.Name, due), due, "cron"); err != nil {
			return err
		}
	}
	return nil
}

// Start runs reconciliation and the clock until ctx ends. It blocks.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("bonnie: schedule: already started")
	}
	e.started = true
	e.mu.Unlock()
	if err := e.Reconcile(ctx); err != nil {
		return err
	}
	// Catch-up requires a prior cron occurrence. New jobs wait for the clock.
	for _, d := range e.List() {
		if d.CatchUp != "latest" {
			continue
		}
		history, err := e.History(ctx, d.Name)
		if err != nil {
			return err
		}
		for _, old := range history {
			if old.Fire.Kind == "cron" {
				if err := e.tickDefinition(ctx, d, time.Now().Truncate(time.Minute)); err != nil {
					return err
				}
				break
			}
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := time.Now().Truncate(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case at := <-ticker.C:
			if err := e.Reconcile(ctx); err != nil {
				return err
			}
			minute := at.Truncate(time.Minute)
			if minute.After(last) {
				if err := e.Tick(ctx, minute); err != nil {
					return err
				}
				last = minute
			}
		}
	}
}

// Shutdown refuses new fires, drains work, then cancels work at the deadline.
// It waits for cancelled executions before the host closes their journal.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		e.cancel()
		return nil
	case <-ctx.Done():
		e.cancel()
		<-done
		return ctx.Err()
	}
}
