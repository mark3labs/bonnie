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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// Channel is the HTTP transport. It implements [channel.Channel] and
// [channel.Inbound].
type Channel struct {
	runner *runtime.Runner
	policy channel.TurnPolicy
	newID  func() string
	reg    *registry

	mu    sync.Mutex
	locks map[string]*sync.Mutex
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
	return func(c *Channel) { c.newID = fn }
}

// New returns an HTTP channel over a runner.
func New(r *runtime.Runner, opts ...Option) *Channel {
	c := &Channel{
		runner: r,
		policy: channel.PolicySteer,
		newID:  newRunID,
		reg:    newRegistry(r.Journal()),
		locks:  make(map[string]*sync.Mutex),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return "http" }

// Routes implements [channel.Channel].
func (c *Channel) Routes() []channel.Route {
	return []channel.Route{
		{Method: http.MethodPost, Path: "/runs", Handler: c.handleStart},
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
	return &ref{c: c, address: address, create: true}
}

// Attach implements [channel.Inbound]. The returned reference targets exactly
// one run and never creates one.
func (c *Channel) Attach(runID string) channel.SessionRef {
	return &ref{c: c, runID: runID}
}

// Rebind points an address at a different run, and creates the binding when
// the address is new. Use it to start a fresh conversation in a place that
// already has one — the same Slack thread, the same browser session.
//
// The binding is journalled, so it survives a restart. The old run is not
// touched: it keeps its history and can still be reached with
// [Channel.Attach].
func (c *Channel) Rebind(ctx context.Context, address, runID string) error {
	return c.reg.bind(ctx, address, runID)
}

// lock serialises turns for one run. A queued message waits here instead of
// racing an active turn.
func (c *Channel) lock(runID string) func() {
	c.mu.Lock()
	mu, ok := c.locks[runID]
	if !ok {
		mu = &sync.Mutex{}
		c.locks[runID] = mu
	}
	c.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// ref is a [channel.SessionRef] over one address or one run ID.
type ref struct {
	c       *Channel
	address string
	runID   string
	create  bool
}

var _ channel.SessionRef = (*ref)(nil)

// RunID implements [channel.SessionRef].
func (s *ref) RunID(ctx context.Context) (string, error) {
	if !s.create {
		// Attach: the run must already exist.
		if _, err := s.c.runner.Journal().State(ctx, s.runID); err != nil {
			return "", err
		}
		return s.runID, nil
	}
	return s.c.reg.resolve(ctx, s.address, s.c.newID)
}

// Send implements [channel.SessionRef].
func (s *ref) Send(ctx context.Context, text string, opts channel.SendOptions) (*runtime.Run, error) {
	runID, err := s.RunID(ctx)
	if err != nil {
		return nil, err
	}
	if opts.Auth != nil {
		if err := s.c.reg.notePrincipal(ctx, runID, opts.Auth); err != nil {
			return nil, err
		}
	}

	policy := opts.TurnPolicy
	if policy == "" {
		policy = s.c.policy
	}

	// A message that lands mid-turn is steered into the turn that is already
	// running, which keeps the work the turn has done. Queue waits instead.
	if policy == channel.PolicySteer && s.c.runner.IsActive(runID) {
		if err := s.c.runner.Steer(runID, text); err == nil {
			return s.c.runner.Snapshot(ctx, runID)
		}
		// The turn ended between the check and the steer: fall through and
		// run it as a new turn.
	}

	unlock := s.c.lock(runID)
	defer unlock()
	return s.c.runner.Start(ctx, runID, runtime.Input{Text: text})
}

// Respond implements [channel.SessionRef].
func (s *ref) Respond(ctx context.Context, responses []runtime.InputResponse) (*runtime.Run, error) {
	runID, err := s.RunID(ctx)
	if err != nil {
		return nil, err
	}
	unlock := s.c.lock(runID)
	defer unlock()
	return s.c.runner.Resume(ctx, runID, responses)
}

// Cancel implements [channel.SessionRef].
func (s *ref) Cancel(ctx context.Context) error {
	runID, err := s.RunID(ctx)
	if err != nil {
		return err
	}
	return s.c.runner.Cancel(runID)
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
		sess = in.From(c.newID())
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

func (c *Channel) handleGet(w http.ResponseWriter, r *http.Request, in channel.Inbound) {
	runID, ok := attachedRunID(w, r, in)
	if !ok {
		return
	}
	run, err := c.runner.Snapshot(r.Context(), runID)
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
// that is the point of the system.
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

	events, unsubscribe := c.runner.Events().Subscribe(runID, cursor)
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
	case errors.Is(err, context.Canceled):
		writeJSON(w, 499, ErrorResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
}

// newRunID returns a run ID that is safe as a file name, which is what the
// file journal needs.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever
		// does, a duplicate run ID would silently merge two conversations.
		panic(fmt.Sprintf("bonnie: read random: %v", err))
	}
	return "run-" + hex.EncodeToString(b[:])
}
