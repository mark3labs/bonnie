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
	"net/http"

	"github.com/mark3labs/bonnie/runtime"
)

// TurnPolicy decides what happens when a message arrives while a turn is
// already running for the same address.
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
	// Title names the run for operator-facing listings.
	Title string
}

// SessionRef is a handle to the run that serves an address.
//
// Every method resolves the run first, so a reference stays correct after the
// address is re-keyed. A reference from [Inbound.Attach] returns
// [runtime.ErrRunNotFound] for an unknown run instead of creating one.
type SessionRef interface {
	// Send delivers a message and returns the run at its next boundary.
	Send(ctx context.Context, text string, opts SendOptions) (*runtime.Run, error)
	// Respond answers a suspended run.
	Respond(ctx context.Context, responses []runtime.InputResponse) (*runtime.Run, error)
	// Cancel stops the active turn.
	Cancel(ctx context.Context) error
	// RunID returns the durable run this reference currently resolves to.
	RunID(ctx context.Context) (string, error)
}

// Inbound is handed to a channel when traffic arrives.
type Inbound interface {
	// From resolves a channel-local address to whichever run owns it now,
	// creating one when the address is new. It is dynamic: two calls may
	// resolve to different runs if the address was re-keyed in between.
	From(address string) SessionRef
	// Attach targets one exact run ID. It never creates, follows, or
	// replaces.
	Attach(runID string) SessionRef
}

// Route binds an HTTP method and path to a handler.
type Route struct {
	Method  string
	Path    string
	Handler func(w http.ResponseWriter, r *http.Request, in Inbound)
}

// Channel is an inbound transport.
type Channel interface {
	// Name identifies the channel in logs and configuration.
	Name() string
	// Routes returns the HTTP surface the channel needs mounted.
	Routes() []Route
}
