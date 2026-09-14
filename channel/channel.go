// Package channel defines BONNIE's inbound transport abstraction (L3).
//
// A channel is an edge adapter with three jobs:
//
//  1. Normalise platform input into an agent message.
//  2. Own the channel-local address that maps a platform conversation
//     (a Slack thread, a GitHub PR, an HTTP session cookie) to the durable
//     run that currently serves it.
//  3. Decide delivery: how, where, and whether a response goes back.
//
// After normalisation every channel runs the same agent, so tools and
// instructions stay channel-agnostic.
//
// This package is interfaces only. Concrete adapters live in subpackages so a
// host that only needs HTTP does not compile a Slack SDK.
package channel

import (
	"context"
	"errors"
	"net/http"

	"github.com/mark3labs/bonnie/runtime"
)

// ReservedPathPrefix is the URL namespace BONNIE keeps for itself. The
// framework's own API — the HTTP channel, health, info — is mounted under
// [APIPrefix]; every other channel is refused a route that starts with this
// prefix, so a custom adapter cannot shadow the framework or collide with a
// route a later BONNIE version adds. eve makes the same promise with
// `/eve/v1/*`.
//
// The webhook paths of the chat adapters (`/slack/events`, `/telegram`) are
// not framework API: they are configured in the platform's console and stay
// where they are.
const ReservedPathPrefix = "/bonnie/"

// APIPrefix is where the framework HTTP API lives. The version segment is
// the wire contract: a breaking change to a route or a body bumps it, and
// clients built against `v1` keep working until `v1` is removed.
const APIPrefix = "/bonnie/v1"

// ErrReservedPath is returned when a channel that is not the framework's
// asks for a route under [ReservedPathPrefix]. The host refuses to serve
// rather than let the mux decide who wins.
var ErrReservedPath = errors.New("bonnie: channel: route under the reserved /bonnie/ namespace")

// TurnPolicy decides what happens when a message arrives while a turn is
// already running for the same address.
//
// The vocabulary is exactly [PolicySteer] and [PolicyQueue]. An adapter that
// receives anything else must refuse it with [ErrUnknownTurnPolicy] rather
// than guess: a typo silently falling back to one of the two would make the
// same message mean different things on different transports.
type TurnPolicy string

const (
	// PolicySteer injects the message into the turn that is already running,
	// so the next step of the agent loop sees it. This is the default and
	// matches an interactive chat feel: the turn keeps the work it has done
	// instead of starting over.
	//
	// Stopping a turn outright is a separate act — [SessionRef.Cancel].
	PolicySteer TurnPolicy = "steer"
	// PolicyQueue lets the active turn finish, then runs the next message.
	PolicyQueue TurnPolicy = "queue"
)

// ErrUnknownTurnPolicy is returned when a [SendOptions.TurnPolicy] is
// neither [PolicySteer] nor [PolicyQueue]. Every adapter refuses it; none
// silently falls back to a default.
var ErrUnknownTurnPolicy = errors.New("bonnie: channel: unknown turn policy")

// Principal identifies who is on the other end of a channel. It is carried
// into the run so tools can make per-tenant authorisation decisions.
type Principal struct {
	// Authenticator names the verifier that produced this principal.
	Authenticator string
	// Kind is "user", "app", or "runtime".
	Kind string
	// ID is stable for the lifetime of the identity.
	ID string
	// Attributes carries profile claims such as email or team.
	Attributes map[string]any
}

// SendOptions tunes a single inbound message.
type SendOptions struct {
	Auth       *Principal
	TurnPolicy TurnPolicy
	// Title names the run for operator-facing listings. It is recorded on
	// the run's first turn and ignored after that.
	Title string
	// Context is what the model should know for this turn only: the event
	// that fired, the diff a comment refers to, who is speaking. It is
	// shown to the model in front of the text and never becomes
	// conversation history. See [runtime.Input].
	Context []string
	// Kind is the kind of surface the address names: a DM, a thread, an
	// issue. It is recorded on the run's first turn so instructions and
	// tools can tell where the conversation lives. The chat package owns
	// the vocabulary.
	Kind string
}

// SessionRef is a handle to the run that serves an address.
//
// Every method resolves the run first, so a reference stays correct after the
// address is re-keyed. A reference from [Inbound.Attach] returns
// [runtime.ErrRunNotFound] for an unknown run instead of creating one.
//
// Only Send creates. The controls — Cancel, Reset, Clear, Compact — never
// do: on an address that owns no run they return nil and change nothing,
// which is what a stray "/new" in a thread the agent never joined should
// cost.
type SessionRef interface {
	// Send delivers a message and returns the run at its next boundary.
	Send(ctx context.Context, text string, opts SendOptions) (*runtime.Run, error)
	// Respond answers a suspended run.
	Respond(ctx context.Context, responses []runtime.InputResponse) (*runtime.Run, error)
	// Cancel stops the active turn.
	Cancel(ctx context.Context) error
	// Reset retires the run for good and frees its address, so the next
	// Send on the address starts a fresh run. A reference from Attach keeps
	// pointing at the retired run.
	Reset(ctx context.Context, reason string) error
	// Clear drops the conversation from the model's context and keeps
	// everything else: the run ID, the address, the workspace, the journal.
	Clear(ctx context.Context) error
	// Compact summarises the run's older messages now, without a user
	// message.
	Compact(ctx context.Context) error
	// RunID returns the durable run this reference currently resolves to.
	RunID(ctx context.Context) (string, error)
}

// Inbound is handed to a channel when traffic arrives.
//
// Addresses are channel-local: the framework prefixes every one with the
// channel's name before it reaches the address map, so two channels cannot
// bind the same key by accident and an adapter never spells its own prefix.
// eve does the same with its continuation tokens.
type Inbound interface {
	// From resolves a channel-local address to whichever run owns it now,
	// creating one on Send when the address is new. It is dynamic: two calls
	// may resolve to different runs if the address was re-keyed in between.
	From(address string) SessionRef
	// Attach targets one exact run ID. It never creates, follows, or
	// replaces.
	Attach(runID string) SessionRef
}

// Route binds an HTTP method and path to a handler.
type Route struct {
	Method  string
	Path    string
	Handler func(w http.ResponseWriter, r *http.Request, in Inbound, out Outbound)
}

// Outbound is handed to a channel's route handlers next to [Inbound]: it
// reaches the channels mounted beside this one. A handler that needs none
// ignores the parameter.
//
// Calling another channel is an agent hand-off, not a notification: the
// message becomes turn input and the model runs on the destination
// channel. A caller that only wants to post text calls the platform's API;
// a notification that must survive a crash goes through an outbox of its
// own.
type Outbound interface {
	// To returns the named channel's [Receiver], or false when no channel
	// with that name is mounted.
	To(name string) (Receiver, bool)
}

// Receiver is what a mounted channel exposes to the others: start a
// conversation on its surface without an inbound message. The destination
// channel owns what target means (a Slack channel ID, a GitHub issue),
// creates whatever surface the reply needs, and binds the address before
// the turn runs, so a platform event that arrives while the turn is in
// flight continues this run instead of racing it. The options carry the
// initiating principal, so the destination run records who started it.
type Receiver interface {
	Receive(ctx context.Context, target any, text string, opts SendOptions) error
}

// Channel is an inbound transport.
type Channel interface {
	// Name identifies the channel in logs and configuration.
	Name() string
	// Routes returns the HTTP surface the channel needs mounted.
	Routes() []Route
}
