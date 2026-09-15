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
//	POST /bonnie/v1/addresses/{address}  bind an address to a run, running no turn
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
// # Identity
//
// Every other inbound adapter mints its principal from something it
// verified: Slack an HMAC, Discord an Ed25519 signature, GitHub an HMAC.
// This transport carries no platform signature, so identity is what the
// host configures with [WithAuthenticator] — a bearer token, an OIDC
// assertion, a client certificate.
//
// Without one the channel authenticates nobody, and the `auth` field of a
// request body is recorded exactly as sent: unverified, and worth no more
// than the network boundary around the deployment. That is the default
// because it is what a loopback `bonnie dev` needs, NOT because it is safe
// to expose. A deployment reachable by anyone else wants an authenticator,
// or a reverse proxy that has already established who is calling.
//
// One route refuses to run on an unverified identity at all: an
// `operation_id` start needs a principal an authenticator proved, because
// the idempotency key is namespaced by the caller's identity and a
// self-asserted one lets any caller claim any namespace.
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
	"os"
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
	auth   Authenticator
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

// Authenticator verifies a request and returns the principal it proves.
//
// It is the HTTP channel's equivalent of the signature check every webhook
// adapter runs: Slack verifies an HMAC, Discord an Ed25519 signature, GitHub
// an HMAC, and each then MINTS the principal from what the check proved. An
// Authenticator does the same job for a transport that carries no platform
// signature — a bearer token, an OIDC assertion, a mutual-TLS certificate,
// a session cookie the host can resolve.
//
// Return [ErrUnauthenticated] to refuse the request with 401. Any other
// error is a fault in the verifier itself and becomes a 500, because a
// verifier that broke has not proved the caller is an impostor.
//
// A nil principal with a nil error means "no identity, and that is fine":
// the request proceeds unattributed, which is what an open read-only
// deployment wants.
type Authenticator func(*http.Request) (*channel.Principal, error)

// ErrUnauthenticated is what an [Authenticator] returns to refuse a caller.
// It is the only error of a verifier that becomes a 401.
var ErrUnauthenticated = errors.New("bonnie: channel/http: unauthenticated")

// WithAuthenticator verifies every request except `GET /bonnie/v1/health`,
// and makes the principal it returns the run's identity.
//
// With an authenticator set, the `auth` field of a request body is IGNORED
// rather than merged: a caller must not be able to add claims to, or
// override, an identity the verifier established. Without one, the body's
// `auth` is recorded as before — unverified, self-asserted, and trusted only
// as far as the deployment's own network boundary — and `operation_id` is
// refused outright, because an idempotency key with no proven owner is a way
// to read another caller's run by guessing the key.
//
// Health stays public so a deployment probe needs no credential; it reports
// that the process is up and nothing about any run.
func WithAuthenticator(fn Authenticator) Option {
	return func(c *Channel) { c.auth = fn }
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
		{Method: http.MethodPost, Path: p + "/addresses/{address}", Handler: c.handleEnsureAddress},
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
// serves other things. The outbound registry is empty rather than nil: a
// standalone handler has no channels to hand work to, and a handler that
// asks gets "not mounted", not a panic.
func (c *Channel) Handler() http.Handler {
	return c.HandlerWithOutbound(noOutbound{})
}

// noOutbound is the empty registry: nothing is mounted beside this
// handler.
type noOutbound struct{}

func (noOutbound) To(string) (channel.Receiver, bool) { return nil, false }

// HandlerWithOutbound is [Channel.Handler] with a registry of the other
// mounted channels, for a host that serves hand-offs. A nil registry is
// the empty one.
func (c *Channel) HandlerWithOutbound(out channel.Outbound) http.Handler {
	if out == nil {
		out = noOutbound{}
	}
	mux := http.NewServeMux()
	for _, route := range c.Routes() {
		handler := route.Handler
		public := route.Path == channel.APIPrefix+"/health"
		mux.HandleFunc(route.Method+" "+route.Path, func(w http.ResponseWriter, r *http.Request) {
			if !public && !c.authenticate(w, r) {
				return
			}
			handler(w, r, c, out)
		})
	}
	return mux
}

// principalKey carries the verified principal from the mux wrapper to the
// handler. It is request-scoped and never leaves this package: a handler
// reads it with [verifiedPrincipal], and nothing can put one there from
// outside.
type principalKey struct{}

// authenticate runs the configured [Authenticator] and stores what it
// proved on the request. It reports whether the request may proceed, and
// writes the refusal itself when it may not.
func (c *Channel) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if c.auth == nil {
		return true
	}
	p, err := c.auth(r)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		writeJSON(w, http.StatusUnauthorized, ErrorResponse{
			Error: "the request carries no identity this deployment accepts",
			Code:  errUnauthenticated,
		})
		return false
	case err != nil:
		// The verifier itself broke. That is not proof the caller is an
		// impostor, and answering 401 would send a legitimate client off
		// to re-authenticate against a fault it cannot fix.
		logInternal("authenticator", err)
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: "the request could not be authenticated",
			Code:  errInternal,
		})
		return false
	}
	if p != nil {
		*r = *r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
	}
	return true
}

// verifiedPrincipal returns the principal the [Authenticator] proved for
// this request, and whether an authenticator ran at all. A handler uses the
// second result to tell "verified, anonymous" from "never verified".
func (c *Channel) verifiedPrincipal(r *http.Request) (*channel.Principal, bool) {
	if c.auth == nil {
		return nil, false
	}
	p, _ := r.Context().Value(principalKey{}).(*channel.Principal)
	return p, true
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
	// OperationID makes the start idempotent: the same operation ID from
	// the same authenticated principal resolves to the run the first call
	// created, instead of creating another. A client that times out and
	// retries gets one run, not two. It needs an authenticated principal —
	// an idempotency key with no owner would let one caller read another's
	// run by guessing the key.
	OperationID string `json:"operation_id,omitempty"`
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

// ErrorResponse is the body of every non-2xx reply. Code is the stable
// machine-readable one; Error is the human-readable one and may change
// wording between versions. A client switches on Code.
type ErrorResponse struct {
	Error string `json:"error"`
	// Code is one of the Err* constants of this package, stable across
	// versions: "run_not_found", "invalid_run_id", "run_not_waiting",
	// "run_active", "run_not_active", "run_retired",
	// "run_owned_elsewhere", "compaction_unsupported",
	// "unknown_turn_policy", "client_closed", "bad_request",
	// "too_large", "unauthenticated", "internal".
	Code string `json:"code"`
}

// The codes writeError maps to. They are values, not an enum, so the JSON
// body never carries a Go type name.
const (
	errNotFound           = "run_not_found"
	errInvalidID          = "invalid_run_id"
	errNotWaiting         = "run_not_waiting"
	errActive             = "run_active"
	errNotActive          = "run_not_active"
	errRetired            = "run_retired"
	errOwnedElsewhere     = "run_owned_elsewhere"
	errCompactUnsupported = "compaction_unsupported"
	errUnknownPolicy      = "unknown_turn_policy"
	errClientClosed       = "client_closed"
	errBadRequest         = "bad_request"
	errTooLarge           = "too_large"
	errUnauthenticated    = "unauthenticated"
	errInternal           = "internal"
)

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
func (c *Channel) handleHealth(w http.ResponseWriter, _ *http.Request, _ channel.Inbound, _ channel.Outbound) {
	writeJSON(w, http.StatusOK, healthResponse{OK: true, Status: "ready"})
}

func (c *Channel) handleInfo(w http.ResponseWriter, _ *http.Request, _ channel.Inbound, _ channel.Outbound) {
	writeJSON(w, http.StatusOK, c.info)
}

func (c *Channel) handleStart(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
	var req StartRequest
	if !decode(w, r, &req) {
		return
	}
	auth, verified := c.principalFor(r, req.Auth)
	if req.OperationID != "" {
		// Idempotency is an ownership claim, and an unverified body field
		// cannot make one. Without an authenticator any caller could set
		// auth to someone else's identity, guess an operation ID, and be
		// handed that principal's run.
		if !verified {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: "operation_id needs a verified principal: configure an authenticator on the channel, because an idempotency key with no proven owner is a way to read someone else's run",
				Code:  errBadRequest,
			})
			return
		}
		if auth == nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: "operation_id needs an authenticated principal: an idempotency key with no owner is a way to read someone else's run",
				Code:  errBadRequest,
			})
			return
		}
	}

	var sess channel.SessionRef
	switch {
	case req.Address != "":
		sess = in.From(req.Address)
	case req.OperationID != "":
		// The address map is the idempotency store: a namespaced key the
		// principal owns. Resolving through it is create-once by the same
		// rule every address follows, and the binding survives a restart.
		sess = in.From(operationAddress(auth, req.OperationID))
	default:
		sess = in.From(c.core.NewID())
	}

	run, err := sess.Send(r.Context(), req.Text, channel.SendOptions{
		Auth:       auth,
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

// principalFor decides which identity a request carries, and whether it was
// proved. A configured [Authenticator] wins outright: the body's auth is
// dropped, never merged, so a caller cannot add a claim to a verified
// identity. With no authenticator the body's auth is used as before, and
// reported as unverified.
func (c *Channel) principalFor(r *http.Request, body *channel.Principal) (*channel.Principal, bool) {
	if p, verified := c.verifiedPrincipal(r); verified {
		return p, true
	}
	return body, false
}

func (c *Channel) handleAddress(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	runID, ok, err := c.core.Lookup(r.Context(), r.PathValue("address"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "address is not bound", Code: errNotFound})
		return
	}
	cursor := 0
	if p, ok := c.core.Runner().Journal().(runtime.Positioner); ok {
		cursor, _ = p.Position(r.Context(), runID)
	}
	writeJSON(w, http.StatusOK, RunResponse{RunID: runID, Cursor: cursor})
}

// handleEnsureAddress resolves an address to its run, creating and binding
// one when the address is new, and runs no turn.
//
// It exists so a client can subscribe before it speaks. A turn's reasoning
// deltas and tool events are live-only — the journal holds the conversation,
// not the mid-turn deltas (see [runtime.Runner.StreamEvents]) — so a client
// that learns its run ID from the reply to its first message has already
// missed that turn's events, and no replay can recover them. POST here first,
// open the stream, then send.
//
// It is idempotent: an address that already owns a run returns that run and
// binds nothing new.
func (c *Channel) handleEnsureAddress(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
	runID, err := in.From(r.PathValue("address")).RunID(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	cursor := 0
	if p, ok := c.core.Runner().Journal().(runtime.Positioner); ok {
		cursor, _ = p.Position(r.Context(), runID)
	}
	writeJSON(w, http.StatusOK, RunResponse{RunID: runID, Cursor: cursor})
}

func (c *Channel) handleGet(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
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

func (c *Channel) handleSend(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
	var req SendRequest
	if !decode(w, r, &req) {
		return
	}
	auth, _ := c.principalFor(r, req.Auth)
	// Attach, not From: a message addressed to a run ID must never create one.
	sess := in.Attach(r.PathValue("id"))
	run, err := sess.Send(r.Context(), req.Text, channel.SendOptions{
		Auth:       auth,
		TurnPolicy: req.TurnPolicy,
		Context:    req.Context,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse(run))
}

func (c *Channel) handleRespond(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
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

func (c *Channel) handleCancel(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
	if err := in.Attach(r.PathValue("id")).Cancel(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleReset retires the run. The route is ID-addressed, so it retires
// that run and no other; the address that pointed at it is freed by the
// core, and a later start on that address creates a fresh run.
func (c *Channel) handleReset(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
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

func (c *Channel) handleClear(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
	if err := in.Attach(r.PathValue("id")).Clear(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Channel) handleCompact(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
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
func (c *Channel) handleStream(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
	runID, ok := attachedRunID(w, r, in)
	if !ok {
		return
	}

	cursor := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "cursor must be a non-negative integer", Code: errBadRequest})
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
				ErrorResponse{Error: fmt.Sprintf("request body is larger than %d bytes", maxRequestBody), Code: errTooLarge})
			return false
		}
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "malformed JSON body: " + err.Error(), Code: errBadRequest})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError maps a runtime sentinel to a status code and a stable code.
// Anything unrecognised is a 500: guessing would hide a real fault behind a
// plausible 4xx.
//
// The mapped errors carry their own text to the client on purpose — "run not
// found", "run is not waiting" is what the caller needs to act. An unmapped
// one does not: it is whatever the journal, the driver, or the model client
// said, and that text has carried file paths and SQL to whoever could reach
// the API. The client gets the code and nothing else; the operator gets the
// detail on stderr.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrRunNotFound):
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error(), Code: errNotFound})
	case errors.Is(err, runtime.ErrInvalidRunID):
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error(), Code: errInvalidID})
	case errors.Is(err, runtime.ErrNotWaiting):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: errNotWaiting})
	case errors.Is(err, runtime.ErrRunActive):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: errActive})
	case errors.Is(err, runtime.ErrRunNotActive):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: errNotActive})
	case errors.Is(err, runtime.ErrRunRetired):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: errRetired})
	case errors.Is(err, runtime.ErrRunOwnedElsewhere):
		// Another process owns this run's journal. That is a conflict
		// over who may write, not a fault in this server, and a client
		// that reads 500 would retry a request no retry can fix.
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: errOwnedElsewhere})
	case errors.Is(err, runtime.ErrCompactionUnsupported):
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: err.Error(), Code: errCompactUnsupported})
	case errors.Is(err, channel.ErrUnknownTurnPolicy):
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error(), Code: errUnknownPolicy})
	case errors.Is(err, context.Canceled):
		writeJSON(w, 499, ErrorResponse{Error: err.Error(), Code: errClientClosed})
	default:
		logInternal("request", err)
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: "the request failed; the server log has the detail",
			Code:  errInternal,
		})
	}
}

// logInternal reports a fault to the operator. It is the other half of an
// opaque 500: the detail has to go somewhere, and stderr is where every
// other adapter in this framework already reports a delivery it could not
// make.
func logInternal(what string, err error) {
	fmt.Fprintf(os.Stderr, "bonnie: channel/http: %s: %v\n", what, err)
}

// opPrefix namespaces idempotency keys inside the address map, so an
// operation ID cannot collide with a channel-local address a client chose.
const opPrefix = "operation/"

// operationAddress is the address-map key of one idempotent start: the
// principal's identity, the fixed prefix, and the caller's operation ID. A
// key another principal chose cannot resolve to this one's run.
//
// That property holds only as far as the identity does. It is real when an
// [Authenticator] proved the principal, which is why handleStart refuses an
// operation ID without one: a self-asserted principal makes the namespace a
// formality any caller can step into.
func operationAddress(p *channel.Principal, operationID string) string {
	return opPrefix + p.Authenticator + "/" + p.ID + "/" + operationID
}
