package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// RecordDecision records one call to a decision model. It is run metadata,
// not conversation: replay and Restore skip it. Its payload is a
// [DecisionRecord].
const RecordDecision RecordKind = "decision"

// Decision sources.
const (
	// DecisionSourceHost is a call from host code: a tool factory, a Kit
	// setup callback or a completion hook.
	DecisionSourceHost = "host"
	// DecisionSourceTool is a call from the model-facing decide tool.
	DecisionSourceTool = "tool"
)

var (
	// ErrNoDecider means that no decision model is configured.
	ErrNoDecider = errors.New("bonnie: no decision model configured")
	// ErrDecisionKeyReused means that a decision key was used again with a
	// different model, state or questions. A stored answer is never given
	// for different input.
	ErrDecisionKeyReused = errors.New("bonnie: decision key reused with different input")
)

// Decider sends one request to a decision model. [kit.Decide] and
// [kit.Kit.Decide] satisfy it. A test or a host can supply its own.
type Decider func(context.Context, kit.DecisionRequest) (*kit.DecisionResult, error)

// DecisionCall is one decision for [Session.Decide].
type DecisionCall struct {
	Request kit.DecisionRequest
	// Key makes the call idempotent within the run. When the run already
	// has a successful decision with the same key and the same input,
	// Decide returns the stored result and does not call the model. Empty
	// means that each call asks the model.
	Key string
	// Source is DecisionSourceHost or DecisionSourceTool. Empty is
	// DecisionSourceHost.
	Source string
	// ToolCallID is the tool call the decision belongs to. Empty takes it
	// from ctx ([kit.ToolCallIDFromContext]) when the call runs in a tool.
	ToolCallID string
}

// DecisionRecord is the payload of a [RecordDecision].
//
// It does not keep the state the questions were about: the state can be
// large, contain images, or be sensitive. InputSHA256 identifies it, with the
// model, the questions and the provider options, so a key used again for
// different input is detected. A failed call keeps the class of its error only, never the
// provider's response body.
type DecisionRecord struct {
	Key         string                 `json:"key,omitempty"`
	Source      string                 `json:"source"`
	ToolCallID  string                 `json:"tool_call_id,omitempty"`
	Model       string                 `json:"model"`
	InputSHA256 string                 `json:"input_sha256"`
	Questions   []kit.DecisionQuestion `json:"questions"`
	Answers     []kit.DecisionAnswer   `json:"answers,omitempty"`
	// AnsweredBy is the model the provider reports, for example a resolved
	// version.
	AnsweredBy string            `json:"answered_by,omitempty"`
	Usage      kit.DecisionUsage `json:"usage"`
	// Error is the class of a failed call, for example "refused" or
	// "http_429". Empty means success.
	Error string `json:"error,omitempty"`
}

// Result returns the stored answers as a [kit.DecisionResult]. Provider
// metadata is not stored, so the result has none.
func (d DecisionRecord) Result() *kit.DecisionResult {
	return &kit.DecisionResult{Answers: d.Answers, Model: d.AnsweredBy, Usage: d.Usage}
}

// DecodeDecisionRecord decodes the payload of a [RecordDecision].
func DecodeDecisionRecord(rec Record) (DecisionRecord, error) {
	var d DecisionRecord
	if rec.Kind != RecordDecision {
		return d, fmt.Errorf("bonnie: record %d is %s, not %s", rec.Seq, rec.Kind, RecordDecision)
	}
	if err := json.Unmarshal(rec.Payload, &d); err != nil {
		return d, fmt.Errorf("bonnie: decode decision record: %w", err)
	}
	return d, nil
}

// decisionIndex holds the successful keyed decisions of one session's run.
type decisionIndex struct {
	mu     sync.Mutex
	loaded bool
	done   map[string]DecisionRecord
	locks  map[string]*sync.Mutex
	// observer is the current turn's [DecisionObservation], or nil.
	observer DecisionObservation
}

// observeDecisions sets the observation that sees the session's decision
// calls. Nil stops it. The runner sets it for the duration of one turn.
func (s *Session) observeDecisions(o DecisionObservation) {
	s.decisions.mu.Lock()
	s.decisions.observer = o
	s.decisions.mu.Unlock()
}

// Decide asks a decision model and journals a [RecordDecision] for each call
// that reaches d, also for a call that fails. A call that does not reach d
// writes no record: a key answered from the journal, a key used again with
// different input, a nil d, or input that cannot be encoded.
//
// A call with a Key is idempotent within the run: a stored successful answer
// for the same key and input is returned without a model call, also by a
// session in another process. Calls with the same key are serialized. A key
// used again with different input returns [ErrDecisionKeyReused]. A failed
// call is not reused; the next call with its key asks again.
//
// When the record cannot be written, Decide returns an error and no result:
// an answer that is not durable must not drive an action. If the process
// stops after the model answers but before the record is written, a later
// call asks again. This is not exactly-once execution, and a retry can cost
// money.
func (s *Session) Decide(ctx context.Context, d Decider, call DecisionCall) (*kit.DecisionResult, error) {
	if d == nil {
		return nil, ErrNoDecider
	}
	if call.Source == "" {
		call.Source = DecisionSourceHost
	}
	if call.ToolCallID == "" {
		call.ToolCallID = kit.ToolCallIDFromContext(ctx)
	}
	hash, err := decisionInputHash(call.Request)
	if err != nil {
		return nil, err
	}
	if call.Key == "" {
		rec, err := s.decide(ctx, d, call, hash)
		if err != nil {
			return nil, err
		}
		return rec.Result(), nil
	}

	lock := s.decisionLock(call.Key)
	lock.Lock()
	defer lock.Unlock()
	prev, ok, err := s.storedDecision(ctx, call.Key)
	if err != nil {
		return nil, err
	}
	if ok {
		if prev.InputSHA256 != hash {
			return nil, fmt.Errorf("%w: %q", ErrDecisionKeyReused, call.Key)
		}
		return prev.Result(), nil
	}
	rec, err := s.decide(ctx, d, call, hash)
	if err != nil {
		return nil, err
	}
	s.decisions.mu.Lock()
	s.decisions.done[call.Key] = rec
	s.decisions.mu.Unlock()
	return rec.Result(), nil
}

// decide calls d and journals the outcome.
func (s *Session) decide(ctx context.Context, d Decider, call DecisionCall, hash string) (DecisionRecord, error) {
	rec := DecisionRecord{
		Key:         call.Key,
		Source:      call.Source,
		ToolCallID:  call.ToolCallID,
		Model:       call.Request.Model,
		InputSHA256: hash,
		Questions:   call.Request.Questions,
	}
	start := now()
	res, callErr := d(ctx, call.Request)
	defer func() {
		s.decisions.mu.Lock()
		o := s.decisions.observer
		s.decisions.mu.Unlock()
		if o != nil {
			o.Decision(ctx, rec, start, now())
		}
	}()
	if callErr == nil && res == nil {
		callErr = fmt.Errorf("%w: no result", kit.ErrInvalidDecisionResponse)
	}
	if callErr != nil {
		rec.Error = decisionErrorClass(callErr)
	} else {
		rec.Answers, rec.AnsweredBy, rec.Usage = res.Answers, res.Model, res.Usage
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return rec, errors.Join(callErr, fmt.Errorf("bonnie: encode decision record: %w", err))
	}
	// The call is spent once the provider has it. Record it also when the
	// caller has given up, so the journal shows what it cost.
	_, err = s.journal.Append(context.WithoutCancel(ctx), Record{RunID: s.runID, Kind: RecordDecision, Timestamp: now(), Payload: payload})
	if err != nil {
		err = fmt.Errorf("bonnie: journal decision: %w", err)
	}
	if err := errors.Join(callErr, err); err != nil {
		return rec, err
	}
	return rec, nil
}

// decisionLock returns the lock that serializes calls with one key.
func (s *Session) decisionLock(key string) *sync.Mutex {
	s.decisions.mu.Lock()
	defer s.decisions.mu.Unlock()
	if s.decisions.locks == nil {
		s.decisions.locks = make(map[string]*sync.Mutex)
	}
	lock, ok := s.decisions.locks[key]
	if !ok {
		lock = new(sync.Mutex)
		s.decisions.locks[key] = lock
	}
	return lock
}

// storedDecision returns the successful decision journalled with key. The
// first call reads the run's journal; later ones read the index this session
// keeps current. One process owns a run's journal, so no other writer can
// add a decision this index does not see.
func (s *Session) storedDecision(ctx context.Context, key string) (DecisionRecord, bool, error) {
	s.decisions.mu.Lock()
	defer s.decisions.mu.Unlock()
	if !s.decisions.loaded {
		recs, err := s.journal.Replay(ctx, s.runID)
		if err != nil && !errors.Is(err, ErrRunNotFound) {
			return DecisionRecord{}, false, err
		}
		done := make(map[string]DecisionRecord)
		for _, rec := range recs {
			if rec.Kind != RecordDecision {
				continue
			}
			d, err := DecodeDecisionRecord(rec)
			if err != nil {
				return DecisionRecord{}, false, err
			}
			if d.Key != "" && d.Error == "" {
				done[d.Key] = d
			}
		}
		s.decisions.done, s.decisions.loaded = done, true
	}
	d, ok := s.decisions.done[key]
	return d, ok, nil
}

// decisionInputHash identifies the model, state, questions and provider
// options of a request.
// encoding/json sorts map keys, so equal input gives an equal hash.
func decisionInputHash(req kit.DecisionRequest) (string, error) {
	data, err := json.Marshal(struct {
		Model           string                 `json:"model"`
		State           kit.DecisionState      `json:"state"`
		Questions       []kit.DecisionQuestion `json:"questions"`
		ProviderOptions map[string]any         `json:"provider_options,omitempty"`
	}{req.Model, req.State, req.Questions, req.ProviderOptions})
	if err != nil {
		return "", fmt.Errorf("%w: encode state: %v", kit.ErrInvalidDecisionRequest, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// decisionErrorClass names the kind of a failed call without its message,
// which can hold provider data.
func decisionErrorClass(err error) string {
	if apiErr, ok := errors.AsType[*kit.DecisionAPIError](err); ok {
		return fmt.Sprintf("http_%d", apiErr.StatusCode)
	}
	for _, c := range []struct {
		target error
		class  string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{kit.ErrInvalidDecisionRequest, "invalid_request"},
		{kit.ErrInvalidDecisionResponse, "invalid_response"},
		{kit.ErrDecisionRefused, "refused"},
		{kit.ErrNotDecisionModel, "not_decision_model"},
		{kit.ErrUnknownDecisionWire, "unknown_wire"},
	} {
		if errors.Is(err, c.target) {
			return c.class
		}
	}
	return "error"
}
