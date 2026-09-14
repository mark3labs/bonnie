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
// Every route lives under [channel.APIPrefix], `/bonnie/v1`:
//
//	GET  /bonnie/v1/health               liveness, no auth, no run needed
//	GET  /bonnie/v1/info                 agent name, BONNIE version, channels
//	POST /bonnie/v1/runs                 start a run, or resolve an address to one
//	GET  /bonnie/v1/addresses/{address}  look up an address without creating a run
//	GET  /bonnie/v1/runs/{id}            report a run's durable state
//	POST /bonnie/v1/runs/{id}            send a message to an existing run
//	POST /bonnie/v1/runs/{id}/respond    answer a suspended run
//	POST /bonnie/v1/runs/{id}/cancel     stop the turn a run is executing
//	POST /bonnie/v1/runs/{id}/reset      retire the run for good
//	POST /bonnie/v1/runs/{id}/clear      drop the conversation, keep the run
//	POST /bonnie/v1/runs/{id}/compact    summarise the conversation now
//	GET  /bonnie/v1/runs/{id}/stream     NDJSON event stream, resumable with ?cursor=
//
// The prefix is the framework's reserved namespace: the host refuses any
// other channel a route under `/bonnie/`, so nothing can shadow these.
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
	"fmt"
	"net/http"
	"runtime/debug"
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
	info   Info
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

// WithInfo sets what `GET /bonnie/v1/info` reports. The host fills it in
// once every channel is built, which is the first moment the channel list is
// known. An empty Version is replaced by the BONNIE module version from the
// binary's build info.
func WithInfo(info Info) Option {
	return func(c *Channel) { c.info = info }
}

// Info is the body of `GET /bonnie/v1/info`: enough for a client to say what
// it is talking to, and nothing an operator would call a secret.
type Info struct {
	// Agent is the agent's name, when the host has one.
	Agent string `json:"agent,omitempty"`
	// Version is the BONNIE version the server was built with.
	Version string `json:"version"`
	// Channels lists the names of every mounted channel, the HTTP channel
	// included.
	Channels []string `json:"channels"`
}

// healthResponse is the body of `GET /bonnie/v1/health`.
type healthResponse struct {
	OK     bool   `json:"ok"`
	Status string `json:"status"`
}

// New returns an HTTP channel over a runner.
func New(r *runtime.Runner, opts ...Option) *Channel {
	c := &Channel{policy: channel.PolicySteer}
	for _, opt := range opts {
		opt(c)
	}
	c.core = chat.NewCore(r, c.Name(), c.policy, c.idGen...)
	if c.info.Version == "" {
		c.info.Version = moduleVersion()
	}
	if len(c.info.Channels) == 0 {
		c.info.Channels = []string{c.Name()}
	}
	return c
}

// moduleVersion is the BONNIE version compiled into this binary, read from
// the module's build info. A binary built from a checkout reports "(devel)".
func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	const path = "github.com/mark3labs/bonnie"
	if bi.Main.Path == path {
		return bi.Main.Version
	}
	for _, dep := range bi.Deps {
		if dep.Path == path {
			if dep.Replace != nil && dep.Replace.Version != "" {
				return dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "unknown"
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return "http" }

// Routes implements [channel.Channel]. Every path is under
// [channel.APIPrefix].
func (c *Channel) Routes() []channel.Route {
	p := channel.APIPrefix
	return []channel.Route{
		{Method: http.MethodGet, Path: p + "/health", Handler: c.handleHealth},
		{Method: http.MethodGet, Path: p + "/info", Handler: c.handleInfo},
		{Method: http.MethodPost, Path: p + "/runs", Handler: c.handleStart},
		{Method: http.MethodGet, Path: p + "/addresses/{address}", Handler: c.handleAddress},
		{Method: http.MethodGet, Path: p + "/runs/{id}", Handler: c.handleGet},
		{Method: http.MethodPost, Path: p + "/runs/{id}", Handler: c.handleSend},
		{Method: http.MethodPost, Path: p + "/runs/{id}/respond", Handler: c.handleRespond},
		{Method: http.MethodPost, Path: p + "/runs/{id}/cancel", Handler: c.handleCancel},
		{Method: http.MethodPost, Path: p + "/runs/{id}/reset", Handler: c.handleReset},
		{Method: http.MethodPost, Path: p + "/runs/{id}/clear", Handler: c.handleClear},
		{Method: http.MethodPost, Path: p + "/runs/{id}/compact", Handler: c.handleCompact},
		{Method: http.MethodGet, Path: p + "/runs/{id}/stream", Handler: c.handleStream},
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
	return c.core.Bind(ctx, address, runID)
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
	// Kind says what kind of surface the address names ("dm", "thread",
	// "issue", ...). Recorded on the run's first turn.
	Kind string `json:"kind,omitempty"`
	// Context is per-turn model context: shown to the model in front of
	// the text on this turn only, never kept as history.
	Context []string `json:"context,omitempty"`
	// TurnPolicy overrides the channel default for this message.
	TurnPolicy channel.TurnPolicy `json:"turn_policy,omitempty"`
	// Auth carries the caller's identity. It is recorded, not verified.
	Auth *channel.Principal `json:"auth,omitempty"`
}

// SendRequest sends a message to a run that already exists.
type SendRequest struct {
	Text       string             `json:"text"`
	Context    []string           `json:"context,omitempty"`
	TurnPolicy channel.TurnPolicy `json:"turn_policy,omitempty"`
	Auth       *channel.Principal `json:"auth,omitempty"`
}

// RespondRequest answers a suspended run.
type RespondRequest struct {
	Responses []runtime.InputResponse `json:"responses"`
}

// ResetRequest is the optional body of a reset: why the run is retired.
type ResetRequest struct {
	Reason string `json:"reason,omitempty"`
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

// handleHealth answers before any run exists and without touching the
// journal: it says the process is up and routing, which is what a deployment
// probe asks. It is deliberately not a journal check — a probe that fails
// when the disk is slow would restart a server that is doing its job.
func (c *Channel) handleHealth(w http.ResponseWriter, _ *http.Request, _ channel.Inbound) {
	writeJSON(w, http.StatusOK, healthResponse{OK: true, Status: "ready"})
}

func (c *Channel) handleInfo(w http.ResponseWriter, _ *http.Request, _ channel.Inbound) {
	writeJSON(w, http.StatusOK, c.info)
}

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
		Kind:       req.Kind,
		Context:    req.Context,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse(run))
}

func (c *Channel) handleAddress(w http.ResponseWriter, r *http.Request, _ channel.Inbound) {
	runID, ok, err := c.core.Lookup(r.Context(), r.PathValue("address"))
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
		Context:    req.Context,
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

// handleReset retires the run. The route is ID-addressed, so it retires
// that run and no other; the address that pointed at it is freed by the
// core, and a later start on that address creates a fresh run.
func (c *Channel) handleReset(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	var req ResetRequest
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	if err := in.Attach(r.PathValue("id")).Reset(r.Context(), req.Reason); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Channel) handleClear(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	if err := in.Attach(r.PathValue("id")).Clear(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Channel) handleCompact(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	if err := in.Attach(r.PathValue("id")).Compact(r.Context()); err != nil {
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

// maxRequestBody caps a request body. A turn's text is a message, not a
// file: the webhook adapters have always capped theirs, and the channel's
// own routes buffered whatever a client sent until the decoder gave up.
const maxRequestBody = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(v); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeJSON(w, http.StatusRequestEntityTooLarge,
				ErrorResponse{Error: fmt.Sprintf("request body is larger than %d bytes", maxRequestBody)})
			return false
		}
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
		errors.Is(err, runtime.ErrRunNotActive),
		errors.Is(err, runtime.ErrRunRetired),
		// Another process owns this run's journal. That is a conflict
		// over who may write, not a fault in this server, and a client
		// that reads 500 would retry a request no retry can fix.
		errors.Is(err, runtime.ErrRunOwnedElsewhere):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error()})
	case errors.Is(err, runtime.ErrCompactionUnsupported):
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: err.Error()})
	case errors.Is(err, channel.ErrUnknownTurnPolicy):
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	case errors.Is(err, context.Canceled):
		writeJSON(w, 499, ErrorResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
}
