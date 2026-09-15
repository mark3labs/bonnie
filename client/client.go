// Package client is a Go client for BONNIE's HTTP wire API.
//
// It speaks the contract [github.com/mark3labs/bonnie/channel/http] serves
// under `/bonnie/v1`: start a run, send it a message, answer a suspension,
// cancel a turn, and stream what happens as newline-delimited JSON.
//
// The package is deliberately terminal-free and transport-only. It holds no
// conversation state and renders nothing, so a TUI, a test, a webhook
// bridge, or another service can each build what they need on top. BONNIE's
// own terminal UI is one of its callers, which is what keeps the client
// honest: a capability the wire cannot express is one the TUI cannot show.
//
// # Streams and cursors
//
// [Client.Stream] takes the cursor of the last event a caller saw and
// returns everything after it, replayed from the journal when the live
// backlog no longer reaches. A client that wants to miss nothing learns its
// run ID with [Client.Ensure] first, opens the stream, and only then sends:
// a turn's reasoning and tool events are live-only, and no replay brings
// back the ones that happened before anyone subscribed.
//
// # Identity
//
// The server decides whether a request needs a credential. Give the client
// one with [WithBearerToken] or [WithHeader], and it goes on every request.
// Against a server with no authenticator configured, a client with no
// credential works — which is the loopback `bonnie dev` case.
package client

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
// same value as channel.APIPrefix, spelled out because this package is a
// client of the wire rather than of the channel package: importing the
// constant would tie a client build to the server's package tree.
const apiPrefix = "/bonnie/v1"

// ErrNotFound is returned when the server answers 404 — the run or address
// does not exist. A caller that asked whether a conversation exists reads it
// as "not yet", not as a failure.
var ErrNotFound = errors.New("bonnie: client: not found")

// ErrUnauthenticated is returned when the server answers 401. The
// credential is missing, expired, or not one this deployment accepts.
var ErrUnauthenticated = errors.New("bonnie: client: unauthenticated")

// Client talks to one BONNIE HTTP channel. It is safe for concurrent use,
// and reuses one underlying connection pool.
type Client struct {
	base    string
	hc      *http.Client
	headers map[string]string

	mu           sync.Mutex
	streamCancel context.CancelFunc
	streamGen    uint64
}

// Option configures a [Client].
type Option func(*Client)

// WithHTTPClient replaces the underlying HTTP client. Use it to set a
// proxy, a transport, or a timeout.
//
// The default client has NO timeout, and that is deliberate: a stream stays
// open while a run waits for a human, which may be days. A timeout set here
// applies to the stream as well, so bound the request context instead when
// only the short calls should expire.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.hc = hc
		}
	}
}

// WithHeader sends a header on every request. Use it for whatever scheme
// the deployment's authenticator reads.
func WithHeader(name, value string) Option {
	return func(c *Client) { c.headers[name] = value }
}

// WithBearerToken sends "Authorization: Bearer <token>" on every request.
func WithBearerToken(token string) Option {
	return WithHeader("Authorization", "Bearer "+token)
}

// New returns a client for the channel at base, for example
// "http://127.0.0.1:8080". A trailing slash is ignored.
func New(base string, opts ...Option) *Client {
	c := &Client{
		base:    strings.TrimRight(base, "/"),
		hc:      &http.Client{Timeout: 0},
		headers: make(map[string]string),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Info is what `GET /bonnie/v1/info` reports.
type Info struct {
	Agent    string   `json:"agent,omitempty"`
	Version  string   `json:"version"`
	Channels []string `json:"channels"`
}

// Health is what `GET /bonnie/v1/health` reports.
type Health struct {
	OK     bool   `json:"ok"`
	Status string `json:"status"`
}

// runResponse is the wire form of a run at a turn boundary.
type runResponse struct {
	RunID    string                  `json:"run_id"`
	Cursor   int                     `json:"cursor,omitempty"`
	State    runtime.RunState        `json:"state"`
	Response string                  `json:"response,omitempty"`
	Suspend  *runtime.SuspendRequest `json:"suspend,omitempty"`
}

func (r runResponse) toRun() *runtime.Run {
	return &runtime.Run{ID: r.RunID, State: r.State, Response: r.Response, Suspend: r.Suspend}
}

// Health reports whether the server is up and routing. It needs no
// credential: it says nothing about any run.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.get(ctx, apiPrefix+"/health", &out)
	return out, err
}

// Info reports the agent's name, the BONNIE version, and the mounted
// channels.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.get(ctx, apiPrefix+"/info", &out)
	return out, err
}

// Lookup returns the run bound to an address and the journal cursor it is
// at, without creating anything. An address that owns no run answers
// [ErrNotFound].
func (c *Client) Lookup(ctx context.Context, address string) (string, int, error) {
	var out runResponse
	if err := c.get(ctx, apiPrefix+"/addresses/"+url.PathEscape(address), &out); err != nil {
		return "", 0, err
	}
	return out.RunID, out.Cursor, nil
}

// Ensure resolves an address to its run, creating and binding one when the
// address is new, and runs no turn.
//
// It is how a client learns its run ID before it sends anything, so the
// stream is open in time to see the first turn's reasoning and tool events.
// Those are live-only and no replay brings them back.
func (c *Client) Ensure(ctx context.Context, address string) (string, int, error) {
	var out runResponse
	if err := c.post(ctx, apiPrefix+"/addresses/"+url.PathEscape(address), nil, &out); err != nil {
		return "", 0, err
	}
	return out.RunID, out.Cursor, nil
}

// Start begins a run for an address, or continues it — the channel resolves
// the address to the same run on every call, so one address is one
// conversation. The returned run reports the turn's outcome.
func (c *Client) Start(ctx context.Context, address, text string) (*runtime.Run, error) {
	var out runResponse
	err := c.post(ctx, apiPrefix+"/runs", map[string]any{"address": address, "text": text}, &out)
	if err != nil {
		return nil, err
	}
	return out.toRun(), nil
}

// Send delivers a message to an existing run as a new turn. It never
// creates: an unknown run ID answers [ErrNotFound].
func (c *Client) Send(ctx context.Context, runID, text string) (*runtime.Run, error) {
	var out runResponse
	err := c.post(ctx, apiPrefix+"/runs/"+url.PathEscape(runID), map[string]any{"text": text}, &out)
	if err != nil {
		return nil, err
	}
	return out.toRun(), nil
}

// Respond answers a suspended run with text. The text is the answer to the
// prompt the run's [runtime.SuspendRequest] carries.
//
// Use [Client.RespondWith] to answer an approval, which needs a verdict and
// not only words.
func (c *Client) Respond(ctx context.Context, runID, text string) (*runtime.Run, error) {
	return c.RespondWith(ctx, runID, []runtime.InputResponse{{Text: text}})
}

// RespondWith answers a suspended run with whatever the suspension asked
// for: text for a question, a verdict for an approval.
//
//	_, err := c.RespondWith(ctx, runID, []runtime.InputResponse{
//		runtime.Reject("that would drop the production table"),
//	})
//
// An approval carries its verdict separately from its words, so "no, and
// here is why" reaches the agent as a refusal with a reason rather than as
// prose it has to interpret.
func (c *Client) RespondWith(ctx context.Context, runID string, responses []runtime.InputResponse) (*runtime.Run, error) {
	var out runResponse
	err := c.post(ctx, apiPrefix+"/runs/"+url.PathEscape(runID)+"/respond",
		map[string]any{"responses": responses}, &out)
	if err != nil {
		return nil, err
	}
	return out.toRun(), nil
}

// Get reports a run's durable state without changing it.
func (c *Client) Get(ctx context.Context, runID string) (*runtime.Run, error) {
	var out runResponse
	if err := c.get(ctx, apiPrefix+"/runs/"+url.PathEscape(runID), &out); err != nil {
		return nil, err
	}
	return out.toRun(), nil
}

// Cancel stops the turn a run is executing. A run with no turn in flight
// answers a conflict.
func (c *Client) Cancel(ctx context.Context, runID string) error {
	return c.post(ctx, apiPrefix+"/runs/"+url.PathEscape(runID)+"/cancel", nil, nil)
}

// Reset retires the run for good and frees the address that pointed at it,
// so the next Start on that address begins a fresh conversation.
func (c *Client) Reset(ctx context.Context, runID, reason string) error {
	var body any
	if reason != "" {
		body = map[string]any{"reason": reason}
	}
	return c.post(ctx, apiPrefix+"/runs/"+url.PathEscape(runID)+"/reset", body, nil)
}

// Clear drops the conversation from the model's context and keeps
// everything else: the run ID, the address, the journal.
func (c *Client) Clear(ctx context.Context, runID string) error {
	return c.post(ctx, apiPrefix+"/runs/"+url.PathEscape(runID)+"/clear", nil, nil)
}

// Compact summarises the run's older messages now, without a user message.
// A server whose agent cannot compact answers 501.
func (c *Client) Compact(ctx context.Context, runID string) error {
	return c.post(ctx, apiPrefix+"/runs/"+url.PathEscape(runID)+"/compact", nil, nil)
}

// Stream yields a run's events after the given cursor. The returned
// function unsubscribes and closes the channel; call it when done, or the
// connection stays open.
//
// The stream is durable: it replays from the journal up to the live edge,
// then stays open. A run parked on a human's answer holds it open for as
// long as the answer takes.
func (c *Client) Stream(ctx context.Context, runID string, after int) (<-chan runtime.Event, func(), error) {
	target := c.base + apiPrefix + "/runs/" + url.PathEscape(runID) + "/stream?cursor=" + strconv.Itoa(after)
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
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
	if err != nil {
		unsubscribe()
		return nil, nil, err
	}
	c.applyHeaders(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		unsubscribe()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		unsubscribe()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, runID)
		case http.StatusUnauthorized:
			return nil, nil, fmt.Errorf("%w: %s", ErrUnauthenticated, runID)
		}
		return nil, nil, fmt.Errorf("bonnie: client: stream: %s", strings.TrimSpace(string(body)))
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

// CloseStreams closes the active event stream. A host uses it before it
// stops a server it is about to replace, so a graceful shutdown does not
// wait on a connection that will reconnect anyway.
func (c *Client) CloseStreams() {
	c.mu.Lock()
	cancel := c.streamCancel
	c.streamCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Client) applyHeaders(req *http.Request) {
	for name, value := range c.headers {
		req.Header.Set(name, value)
	}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("bonnie: client: request: %w", err)
	}
	return c.do(req, path, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("bonnie: client: encode: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(string(b)))
	if err != nil {
		return fmt.Errorf("bonnie: client: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, path, out)
}

func (c *Client) do(req *http.Request, path string, out any) error {
	c.applyHeaders(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("bonnie: client: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		switch resp.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, path)
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: %s", ErrUnauthenticated, path)
		}
		return fmt.Errorf("bonnie: client: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("bonnie: client: decode: %w", err)
		}
	}
	return nil
}
