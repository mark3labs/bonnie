package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/runtime"
)

// Target selects an exact attempt on its owning worker. All fields are required.
type Target struct {
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`
	RunID     string `json:"run_id"`
	WorkerID  string `json:"worker_id"`
}

// StatusEvent reports durable acceptance or a journalled run-state change.
// Delivery is at least once. EventID is stable across publication retries.
// Seq is a per-run journal cursor; acceptance uses zero. No agent activity,
// prompts, tool arguments, or response text are included.
type StatusEvent struct {
	Version int `json:"version"`
	Target
	EventID string           `json:"event_id"`
	Type    string           `json:"type"`
	Seq     int              `json:"seq"`
	Time    time.Time        `json:"time"`
	State   runtime.RunState `json:"state"`
}

// StatusRequest queries an exact attempt. Queries use NATS request/reply.
type StatusRequest struct {
	Version int `json:"version"`
	Target
	// TurnID optionally scopes cancellation to the observed turn.
	TurnID string `json:"turn_id,omitempty"`
}

// CancelRequest requests cancellation of an active or parked turn. It cannot
// undo external effects. Finished runs are harmless no-ops.
type CancelRequest = StatusRequest

// Status is a current attempt snapshot. Active means this process is executing
// it; a saved running state with Active false is interrupted, not proof of work.
// Error reports a rejected query or command. CancelRequested is not completion.
type Status struct {
	Version int `json:"version"`
	Target
	State           runtime.RunState        `json:"state,omitempty"`
	Active          bool                    `json:"active"`
	Seq             int                     `json:"seq"`
	Suspend         *runtime.SuspendRequest `json:"suspend,omitempty"`
	CancelStatus    string                  `json:"cancel_status,omitempty"`
	TurnID          string                  `json:"turn_id,omitempty"`
	CancelRequested bool                    `json:"cancel_requested,omitempty"`
	Error           string                  `json:"error,omitempty"`
}

type taskAdmission struct {
	Target
	Time time.Time `json:"time"`
}

func (c *Channel) statusKey() string { return c.cacheKey("status", "index") }

// admitTask saves correlation before any execution. Redelivery still follows
// the existing fresh-attempt policy; the index preserves every prior attempt.
func (c *Channel) admitTask(ctx context.Context, result Result, runID string) error {
	if c.cfg.EventSubject == "" && c.cfg.QuerySubject == "" && c.cfg.CommandSubject == "" {
		return nil
	}
	target := Target{TaskID: result.TaskID, AttemptID: result.AttemptID, RunID: runID, WorkerID: c.cfg.WorkerID}
	data, err := json.Marshal(taskAdmission{Target: target, Time: time.Now().UTC()})
	if err != nil {
		return err
	}
	_, err = c.core.Runner().Journal().Append(ctx, runtime.Record{RunID: c.statusKey(), Kind: runtime.RecordExtensionData, ExtType: "nats_task_admitted", Payload: data})
	return err
}

func (c *Channel) admissions(ctx context.Context) ([]taskAdmission, map[string]int, error) {
	recs, err := c.core.Runner().Journal().Replay(ctx, c.statusKey())
	if errors.Is(err, runtime.ErrRunNotFound) {
		return nil, map[string]int{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var out []taskAdmission
	cursors := map[string]int{}
	for _, rec := range recs {
		switch rec.ExtType {
		case "nats_task_admitted":
			var a taskAdmission
			if err := json.Unmarshal(rec.Payload, &a); err != nil {
				return nil, nil, err
			}
			out = append(out, a)
		case "nats_status_cursor":
			var p struct {
				RunID string `json:"run_id"`
				Seq   int    `json:"seq"`
			}
			if err := json.Unmarshal(rec.Payload, &p); err != nil {
				return nil, nil, err
			}
			cursors[p.RunID] = p.Seq
		}
	}
	return out, cursors, nil
}

// flushStatuses reads the journal directly, not the live activity bus. The
// cursor is saved only after broker confirmation. A crash between these steps
// can repeat an event but cannot discard it. One loop owns publication order.
func (c *Channel) flushStatuses(ctx context.Context) error {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	admissions, cursors, err := c.admissions(ctx)
	if err != nil {
		return err
	}
	for _, a := range admissions {
		cursor, accepted := cursors[a.RunID]
		if !accepted {
			if err := c.publishStatus(ctx, StatusEvent{Version: 1, Target: a.Target, Type: "task_accepted", State: runtime.RunPending, Time: a.Time}); err != nil {
				return err
			}
			if err := c.saveStatusCursor(ctx, a.RunID, 0); err != nil {
				return err
			}
		}
		recs, err := c.core.Runner().Journal().Replay(ctx, a.RunID)
		if errors.Is(err, runtime.ErrRunNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if rec.Seq <= cursor || rec.Kind != runtime.RecordState {
				continue
			}
			ev := StatusEvent{Version: 1, Target: a.Target, Type: runtime.EventState, Seq: rec.Seq, Time: rec.Timestamp, State: rec.State}
			if err := c.publishStatus(ctx, ev); err != nil {
				return err
			}
			if err := c.saveStatusCursor(ctx, a.RunID, rec.Seq); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Channel) publishStatus(ctx context.Context, ev StatusEvent) error {
	ev.EventID = streamName("bonnie-status-", ev.RunID+":"+ev.Type+":"+strconv.Itoa(ev.Seq))
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	msg := gonats.NewMsg(c.cfg.EventSubject)
	msg.Data = data
	msg.Header.Set(gonats.MsgIdHdr, ev.EventID)
	publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = c.js.PublishMsg(msg, gonats.Context(publishCtx))
	return err
}

func (c *Channel) saveStatusCursor(ctx context.Context, runID string, seq int) error {
	data, err := json.Marshal(struct {
		RunID string `json:"run_id"`
		Seq   int    `json:"seq"`
	}{runID, seq})
	if err != nil {
		return err
	}
	_, err = c.core.Runner().Journal().Append(ctx, runtime.Record{RunID: c.statusKey(), Kind: runtime.RecordExtensionData, ExtType: "nats_status_cursor", Payload: data})
	return err
}

func (c *Channel) inspect(ctx context.Context, req StatusRequest, cancel bool) Status {
	out := Status{Version: 1, Target: req.Target}
	if req.Version != 1 || !validID(req.TaskID) || !validID(req.RunID) || !validID(req.AttemptID) || req.WorkerID != c.cfg.WorkerID {
		out.Error = "invalid target or version"
		return out
	}
	admissions, _, err := c.admissions(ctx)
	if err != nil {
		out.Error = "status index read failed"
		return out
	}
	found := false
	for _, a := range admissions {
		if a.Target == req.Target {
			found = true
			break
		}
	}
	if !found {
		out.Error = "unknown task attempt"
		return out
	}
	if cancel {
		result, err := c.core.Runner().RequestCancel(ctx, req.RunID, req.TurnID)
		if err != nil {
			out.Error = err.Error()
		} else {
			out.CancelStatus, out.TurnID = result.Status, result.TurnID
			out.CancelRequested = result.Status == runtime.CancelRequested
		}
	}
	recs, err := c.core.Runner().Journal().Replay(ctx, req.RunID)
	if errors.Is(err, runtime.ErrRunNotFound) {
		out.State = runtime.RunPending
		return out
	}
	if err != nil {
		out.Error = "run read failed"
		return out
	}
	out.State = runtime.RunPending
	for _, rec := range recs {
		out.Seq = rec.Seq
		if rec.Kind == runtime.RecordTurn {
			out.TurnID = rec.Text
		}
		if rec.Kind == runtime.RecordState {
			out.State = rec.State
		}
	}
	out.Active = c.core.Runner().IsActive(req.RunID)
	if out.State == runtime.RunWaiting {
		run, err := c.core.Runner().Snapshot(ctx, req.RunID)
		if err != nil {
			out.Error = "snapshot read failed"
		} else {
			out.Suspend = run.Suspend
		}
	}
	return out
}

func (c *Channel) startStatusControls(ctx context.Context, nc *gonats.Conn) error {
	for _, route := range []struct {
		base   string
		cancel bool
	}{{c.cfg.QuerySubject, false}, {c.cfg.CommandSubject, true}} {
		if route.base == "" {
			continue
		}
		sub, err := nc.Subscribe(route.base+"."+c.cfg.WorkerID, func(msg *gonats.Msg) {
			// Control routes are request/reply only. Never publish to a caller-selected
			// route unless it is a NATS inbox. Use permissions to protect these routes.
			if !validSubject(msg.Reply) || len(msg.Reply) < 7 || msg.Reply[:7] != "_INBOX." {
				return
			}
			var req StatusRequest
			var out Status
			if err := decode(msg.Data, &req); err != nil {
				out = Status{Version: 1, Error: "invalid request"}
			} else {
				out = c.inspect(ctx, req, route.cancel)
			}
			data, err := json.Marshal(out)
			if err != nil {
				c.report("control encode failed")
				return
			}
			if len(data) > maxMessageBytes {
				data = []byte(`{"version":1,"error":"status exceeds message limit"}`)
			}
			if err := msg.Respond(data); err != nil && ctx.Err() == nil {
				c.report("control reply failed")
			}
		})
		if err != nil {
			return fmt.Errorf("bonnie: channel/nats: control subscription: %w", err)
		}
		c.subs = append(c.subs, sub)
	}
	ready, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return nc.FlushWithContext(ready)
}
