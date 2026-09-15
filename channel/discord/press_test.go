package discord

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// press is a MESSAGE_COMPONENT interaction: what Discord posts when a person
// presses a button. It arrives on the same route as a slash command, which
// is why buttons cost this adapter no new endpoint.
func press(customID, messageContent string) string {
	b, _ := json.Marshal(map[string]any{
		"type":       3,
		"channel_id": "C77",
		"data":       map[string]any{"custom_id": customID},
		"member":     map[string]any{"user": map[string]any{"id": "U7", "username": "ada"}},
		"message":    map[string]any{"content": messageContent},
	})
	return string(b)
}

// componentsOf pulls the custom_ids out of a posted message, in order.
func componentsOf(t *testing.T, body string) []string {
	t.Helper()
	var payload struct {
		Components []struct {
			Components []struct {
				Label    string `json:"label"`
				CustomID string `json:"custom_id"`
				Style    int    `json:"style"`
			} `json:"components"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("undecodable post: %v", err)
	}
	var ids []string
	for _, row := range payload.Components {
		for _, c := range row.Components {
			ids = append(ids, c.CustomID)
		}
	}
	return ids
}

// A run parked on an approval is delivered with buttons, and pressing one
// resumes the run with a VERDICT.
//
// This is the whole point of the feature: an approval answered in prose has
// to be interpreted, and "sure, go ahead" is not a decision a tool can act
// on. The test drives the real webhook, so it covers the outbound render and
// the inbound press together.
func TestApprovalIsDeliveredAsButtonsAndPressedForAVerdict(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{
		{Response: "May I deploy?", FinalValue: runtime.SuspendRequest{
			Kind:       runtime.SuspendApproval,
			Prompt:     "Deploy to production?",
			Options:    []string{"approve", "reject"},
			ToolCallID: "call_1",
		}},
		{Response: "Deployed."},
	})

	h.post(t, ask("deploy the app"))
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })

	// The question arrived with two controls and no "type your answer" hint.
	body := h.fake.bodies()[0]
	ids := componentsOf(t, body)
	if len(ids) != 2 {
		t.Fatalf("delivered %d controls, want 2: %s", len(ids), body)
	}
	if !strings.Contains(body, `"label":"Approve"`) || !strings.Contains(body, `"label":"Reject"`) {
		t.Fatalf("the controls are not an approval: %s", body)
	}
	if strings.Contains(h.fake.messages()[0], "Answer with /") {
		t.Fatalf("a typing hint was delivered beside the buttons: %q", h.fake.messages()[0])
	}

	// Press Approve.
	resp, raw := h.post(t, press(ids[0], "Deploy to production?"))
	if resp.StatusCode != 200 {
		t.Fatalf("press = %d", resp.StatusCode)
	}
	// The acknowledgement replaces the message and takes the buttons away,
	// so the question cannot be answered twice from the same message.
	if got := responseType(t, raw); got != typeUpdateMessage {
		t.Fatalf("press answered with type %d, want %d (update the message)", got, typeUpdateMessage)
	}
	if !strings.Contains(raw, `"components":[]`) {
		t.Fatalf("the acknowledgement left the buttons in place: %s", raw)
	}
	if !strings.Contains(raw, "ada") {
		t.Fatalf("the acknowledgement does not say who answered: %s", raw)
	}

	waitFor(t, func() bool { return len(h.fake.messages()) > 1 })

	// The verdict, not the word on the button, reached the model.
	runID, bound, _ := h.ch.core.Lookup(t.Context(), "C77")
	if !bound {
		t.Fatal("the channel lost its run")
	}
	recs, err := h.ch.core.Runner().Journal().Replay(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var resume string
	for _, rec := range recs {
		if rec.Kind == runtime.RecordResume {
			resume = rec.Text
		}
	}
	if resume != "approved" {
		t.Fatalf("the run resumed with %q, want %q", resume, "approved")
	}
}

// A question with options is delivered as one button per option, and a press
// answers with that option's text.
func TestOptionsAreDeliveredAsButtons(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{
		{Response: "Which region?", FinalValue: runtime.SuspendRequest{
			Kind:       runtime.SuspendQuestion,
			Prompt:     "Which region?",
			Options:    []string{"eu-west-1", "us-east-1"},
			ToolCallID: "call_2",
		}},
		{Response: "Done."},
	})

	h.post(t, ask("deploy it"))
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })

	ids := componentsOf(t, h.fake.bodies()[0])
	if len(ids) != 2 {
		t.Fatalf("delivered %d controls, want one per option", len(ids))
	}

	h.post(t, press(ids[1], "Which region?"))
	waitFor(t, func() bool { return len(h.fake.messages()) > 1 })

	runID, _, _ := h.ch.core.Lookup(t.Context(), "C77")
	recs, _ := h.ch.core.Runner().Journal().Replay(t.Context(), runID)
	var resume string
	for _, rec := range recs {
		if rec.Kind == runtime.RecordResume {
			resume = rec.Text
		}
	}
	if resume != "us-east-1" {
		t.Fatalf("the run resumed with %q, want the option that was pressed", resume)
	}
}

// A free-text question carries no buttons and keeps the hint that tells the
// person how to answer.
func TestFreeTextQuestionKeepsTheTypingHint(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{
		{Response: "What name?", FinalValue: runtime.SuspendRequest{
			Kind:       runtime.SuspendQuestion,
			Prompt:     "What should I call it?",
			ToolCallID: "call_3",
		}},
	})

	h.post(t, ask("make a bucket"))
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })

	if ids := componentsOf(t, h.fake.bodies()[0]); len(ids) != 0 {
		t.Fatalf("a free-text question carried controls: %v", ids)
	}
	if !strings.Contains(h.fake.messages()[0], "Answer with /") {
		t.Fatalf("the typing hint is missing: %q", h.fake.messages()[0])
	}
}

// A control from an answered question is refused rather than applied to
// whatever the run is parked on now. The buttons of old messages stay in a
// channel's history for ever.
func TestAStaleControlIsRefused(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{
		{Response: "May I?", FinalValue: runtime.SuspendRequest{
			Kind: runtime.SuspendApproval, Prompt: "Deploy?", ToolCallID: "call_now",
		}},
	})

	h.post(t, ask("deploy"))
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })

	// A press carrying another suspension's tool call.
	_, raw := h.post(t, press("call_yesterday|a", "Deploy?"))
	if got := responseType(t, raw); got != typeMessageWithSource {
		t.Fatalf("a stale press answered with type %d, want a plain message", got)
	}
	if !strings.Contains(raw, "already been answered") {
		t.Fatalf("a stale press was not explained: %s", raw)
	}

	// The run is untouched: still waiting, and no resume was journalled.
	runID, _, _ := h.ch.core.Lookup(t.Context(), "C77")
	state, _ := h.ch.core.Runner().Journal().State(t.Context(), runID)
	if state != runtime.RunWaiting {
		t.Fatalf("the run moved to %q on a stale press", state)
	}
}

// A press in a channel that carries no conversation says so and creates
// nothing.
func TestPressWithNoConversation(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)

	_, raw := h.post(t, press("call_1|a", "Deploy?"))
	if !strings.Contains(raw, "no conversation") {
		t.Fatalf("an orphan press was not explained: %s", raw)
	}
	if _, bound, _ := h.ch.core.Lookup(t.Context(), "C77"); bound {
		t.Fatal("an orphan press bound a run")
	}
}

// A press carrying no custom_id is refused without touching the run.
func TestPressWithoutACustomID(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	body, _ := json.Marshal(map[string]any{
		"type":       3,
		"channel_id": "C77",
		"data":       map[string]any{},
		"member":     map[string]any{"user": map[string]any{"id": "U7", "username": "ada"}},
	})

	_, raw := h.post(t, string(body))
	if !strings.Contains(raw, "carried nothing") {
		t.Fatalf("an empty press was not explained: %s", raw)
	}
}

// Discord allows five buttons per row and five rows. A suspension offering
// more than that is delivered as text: twenty-five buttons is not a choice
// a person can make.
func TestTooManyOptionsFallBackToText(t *testing.T) {
	t.Parallel()
	many := make([]string, 26)
	for i := range many {
		many[i] = string(rune('a' + i))
	}
	if rows := buttonRows(choicesFor(many)); rows != nil {
		t.Fatalf("26 options produced %d rows, want a text fallback", len(rows))
	}
	// Twenty-five fit, in five rows of five.
	rows := buttonRows(choicesFor(many[:25]))
	if len(rows) != 5 {
		t.Fatalf("25 options produced %d rows, want 5", len(rows))
	}
}

// choicesFor is the choice list a question with these options produces.
func choicesFor(options []string) []chat.Choice {
	out := make([]chat.Choice, 0, len(options))
	for i, o := range options {
		out = append(out, chat.Choice{Label: o, Token: "c|o" + strconv.Itoa(i)})
	}
	return out
}
