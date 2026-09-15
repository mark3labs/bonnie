// Package tui is the BONNIE terminal user interface.
//
// It is an HTTP client of the same channel a server exposes, so the transcript
// a developer sees is the transcript any client sees: one durable run, streamed
// from the journal. The model never imports the runtime's internals beyond the
// public types it already reaches for — it talks to the wire the channel
// defines, which is what makes the TUI usable against any running bonnie
// server, not only the one `dev` started.
//
// The wire is spoken by [github.com/mark3labs/bonnie/client], which is a
// public package precisely so this is true: the TUI is one caller of the
// same client anyone else would use, not a privileged path into the server.
// What remains here is the [Client] interface — the slice of that API the
// terminal model drives — so a test can substitute a fake and play a
// transcript without a network round trip.
package tui

import (
	"context"
	"net/http"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
)

// Client is the slice of a bonnie HTTP channel the TUI drives. It is an
// interface so tests can inject a fake and drive a transcript without a
// network round trip. [github.com/mark3labs/bonnie/client.Client] satisfies
// it.
type Client interface {
	// Lookup returns the run and current journal cursor bound to an address
	// without creating one.
	Lookup(ctx context.Context, address string) (string, int, error)

	// Ensure resolves an address to its run, creating and binding one when
	// the address is new, and runs no turn. It is how the TUI learns its run
	// ID before it sends anything, so the stream is open in time to see the
	// first turn's reasoning and tool events — those are live-only and no
	// replay brings them back.
	Ensure(ctx context.Context, address string) (string, int, error)

	// Start begins a run for an address, or continues it — the channel
	// resolves the address to the same run on every call, so a TUI session
	// is one conversation. The returned run reports the turn's outcome.
	Start(ctx context.Context, address, text string) (*runtime.Run, error)

	// Send delivers a message to an existing run as a new turn.
	Send(ctx context.Context, runID, text string) (*runtime.Run, error)

	// Respond answers a suspended run.
	Respond(ctx context.Context, runID, text string) (*runtime.Run, error)

	// Cancel stops the turn that is in progress.
	Cancel(ctx context.Context, runID string) error

	// Stream yields a run's events after the given cursor. The returned
	// function unsubscribes and closes the channel.
	Stream(ctx context.Context, runID string, after int) (<-chan runtime.Event, func(), error)
}

// ErrNotFound is returned when the server answers 404 — the run does not
// exist. The TUI turns it into a cue to start a conversation rather than an
// error. It is [client.ErrNotFound]; the alias keeps the TUI's own code
// reading in its own vocabulary.
var ErrNotFound = client.ErrNotFound

// HTTP is the wire client the TUI runs against.
//
// Deprecated: use [github.com/mark3labs/bonnie/client.Client] directly. The
// alias remains so an out-of-tree caller of this package's constructor keeps
// compiling.
type HTTP = client.Client

// NewHTTP returns a client that talks to the channel at base, for example
// "http://127.0.0.1:8080".
//
// Deprecated: use [github.com/mark3labs/bonnie/client.New]. It takes options
// rather than a bare *http.Client, and reaches the whole wire API instead of
// the slice the terminal happens to need.
func NewHTTP(base string, hc *http.Client) *client.Client {
	if hc == nil {
		return client.New(base)
	}
	return client.New(base, client.WithHTTPClient(hc))
}
