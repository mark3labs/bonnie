// Package tui is the BONNIE terminal user interface.
//
// It is an HTTP client of the same channel a server exposes, so the transcript
// a developer sees is the transcript any client sees: one durable run, streamed
// from the journal. The model never imports the runtime's internals beyond the
// public types it already reaches for — it talks to the wire the channel
// defines, which is what makes the TUI usable against any running bonnie
// server, not only the one `dev` started.
package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/mark3labs/bonnie/runtime"
)

// apiPrefix is the framework namespace the channel serves under. It is the
// same value as channel.APIPrefix; the TUI spells it out because it is a
// client of the wire, not of the channel package.
const apiPrefix = "/bonnie/v1"

// Client is the slice of a bonnie HTTP channel the TUI drives. It is an
// interface so tests can inject a fake and drive a transcript without a
// network round trip.
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
// error.
var ErrNotFound = errors.New("bonnie: chat: not found")

// runResponse is the wire form of a run at a turn boundary. It mirrors the
// channel/http JSON, which is the contract the TUI is a client of.
type runResponse struct {
	RunID    string                  `json:"run_id"`
	Cursor   int                     `json:"cursor,omitempty"`
	State    runtime.RunState        `json:"state"`
	Response string                  `json:"response,omitempty"`
	Suspend  *runtime.SuspendRequest `json:"suspend,omitempty"`
}

// HTTP is a [Client] over a running bonnie HTTP channel.
type HTTP struct {
	base string
	hc   *http.Client

	mu           sync.Mutex
	streamCancel context.CancelFunc
	streamGen    uint64
}

// NewHTTP returns a [Client] that talks to the channel at base, for example
// "http://127.0.0.1:8080". The client is reused for every request.
func NewHTTP(base string, hc *http.Client) *HTTP {
	if hc == nil {
		hc = &http.Client{Timeout: 0}
	}
	return &HTTP{base: strings.TrimRight(base, "/"), hc: hc}
}

// Lookup implements [Client].
func (c *HTTP) Lookup(ctx context.Context, address string) (string, int, error) {
	var out runResponse
	if err := c.get(ctx, apiPrefix+"/addresses/"+url.PathEscape(address), &out); err != nil {
		return "", 0, err
	}
	return out.RunID, out.Cursor, nil
}

// Ensure implements [Client].
func (c *HTTP) Ensure(ctx context.Context, address string) (string, int, error) {
	var out runResponse
	if err := c.post(ctx, apiPrefix+"/addresses/"+url.PathEscape(address), nil, &out); err != nil {
		return "", 0, err
	}
	return out.RunID, out.Cursor, nil
}

// Start implements [Client].
func (c *HTTP) Start(ctx context.Context, address, text string) (*runtime.Run, error) {
	var out runResponse
	err := c.post(ctx, apiPrefix+"/runs", map[string]any{"address": address, "text": text}, &out)
	if err != nil {
		return nil, err
	}
	return out.toRun(), nil
}

// Send implements [Client].
func (c *HTTP) Send(ctx context.Context, runID, text string) (*runtime.Run, error) {
	var out runResponse
	err := c.post(ctx, apiPrefix+"/runs/"+runID, map[string]any{"text": text}, &out)
	if err != nil {
		return nil, err
	}
	return out.toRun(), nil
}

// Respond implements [Client].
func (c *HTTP) Respond(ctx context.Context, runID, text string) (*runtime.Run, error) {
	var out runResponse
	err := c.post(ctx, apiPrefix+"/runs/"+runID+"/respond",
		map[string]any{"responses": []map[string]any{{"text": text}}}, &out)
	if err != nil {
		return nil, err
	}
	return out.toRun(), nil
}

// Cancel implements [Client].
func (c *HTTP) Cancel(ctx context.Context, runID string) error {
	return c.post(ctx, apiPrefix+"/runs/"+runID+"/cancel", nil, nil)
}

// Stream implements [Client].
func (c *HTTP) Stream(ctx context.Context, runID string, after int) (<-chan runtime.Event, func(), error) {
	url := c.base + apiPrefix + "/runs/" + runID + "/stream?cursor=" + strconv.Itoa(after)
	requestCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.streamGen++
	gen := c.streamGen
	c.streamCancel = cancel
	c.mu.Unlock()
	unsubscribe := func() {
		cancel()
		c.mu.Lock()
		if c.streamGen == gen {
			c.streamCancel = nil
		}
		c.mu.Unlock()
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
	if err != nil {
		unsubscribe()
		return nil, nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		unsubscribe()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		unsubscribe()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, runID)
		}
		return nil, nil, fmt.Errorf("bonnie: chat: stream: %s", strings.TrimSpace(string(body)))
	}

	ch := make(chan runtime.Event)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		dec := json.NewDecoder(bufio.NewReader(resp.Body))
		for {
			var ev runtime.Event
			if err := dec.Decode(&ev); err != nil {
				return
			}
			select {
			case ch <- ev:
			case <-requestCtx.Done():
				return
			}
		}
	}()

	return ch, unsubscribe, nil
}

// CloseStreams closes the active event stream. The dev loop uses it before it
// stops a child, so graceful HTTP shutdown does not wait on the TUI connection
// that will reconnect to the next child.
func (c *HTTP) CloseStreams() {
	c.mu.Lock()
	cancel := c.streamCancel
	c.streamCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *HTTP) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("bonnie: chat: request: %w", err)
	}
	return c.do(req, path, out)
}

func (c *HTTP) post(ctx context.Context, path string, body, out any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("bonnie: chat: encode: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(string(b)))
	if err != nil {
		return fmt.Errorf("bonnie: chat: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, path, out)
}

func (c *HTTP) do(req *http.Request, path string, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("bonnie: chat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return fmt.Errorf("bonnie: chat: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("bonnie: chat: decode: %w", err)
		}
	}
	return nil
}

func (r runResponse) toRun() *runtime.Run {
	return &runtime.Run{ID: r.RunID, State: r.State, Response: r.Response, Suspend: r.Suspend}
}
