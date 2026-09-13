// Package http is BONNIE's HTTP inbound transport (L3).
//
// It makes a durable run reachable from outside the process: start one, send
// it a message, answer a suspension, cancel a turn, and stream what happens as
// newline-delimited JSON.
//
// The package is named http and imports net/http under its own name. That is
// legal — a package never refers to itself by name — and it keeps the handler
// code reading like ordinary Go.
//
// # Routes
//
//	POST /runs                 start a run, or resolve an address to one
//	GET  /addresses/{address}  look up an address without creating a run
//	GET  /runs/{id}            report a run's durable state
//	POST /runs/{id}            send a message to an existing run
//	POST /runs/{id}/respond    answer a suspended run
//	POST /runs/{id}/cancel     stop the turn a run is executing
//	GET  /runs/{id}/stream     NDJSON event stream, resumable with ?cursor=
//
// # From versus Attach
//
// From resolves a channel-local address — a Slack thread, a browser session —
// to whichever run serves it now, and creates one when the address is new.
// Attach targets exactly one run ID and never creates: an unknown ID is a 404.
// Keeping those apart is what stops a typo in a run ID from silently opening a
// fresh conversation.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

// Channel is the HTTP transport. It implements [channel.Channel] and
// [channel.Inbound].
type Channel struct {
	core   *chat.Core
	policy channel.TurnPolicy
	idGen  []chat.CoreOption
}

var (
	_ channel.Channel = (*Channel)(nil)
	_ channel.Inbound = (*Channel)(nil)
)

// Option configures a [Channel].
type Option func(*Channel)

// WithTurnPolicy sets the policy used when a message arrives while a turn is
// already running. The default is [channel.PolicySteer].
func WithTurnPolicy(p channel.TurnPolicy) Option {
	return func(c *Channel) { c.policy = p }
}

// WithIDGenerator replaces the run-ID generator. Tests use it to get stable
// IDs; production rarely needs it.
func WithIDGenerator(fn func() string) Option {
	return func(c *Channel) { c.idGen = append(c.idGen, chat.WithIDGenerator(fn)) }
}

// New returns an HTTP channel over a runner.
func New(r *runtime.Runner, opts ...Option) *Channel {
	c := &Channel{policy: channel.PolicySteer}
	for _, opt := range opts {
		opt(c)
	}
	c.core = chat.NewCore(r, c.policy, c.idGen...)
	return c
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return "http" }

// Routes implements [channel.Channel].
func (c *Channel) Routes() []channel.Route {
	return []channel.Route{
		{Method: http.MethodPost, Path: "/runs", Handler: c.handleStart},
		{Method: http.MethodGet, Path: "/addresses/{address}", Handler: c.handleAddress},
		{Method: http.MethodGet, Path: "/runs/{id}", Handler: c.handleGet},
		{Method: http.MethodPost, Path: "/runs/{id}", Handler: c.handleSend},
		{Method: http.MethodPost, Path: "/runs/{id}/respond", Handler: c.handleRespond},
		{Method: http.MethodPost, Path: "/runs/{id}/cancel", Handler: c.handleCancel},
		{Method: http.MethodGet, Path: "/runs/{id}/stream", Handler: c.handleStream},
	}
}

// Handler mounts [Channel.Routes] on a fresh mux with the channel as the
// inbound side. Mount it under a prefix with http.StripPrefix if the host
// serves other things.
func (c *Channel) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, route := range c.Routes() {
		handler := route.Handler
		mux.HandleFunc(route.Method+" "+route.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(w, r, c)
		})
	}
	return mux
}

// From implements [channel.Inbound]. The returned reference resolves the
// address every time it is used, so a re-keyed address follows its new run.
func (c *Channel) From(address string) channel.SessionRef {
	return c.core.From(address)
}

// Attach implements [channel.Inbound]. The returned reference targets exactly
// one run and never creates one.
func (c *Channel) Attach(runID string) channel.SessionRef {
	return c.core.Attach(runID)
}

// Rebind points an address at a different run, and creates the binding when
// the address is new. Use it to start a fresh conversation in a place that
// already has one — the same Slack thread, the same browser session.
//
// The binding is journalled, so it survives a restart. The old run is not
// touched: it keeps its history and can still be reached with
// [Channel.Attach].
func (c *Channel) Rebind(ctx context.Context, address, runID string) error {
	return c.core.Addresses().Bind(ctx, address, runID)
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// StartRequest starts a run or routes a message to the run that serves an
// address.
type StartRequest struct {
	// Address is a channel-local conversation key. When set, the run is
	// resolved through it, and created on first use.
	Address string `json:"address,omitempty"`
	// Text is the message to send.
	Text string `json:"text"`
	// Title names the run in operator-facing listings.
	Title string `json:"title,omitempty"`
	// TurnPolicy overrides the channel default for this message.
	TurnPolicy channel.TurnPolicy `json:"turn_policy,omitempty"`
	// Auth carries the caller's identity. It is recorded, not verified.
	Auth *channel.Principal `json:"auth,omitempty"`
}

// SendRequest sends a message to a run that already exists.
type SendRequest struct {
	Text       string             `json:"text"`
	TurnPolicy channel.TurnPolicy `json:"turn_policy,omitempty"`
	Auth       *channel.Principal `json:"auth,omitempty"`
}

// RespondRequest answers a suspended run.
type RespondRequest struct {
	Responses []runtime.InputResponse `json:"responses"`
}

// RunResponse is the JSON form of a run at a turn boundary.
type RunResponse struct {
	RunID    string                  `json:"run_id"`
	Cursor   int                     `json:"cursor,omitempty"`
	State    runtime.RunState        `json:"state"`
	Response string                  `json:"response,omitempty"`
	Suspend  *runtime.SuspendRequest `json:"suspend,omitempty"`
	Usage    *usage                  `json:"usage,omitempty"`
}

// usage is the token accounting for a turn, flattened for the wire so the
// response shape does not change when Kit's usage struct does.
type usage struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
}

// ErrorResponse is the body of every non-2xx reply.
type ErrorResponse struct {
	Error string `json:"error"`
}

func runResponse(run *runtime.Run) RunResponse {
	out := RunResponse{
		RunID:    run.ID,
		State:    run.State,
		Response: run.Response,
		Suspend:  run.Suspend,
	}
	if run.Usage != nil {
		out.Usage = &usage{
			InputTokens:  run.Usage.InputTokens,
			OutputTokens: run.Usage.OutputTokens,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (c *Channel) handleStart(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	var req StartRequest
	if !decode(w, r, &req) {
		return
	}

	var sess channel.SessionRef
	if req.Address != "" {
		sess = in.From(req.Address)
	} else {
		sess = in.From(c.core.NewID())
	}

	run, err := sess.Send(r.Context(), req.Text, channel.SendOptions{
		Auth:       req.Auth,
		TurnPolicy: req.TurnPolicy,
		Title:      req.Title,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse(run))
}

func (c *Channel) handleAddress(w http.ResponseWriter, r *http.Request, _ channel.Inbound) {
	runID, ok, err := c.core.Addresses().LookupContext(r.Context(), r.PathValue("address"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "address is not bound"})
		return
	}
	cursor := 0
	if p, ok := c.core.Runner().Journal().(runtime.Positioner); ok {
		cursor, _ = p.Position(r.Context(), runID)
	}
	writeJSON(w, http.StatusOK, RunResponse{RunID: runID, Cursor: cursor})
}

func (c *Channel) handleGet(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	runID, ok := attachedRunID(w, r, in)
	if !ok {
		return
	}
	run, err := c.core.Runner().Snapshot(r.Context(), runID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse(run))
}

func (c *Channel) handleSend(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	var req SendRequest
	if !decode(w, r, &req) {
		return
	}
	// Attach, not From: a message addressed to a run ID must never create one.
	sess := in.Attach(r.PathValue("id"))
	run, err := sess.Send(r.Context(), req.Text, channel.SendOptions{
		Auth:       req.Auth,
		TurnPolicy: req.TurnPolicy,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse(run))
}

func (c *Channel) handleRespond(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	var req RespondRequest
	if !decode(w, r, &req) {
		return
	}
	run, err := in.Attach(r.PathValue("id")).Respond(r.Context(), req.Responses)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse(run))
}

func (c *Channel) handleCancel(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	if err := in.Attach(r.PathValue("id")).Cancel(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStream writes one JSON object per line and flushes each one, so a
// client sees events as they happen.
//
// The cursor query parameter is the last Seq the client saw. The stream stays
// open until the client goes away: a suspended run may wait for hours, and
// that is the point of the system. A cursor that has fallen off the in-memory
// backlog is served from the journal, so no reconnect sees a gap — the
// catch-up is [Runner.StreamEvents], and it is the reason every event
// carries the journal position it is anchored to.
func (c *Channel) handleStream(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	runID, ok := attachedRunID(w, r, in)
	if !ok {
		return
	}

	cursor := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "cursor must be a non-negative integer"})
			return
		}
		cursor = n
	}

	events, unsubscribe := c.core.Runner().StreamEvents(runID, cursor)
	defer unsubscribe()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	enc := json.NewEncoder(w)
	for {
		select {
		case ev, open := <-events:
			if !open {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

// attachedRunID resolves the path's run ID and writes the error reply itself
// when the run is unknown.
func attachedRunID(w http.ResponseWriter, r *http.Request, in channel.Inbound) (string, bool) {
	runID, err := in.Attach(r.PathValue("id")).RunID(r.Context())
	if err != nil {
		writeError(w, err)
		return "", false
	}
	return runID, true
}

// ---------------------------------------------------------------------------
// Encoding helpers
// ---------------------------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "malformed JSON body: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError maps a runtime sentinel to a status code. Anything unrecognised
// is a 500: guessing would hide a real fault behind a plausible 4xx.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrRunNotFound):
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
	case errors.Is(err, runtime.ErrInvalidRunID):
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	case errors.Is(err, runtime.ErrNotWaiting),
		errors.Is(err, runtime.ErrRunActive),
		errors.Is(err, runtime.ErrRunNotActive):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error()})
	case errors.Is(err, channel.ErrUnknownTurnPolicy):
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	case errors.Is(err, context.Canceled):
		writeJSON(w, 499, ErrorResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
}
