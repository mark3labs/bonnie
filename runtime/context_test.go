package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// contextAgent is a fakeAgent that records what the session's turn context
// was at the moment the model was prompted — which is when the
// context-prepare hook reads it on a real Kit.
type contextAgent struct {
	fakeAgent
	seen [][]string
}

func (c *contextAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	c.seen = append(c.seen, c.session.TurnContext())
	return c.fakeAgent.PromptResult(ctx, msg)
}

// The context of one input reaches the model on that turn and on no later
// turn. The session is rebuilt from the journal at every Start, so a second
// turn with no context sees none — and the journal shows the context that
// was sent, as its own record, never as a user message.
func TestContextReachesOnlyItsOwnTurn(t *testing.T) {
	t.Parallel()
	agent := &contextAgent{}
	factory := func(_ context.Context, s *Session) (Agent, error) {
		agent.session = s
		return agent, nil
	}
	j := NewMemoryJournal()
	r := NewRunner(j, factory)
	ctx := context.Background()

	if _, err := r.Start(ctx, "run-ctx", Input{
		Text:    "what changed?",
		Context: []string{"event: pull_request.opened", "diff: +1 -1 main.go"},
		Title:   "PR #42",
		Origin:  Origin{Channel: "github", Kind: "pull_request"},
	}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := r.Start(ctx, "run-ctx", Input{Text: "and now?", Title: "ignored", Origin: Origin{Channel: "slack"}}); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if len(agent.seen) != 2 {
		t.Fatalf("agent ran %d turns, want 2", len(agent.seen))
	}
	if got := strings.Join(agent.seen[0], "|"); got != "event: pull_request.opened|diff: +1 -1 main.go" {
		t.Fatalf("turn 1 context = %q", got)
	}
	if len(agent.seen[1]) != 0 {
		t.Fatalf("turn 2 context = %v, want none: context is per turn", agent.seen[1])
	}

	// The journal holds the context as its own record, and the user
	// messages carry the text only.
	recs, err := j.Replay(ctx, "run-ctx")
	if err != nil {
		t.Fatal(err)
	}
	var contexts, users []string
	for _, rec := range recs {
		switch {
		case rec.Kind == RecordContext:
			var p contextPayload
			if err := json.Unmarshal(rec.Payload, &p); err != nil {
				t.Fatalf("decode context record: %v", err)
			}
			contexts = append(contexts, strings.Join(p.Context, "|"))
		case rec.Kind == RecordMessage && rec.Role == "user":
			users = append(users, rec.Text)
		}
	}
	if len(contexts) != 1 || contexts[0] != "event: pull_request.opened|diff: +1 -1 main.go" {
		t.Fatalf("context records = %v, want exactly the first turn's", contexts)
	}
	if strings.Join(users, "|") != "what changed?|and now?" {
		t.Fatalf("user messages = %v: context leaked into the conversation", users)
	}

	// Title and origin are recorded once: the second turn's values are
	// ignored, and a restored session reads the first turn's.
	s, err := Restore(ctx, "run-ctx", j)
	if err != nil {
		t.Fatal(err)
	}
	if s.Title() != "PR #42" {
		t.Fatalf("title = %q, want the first turn's", s.Title())
	}
	if o := s.Origin(); o != (Origin{Channel: "github", Kind: "pull_request"}) {
		t.Fatalf("origin = %+v, want the first turn's", o)
	}
	// And the restored session carries no turn context of its own.
	if len(s.TurnContext()) != 0 {
		t.Fatalf("restored session has turn context %v", s.TurnContext())
	}
}

// The placement rule: context goes in front of the last user message, as
// user-role messages with a marker, and the origin comes first. Nothing is
// injected when there is nothing to say.
func TestPrepareContextPlacesContextBeforeThePrompt(t *testing.T) {
	t.Parallel()
	user := func(s string) kit.LLMMessage { return kit.NewLLMUserMessage(s) }
	assistant := func(s string) kit.LLMMessage {
		return kit.LLMMessage{Role: kit.LLMMessageRole("assistant"), Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: s}}}
	}
	window := []kit.LLMMessage{user("first"), assistant("ok"), user("second")}

	if got := prepareContext(window, Origin{}, nil); got != nil {
		t.Fatalf("nothing to inject must return nil, got %d messages", len(got))
	}

	got := prepareContext(window, Origin{Channel: "github", Kind: "issue"}, []string{"sender: alice", ""})
	var texts []string
	for _, m := range got {
		texts = append(texts, string(m.Role)+":"+messageText(m))
	}
	want := []string{
		"user:first", "assistant:ok",
		"user:[context] This conversation is on channel github (issue).",
		"user:[context] sender: alice",
		"user:second",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("window =\n%s\nwant\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}

	// A window with no user message appends at the end rather than panics.
	got = prepareContext([]kit.LLMMessage{assistant("hm")}, Origin{}, []string{"x"})
	if len(got) != 2 || messageText(got[1]) != "[context] x" {
		t.Fatalf("no-user window = %v", got)
	}
}
