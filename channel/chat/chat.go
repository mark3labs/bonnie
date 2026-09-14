// Package chat holds the plumbing every chat-platform channel shares: the
// journalled address map, per-run turn locks, the [channel.SessionRef]
// implementation, and the dispatch rule that sends a message to the right
// runner entry point.
//
// A chat adapter receives messages from a platform — Slack, Discord,
// Telegram — where the conversation surface is one thread that carries
// starts, follow-ups, and answers to parked runs alike. The platform gives
// no way to say "this is a resume"; the adapter cannot know whether the
// next reply should start a turn or answer a suspension. [Route] decides,
// and the decision is why this package exists: a reply to a parked run that
// went to Send would start a fresh turn and leave the suspension waiting
// for ever.
//
// Adapters built on this package join the conformance suite in
// `channeltest`, which drives the [channel.Inbound] contract only — the
// webhook half is platform-specific and tested with fakes beside each
// adapter.
package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// The address map lives in a reserved run so it inherits the journal's
// durability. A process-local map would lose every Slack thread on restart,
// which is the failure this whole layer exists to prevent.
const (
	// AddressRun is the reserved run that carries the address bindings of
	// every channel built on this package.
	AddressRun = runtime.ReservedRunPrefix + "addresses"
	// AddressExtType is the extension-data type of a binding record.
	AddressExtType = "channel.address"
	// PrincipalExtType is the extension-data type of a principal record.
	PrincipalExtType = "channel.principal"
)

// AddressMap maps a channel-local address — a Slack thread, a Telegram chat —
// to the run that serves it, and remembers who last spoke on a run. It is
// journalled: bindings survive a restart.
type AddressMap struct {
	journal runtime.Journal

	mu     sync.Mutex
	byAddr map[string]string
	loaded bool
}

// NewAddressMap returns an address map over a journal.
func NewAddressMap(j runtime.Journal) *AddressMap {
	return &AddressMap{journal: j, byAddr: make(map[string]string)}
}

// addressBinding is the durable form of one address-to-run mapping.
type addressBinding struct {
	Address string `json:"address"`
	RunID   string `json:"run_id"`
}

// principalNote records who spoke on a run. BONNIE carries the identity; the
// platform's signature verified the transport, not the person.
type principalNote struct {
	RunID     string             `json:"run_id"`
	Principal *channel.Principal `json:"principal,omitempty"`
}

// loadLocked replays the reserved run once. The caller must hold m.mu.
func (m *AddressMap) loadLocked(ctx context.Context) error {
	if m.loaded {
		return nil
	}
	recs, err := m.journal.Replay(ctx, AddressRun)
	switch {
	case errors.Is(err, runtime.ErrRunNotFound):
		m.loaded = true
		return nil
	case err != nil:
		return fmt.Errorf("bonnie: load address map: %w", err)
	}

	for _, rec := range recs {
		if rec.ExtType != AddressExtType || len(rec.Payload) == 0 {
			continue
		}
		var b addressBinding
		if err := json.Unmarshal(rec.Payload, &b); err != nil {
			continue
		}
		// Last binding wins, so re-keying an address is just another append.
		m.byAddr[b.Address] = b.RunID
	}
	m.loaded = true
	return nil
}

// Resolve returns the run that serves an address, creating and binding one on
// first sight.
func (m *AddressMap) Resolve(ctx context.Context, address string, newID func() string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.loadLocked(ctx); err != nil {
		return "", err
	}
	if runID, ok := m.byAddr[address]; ok {
		return runID, nil
	}

	runID := newID()
	if err := m.bindLocked(ctx, address, runID); err != nil {
		return "", err
	}
	return runID, nil
}

// Lookup returns the run bound to an address without creating one. A chat
// adapter uses it to drop platform events for threads this channel never
// started — everything in a busy channel is not for the agent.
func (m *AddressMap) Lookup(address string) (string, bool) {
	runID, ok, _ := m.LookupContext(context.Background(), address)
	return runID, ok
}

// LookupContext returns the run bound to an address without creating one and
// reports a journal read failure to callers that can surface it.
func (m *AddressMap) LookupContext(ctx context.Context, address string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(ctx); err != nil {
		return "", false, err
	}
	runID, ok := m.byAddr[address]
	return runID, ok, nil
}

// Bind points an address at a run, replacing any earlier binding. Use it to
// re-key an address — to start a fresh conversation in the same Slack thread,
// for example.
func (m *AddressMap) Bind(ctx context.Context, address, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.loadLocked(ctx); err != nil {
		return err
	}
	return m.bindLocked(ctx, address, runID)
}

func (m *AddressMap) bindLocked(ctx context.Context, address, runID string) error {
	payload, err := json.Marshal(addressBinding{Address: address, RunID: runID})
	if err != nil {
		return fmt.Errorf("bonnie: encode address binding: %w", err)
	}
	if _, err := m.journal.Append(ctx, runtime.Record{
		RunID:   AddressRun,
		Kind:    runtime.RecordExtensionData,
		ExtType: AddressExtType,
		Text:    address,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("bonnie: bind address: %w", err)
	}
	m.byAddr[address] = runID
	return nil
}

// NotePrincipal records the identity that sent a message.
func (m *AddressMap) NotePrincipal(ctx context.Context, runID string, p *channel.Principal) error {
	payload, err := json.Marshal(principalNote{RunID: runID, Principal: p})
	if err != nil {
		return fmt.Errorf("bonnie: encode principal: %w", err)
	}
	if _, err := m.journal.Append(ctx, runtime.Record{
		RunID:   AddressRun,
		Kind:    runtime.RecordExtensionData,
		ExtType: PrincipalExtType,
		Text:    runID,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("bonnie: record principal: %w", err)
	}
	return nil
}

// Locks serialises turns per run. A queued message waits here instead of
// racing an active turn.
type Locks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewLocks returns an empty set of per-run locks.
func NewLocks() *Locks {
	return &Locks{locks: make(map[string]*sync.Mutex)}
}

// Lock takes the turn lock for a run and returns its release.
func (l *Locks) Lock(runID string) func() {
	l.mu.Lock()
	mu, ok := l.locks[runID]
	if !ok {
		mu = &sync.Mutex{}
		l.locks[runID] = mu
	}
	l.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// Ref is a [channel.SessionRef] over one address or one run ID. Every method
// resolves the address first, so a reference stays correct after the address
// is re-keyed. A reference from [Core.From] returns [runtime.ErrRunNotFound]
// for an unknown run instead of creating one.
type Ref struct {
	core    *Core
	address string
	runID   string
	create  bool
}

// Core is the shared construction of a chat or HTTP adapter: the runner, the
// journalled address map, the per-run locks, the run-ID generator, and the
// default turn policy. Adapters embed it and hand it out through [Core.From]
// and [Core.Attach], which build [channel.SessionRef] implementations.
type Core struct {
	runner    *runtime.Runner
	addresses *AddressMap
	locks     *Locks
	newID     func() string
	policy    channel.TurnPolicy
}

// CoreOption configures a [Core].
type CoreOption func(*Core)

// WithIDGenerator replaces the run-ID generator. Tests use it to get stable
// IDs; production rarely needs it.
func WithIDGenerator(fn func() string) CoreOption {
	return func(c *Core) { c.newID = fn }
}

// NewCore returns the shared plumbing over a runner. The policy is the
// turn policy used when a message arrives while a turn is already running;
// the empty value means [channel.PolicySteer].
func NewCore(r *runtime.Runner, policy channel.TurnPolicy, opts ...CoreOption) *Core {
	c := &Core{
		runner:    r,
		addresses: NewAddressMap(r.Journal()),
		locks:     NewLocks(),
		newID:     NewRunID,
		policy:    policy,
	}
	if c.policy == "" {
		c.policy = channel.PolicySteer
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Addresses exposes the address map, for re-keying and for the platform
// filters adapters need (a thread this channel never started is not for the
// agent).
func (c *Core) Addresses() *AddressMap { return c.addresses }

// Runner exposes the runner the adapter serves.
func (c *Core) Runner() *runtime.Runner { return c.runner }

// NewID generates a fresh run ID through the configured generator.
func (c *Core) NewID() string { return c.newID() }

// From returns a [channel.SessionRef] that resolves a channel-local
// address, creating and binding the run on first use.
func (c *Core) From(address string) *Ref {
	return &Ref{core: c, address: address, create: true}
}

// Attach returns a [channel.SessionRef] that targets exactly one run ID and
// never creates: an unknown ID is [runtime.ErrRunNotFound].
func (c *Core) Attach(runID string) *Ref {
	return &Ref{core: c, runID: runID}
}

var _ channel.SessionRef = (*Ref)(nil)

// RunID implements [channel.SessionRef].
//
// A reserved run is not addressable from a transport. BONNIE keeps its own
// bookkeeping — the address map — in a run under
// [runtime.ReservedRunPrefix], and that run has a state, so Attach used to
// accept it and a caller who knew the prefix could run a model turn inside
// the store every address binding lives in. Reserved runs stay BONNIE's
// (docs/SPEC.md §8, invariant 8), so the answer is the same one an unknown
// ID gets.
func (s *Ref) RunID(ctx context.Context) (string, error) {
	if !s.create {
		if runtime.IsReservedRun(s.runID) {
			return "", fmt.Errorf("%w: %s is reserved for BONNIE's own bookkeeping", runtime.ErrRunNotFound, s.runID)
		}
		// Attach: the run must already exist.
		if _, err := s.core.runner.Journal().State(ctx, s.runID); err != nil {
			return "", err
		}
		return s.runID, nil
	}
	return s.core.addresses.Resolve(ctx, s.address, s.core.newID)
}

// Send implements [channel.SessionRef]. A message that lands mid-turn is
// steered into the turn that is already running, which keeps the work the
// turn has done. Queue waits instead. An unknown policy is refused, never
// guessed: the same misspelling must not mean "wait" on one transport and
// "interrupt" on another.
func (s *Ref) Send(ctx context.Context, text string, opts channel.SendOptions) (*runtime.Run, error) {
	runID, err := s.RunID(ctx)
	if err != nil {
		return nil, err
	}
	if opts.Auth != nil {
		if err := s.core.addresses.NotePrincipal(ctx, runID, opts.Auth); err != nil {
			return nil, err
		}
	}

	policy := opts.TurnPolicy
	if policy == "" {
		policy = s.core.policy
	}
	switch policy {
	case channel.PolicySteer, channel.PolicyQueue:
	default:
		return nil, fmt.Errorf("%w: %q", channel.ErrUnknownTurnPolicy, policy)
	}

	// A message that lands mid-turn is steered into the turn that is already
	// running, which keeps the work the turn has done. Queue waits instead.
	if policy == channel.PolicySteer && s.core.runner.IsActive(runID) {
		if err := s.core.runner.Steer(runID, text); err == nil {
			return s.core.runner.Snapshot(ctx, runID)
		}
		// The turn ended between the check and the steer: fall through and
		// run it as a new turn.
	}

	unlock := s.core.locks.Lock(runID)
	defer unlock()
	return s.core.runner.Start(ctx, runID, runtime.Input{Text: text})
}

// Respond implements [channel.SessionRef].
func (s *Ref) Respond(ctx context.Context, responses []runtime.InputResponse) (*runtime.Run, error) {
	runID, err := s.RunID(ctx)
	if err != nil {
		return nil, err
	}
	unlock := s.core.locks.Lock(runID)
	defer unlock()
	return s.core.runner.Resume(ctx, runID, responses)
}

// Cancel implements [channel.SessionRef].
func (s *Ref) Cancel(ctx context.Context) error {
	runID, err := s.RunID(ctx)
	if err != nil {
		return err
	}
	return s.core.runner.Cancel(runID)
}

// Route delivers one inbound platform message to the right runner entry
// point. A chat surface gives the adapter no way to say "this is a resume":
// the same thread carries starts, follow-ups, and answers to parked runs.
// The run's state decides — a waiting run is answered with Respond, anything
// else starts a turn. The state check and the entry run under the turn
// lock, so two platform messages cannot race the decision.
//
// An unknown run (the address was never bound) is a Send, which creates.
func Route(ctx context.Context, ref *Ref, text string, opts channel.SendOptions) (*runtime.Run, error) {
	runID, err := ref.RunID(ctx)
	if err != nil {
		return nil, err
	}
	state, err := ref.core.runner.Journal().State(ctx, runID)
	if err != nil && !errors.Is(err, runtime.ErrRunNotFound) {
		return nil, err
	}
	if state == runtime.RunWaiting {
		return ref.Respond(ctx, []runtime.InputResponse{{Text: text}})
	}
	return ref.Send(ctx, text, opts)
}

// Dispatch delivers one inbound platform message and reports the outcome.
//
// A chat platform's webhook wants an acknowledgement within seconds, but a
// turn can run for minutes — so Dispatch runs [Route] in a goroutine that
// survives the handler (its context is [context.WithoutCancel]) and calls
// deliver at the next boundary:
//
//   - a completed, failed, or cancelled turn delivers that turn's result;
//   - a run that parks for human input delivers the suspension, and the
//     next message on the same address resumes it;
//   - a message that was steered into an already-running turn delivers
//     nothing — the turn's own message delivers when the turn ends, and
//     posting both would answer one question twice.
//
// A start failure (no model configured, the journal refused a write)
// delivers as an error, so the person typing learns the run died instead of
// waiting on a reply that never comes.
func Dispatch(ctx context.Context, core *Core, address, text string, opts channel.SendOptions, deliver func(address string, run *runtime.Run, err error)) {
	go func() {
		bg := context.WithoutCancel(ctx)
		run, err := Route(bg, core.From(address), text, opts)
		if err != nil {
			deliver(address, nil, err)
			return
		}
		if run.State == runtime.RunRunning || run.State == runtime.RunPending {
			// Steered into a turn that is still running: the message that
			// owns the turn delivers its result.
			return
		}
		deliver(address, run, nil)
	}()
}

// SplitText splits a reply into platform-sized chunks. It breaks on line
// boundaries when one exists, falls back to a hard cut when a single line
// exceeds the limit, and caps the number of parts so a runaway response
// cannot flood a channel — the tail becomes one truncation notice.
func SplitText(s string, limit int, maxParts int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if limit <= 0 || len(s) <= limit {
		return []string{s}
	}

	var parts []string
	for len(s) > limit && len(parts) < maxParts-1 {
		cut := s[:limit]
		if i := strings.LastIndexByte(cut, '\n'); i > limit/2 {
			cut = cut[:i]
		}
		parts = append(parts, cut)
		s = s[len(cut):]
	}
	if len(s) > limit {
		// Too much even for the final part: cut hard and say so.
		s = s[:limit]
	}
	if len(parts) < maxParts {
		parts = append(parts, s)
		return parts
	}
	// One part left for the tail, and the tail does not fit: truncate.
	return append(parts, s[:limit-len(truncationNote)]+truncationNote)
}

const truncationNote = "\n\n… message truncated"

// DeliveryText renders the boundary of a turn for a person, not for a model:
// the answer, the question the run parked on, a cancellation, or the reason
// it failed. answerHint tells the reader how to answer a parked run, and is
// the only part that differs between transports — a Slack thread, a Telegram
// chat, and a Discord slash command each take an answer their own way.
//
// It lives here because all three adapters had a byte-identical copy. The
// rule for what a person sees at the end of a turn is one rule, and a change
// to it must not reach two transports out of three.
func DeliveryText(run *runtime.Run, err error, answerHint string) string {
	switch {
	case err != nil:
		return "the run failed: " + FirstLine(err.Error())
	case run == nil:
		return ""
	case run.State == runtime.RunWaiting && run.Suspend != nil:
		if answerHint == "" {
			return run.Suspend.Prompt
		}
		return run.Suspend.Prompt + "\n\n" + answerHint
	case run.State == runtime.RunCancelled:
		return "(cancelled)"
	case run.Response != "":
		return run.Response
	default:
		return ""
	}
}

// errorLineLimit caps the error text a chat reply carries. A stack of
// wrapped context helps an operator reading the journal; it only buries the
// answer in a chat window.
const errorLineLimit = 200

// FirstLine is the first line of an error, capped, for a chat reply.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > errorLineLimit {
		s = s[:errorLineLimit]
	}
	return s
}

// NewRunID returns a run ID that is safe as a file name, which is what the
// file journal needs.
func NewRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever
		// does, a duplicate run ID would silently merge two conversations.
		panic(fmt.Sprintf("bonnie: read random: %v", err))
	}
	return "run-" + hex.EncodeToString(b[:])
}
