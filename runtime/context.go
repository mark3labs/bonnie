package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Extension-data types BONNIE writes to a run for its own use. They join the
// conversation tree like any [kit.ExtensionDataEntry], so they are
// branch-aware and survive a resume, and a host can read them back through
// [Session.GetExtensionData].
const (
	// ExtOrigin records where a run's conversation lives: the channel and
	// the kind of surface. It is written once, on the first turn that
	// names one.
	ExtOrigin = "bonnie.origin"
	// ExtTitle records the run's title for operator-facing listings. It is
	// written once, on the first turn that carries one.
	ExtTitle = "bonnie.title"
)

// Origin says where a run's conversation lives. The runtime is blind to
// platforms: both fields are strings the channel layer defines, and the
// runtime only records them and tells the model.
type Origin struct {
	// Channel is the channel's name: "slack", "github", "http".
	Channel string `json:"channel,omitempty"`
	// Kind is the kind of surface on that channel: "dm", "thread",
	// "issue", "pull_request", "review_thread". The channel package owns
	// the vocabulary.
	Kind string `json:"kind,omitempty"`
}

// contextPayload is the durable form of a [RecordContext].
type contextPayload struct {
	Context []string `json:"context"`
}

// SetTurnContext hands the session the context for the turn about to run.
// It is not journalled here and not kept in the tree: the runner journals
// it as a [RecordContext] and the context-prepare hook reads it back for
// the one model call it applies to.
func (s *Session) SetTurnContext(ctx []string) {
	s.mu.Lock()
	s.turnContext = append([]string(nil), ctx...)
	s.mu.Unlock()
}

// TurnContext returns the context set for the current turn, or nil.
func (s *Session) TurnContext() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.turnContext...)
}

// Origin returns the run's recorded origin, or the zero value when no turn
// has named one.
func (s *Session) Origin() Origin {
	var o Origin
	for _, e := range s.GetExtensionData(ExtOrigin) {
		_ = json.Unmarshal([]byte(e.Data), &o)
	}
	return o
}

// Title returns the run's recorded title, or "".
func (s *Session) Title() string {
	title := ""
	for _, e := range s.GetExtensionData(ExtTitle) {
		title = e.Data
	}
	return title
}

// recordOrigin writes the origin once. A later turn that names the same or
// a different origin is ignored: where a conversation started is a fact
// about the run, not about the turn.
func (s *Session) recordOrigin(o Origin) error {
	if o == (Origin{}) || s.Origin() != (Origin{}) {
		return nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("bonnie: encode origin: %w", err)
	}
	_, err = s.AppendExtensionData(ExtOrigin, string(b))
	return err
}

// recordTitle writes the title once.
func (s *Session) recordTitle(title string) error {
	if title == "" || s.Title() != "" {
		return nil
	}
	_, err := s.AppendExtensionData(ExtTitle, title)
	return err
}

// journalContext writes the turn's context as its own record. The record
// carries no entry ID: it is what the model was told, not what anyone said.
func (s *Session) journalContext(ctx context.Context, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	payload, err := json.Marshal(contextPayload{Context: lines})
	if err != nil {
		return fmt.Errorf("bonnie: encode context: %w", err)
	}
	_, err = s.journal.Append(ctx, Record{
		RunID: s.runID, Kind: RecordContext, Timestamp: now(),
		Text: strings.Join(lines, "\n"), Payload: payload,
	})
	return err
}

// contextPrefix marks an injected context message so the model can tell it
// from what a person typed.
const contextPrefix = "[context] "

// prepareContext is what the context-prepare hook does: it puts the turn's
// context, and the run's origin, in front of the last user message, as
// user-role messages, so the model reads the facts before the question. The
// result replaces the context window for one model call and is never
// appended to the session — the journal already holds the context as a
// [RecordContext], and the tree holds the origin as extension data.
//
// It is a pure function so the placement rule can be tested without Kit.
func prepareContext(msgs []kit.LLMMessage, origin Origin, lines []string) []kit.LLMMessage {
	var injected []kit.LLMMessage
	if origin != (Origin{}) {
		where := origin.Channel
		if origin.Kind != "" {
			where += " (" + origin.Kind + ")"
		}
		injected = append(injected, kit.NewLLMUserMessage(contextPrefix+"This conversation is on channel "+where+"."))
	}
	for _, line := range lines {
		if line == "" {
			continue
		}
		injected = append(injected, kit.NewLLMUserMessage(contextPrefix+line))
	}
	if len(injected) == 0 {
		return nil
	}

	// Insert before the last user message: the prompt of this turn. When
	// there is none — a turn with no user message is not one Kit produces,
	// but the rule must not panic — append at the end.
	at := len(msgs)
	for i, m := range slices.Backward(msgs) {
		if m.Role == kit.LLMMessageRole("user") {
			at = i
			break
		}
	}
	out := make([]kit.LLMMessage, 0, len(msgs)+len(injected))
	out = append(out, msgs[:at]...)
	out = append(out, injected...)
	out = append(out, msgs[at:]...)
	return out
}
