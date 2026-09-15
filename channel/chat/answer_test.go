package chat_test

import (
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

// parked builds a run waiting on a suspension, which is the only shape
// Choices answers for.
func parked(sus runtime.SuspendRequest) *runtime.Run {
	return &runtime.Run{ID: "r1", State: runtime.RunWaiting, Suspend: &sus}
}

// An approval gets exactly two controls, and pressing one produces a
// VERDICT rather than the word on the button.
//
// runtime.ApprovalTool sets Kind approval AND Options ["approve",
// "reject"], so reading the options here would turn a press into the text
// "approve" and lose the verdict field the runtime has for it. Kind decides.
func TestApprovalOffersAVerdictNotWords(t *testing.T) {
	t.Parallel()
	sus := runtime.SuspendRequest{
		Kind:       runtime.SuspendApproval,
		Prompt:     "Delete the production database?",
		Options:    []string{"approve", "reject"},
		ToolCallID: "call_1",
	}
	choices := chat.Choices(parked(sus))
	if len(choices) != 2 {
		t.Fatalf("got %d controls, want 2: an approval is a binary", len(choices))
	}
	if choices[0].Label != "Approve" || choices[1].Label != "Reject" {
		t.Fatalf("labels = %q, %q, want Approve and Reject", choices[0].Label, choices[1].Label)
	}

	yes, ok := chat.Answer(&sus, choices[0].Token)
	if !ok || len(yes) != 1 {
		t.Fatalf("Answer(approve) = %v, %v", yes, ok)
	}
	if yes[0].Approved == nil || !*yes[0].Approved {
		t.Fatalf("approve produced %+v, want an explicit approval", yes[0])
	}
	if yes[0].Text != "" {
		t.Fatalf("approve carried text %q: the verdict is the answer", yes[0].Text)
	}

	no, ok := chat.Answer(&sus, choices[1].Token)
	if !ok || no[0].Approved == nil || *no[0].Approved {
		t.Fatalf("reject produced %+v, want an explicit rejection", no)
	}
}

// A question with options gets one control per option, answering with that
// option's own text.
func TestQuestionOffersItsOptions(t *testing.T) {
	t.Parallel()
	sus := runtime.SuspendRequest{
		Kind:       runtime.SuspendQuestion,
		Prompt:     "Which region?",
		Options:    []string{"eu-west-1", "us-east-1", "ap-south-1"},
		ToolCallID: "call_2",
	}
	choices := chat.Choices(parked(sus))
	if len(choices) != 3 {
		t.Fatalf("got %d controls, want one per option", len(choices))
	}
	for i, want := range sus.Options {
		if choices[i].Label != want {
			t.Fatalf("control %d is labelled %q, want %q", i, choices[i].Label, want)
		}
		got, ok := chat.Answer(&sus, choices[i].Token)
		if !ok || len(got) != 1 || got[0].Text != want {
			t.Fatalf("Answer(%q) = %v, %v, want the option's text", choices[i].Token, got, ok)
		}
		if got[0].Approved != nil {
			t.Fatalf("a question's answer carried a verdict: %+v", got[0])
		}
	}
}

// A question with no options has nothing to draw: the person types.
func TestFreeTextQuestionOffersNothing(t *testing.T) {
	t.Parallel()
	sus := runtime.SuspendRequest{
		Kind:       runtime.SuspendQuestion,
		Prompt:     "What should I name it?",
		ToolCallID: "call_3",
	}
	if got := chat.Choices(parked(sus)); got != nil {
		t.Fatalf("Choices = %v, want none for a free-text question", got)
	}
}

// Only a parked run offers controls. A completed run must not leave buttons
// on screen suggesting it is still asking.
func TestOnlyAParkedRunOffersControls(t *testing.T) {
	t.Parallel()
	sus := runtime.SuspendRequest{Kind: runtime.SuspendApproval, ToolCallID: "call_4"}
	for _, run := range []*runtime.Run{
		nil,
		{ID: "r", State: runtime.RunCompleted, Suspend: &sus},
		{ID: "r", State: runtime.RunWaiting},
		{ID: "r", State: runtime.RunCancelled},
	} {
		if got := chat.Choices(run); got != nil {
			t.Fatalf("Choices(%+v) = %v, want none", run, got)
		}
	}
}

// The staleness guard, and the reason the tool-call ID is in the token.
//
// The controls of an answered question stay on screen in a chat history
// forever. An index alone would resolve against whatever the run is parked
// on NOW, so someone scrolling back and pressing Approve on last week's
// question would approve this week's.
func TestAStaleControlIsRefused(t *testing.T) {
	t.Parallel()
	old := runtime.SuspendRequest{
		Kind: runtime.SuspendApproval, Prompt: "Deploy the docs?", ToolCallID: "call_old",
	}
	current := runtime.SuspendRequest{
		Kind: runtime.SuspendApproval, Prompt: "Drop the users table?", ToolCallID: "call_new",
	}

	stale := chat.Choices(parked(old))[0].Token
	if _, ok := chat.Answer(&current, stale); ok {
		t.Fatal("a control from an earlier question answered the current one")
	}
	// The same press against the question it was drawn for still works.
	if _, ok := chat.Answer(&old, stale); !ok {
		t.Fatal("a control did not answer its own question")
	}
}

// A token that is malformed, from nowhere, or names an option that no longer
// exists is refused rather than guessed at.
func TestMalformedTokensAreRefused(t *testing.T) {
	t.Parallel()
	sus := runtime.SuspendRequest{
		Kind:       runtime.SuspendQuestion,
		Options:    []string{"one", "two"},
		ToolCallID: "call_5",
	}
	for _, tok := range []string{
		"",
		"call_5",     // no separator
		"call_5|",    // no selector
		"call_5|o",   // no index
		"call_5|o9",  // out of range
		"call_5|o-1", // negative
		"call_5|ox",  // not a number
		"call_5|a",   // approval selector on a question
		"call_5|z",   // unknown selector
		"other|o0",   // another suspension
		"|o0",        // no tool call
	} {
		if _, ok := chat.Answer(&sus, tok); ok {
			t.Fatalf("Answer(%q) was accepted", tok)
		}
	}
	if _, ok := chat.Answer(nil, "call_5|o0"); ok {
		t.Fatal("Answer against no suspension was accepted")
	}
}

// Telegram rejects the whole message when a callback_data key exceeds 64
// bytes, so a token that would not fit means no controls at all. Half the
// options on screen is worse than none: the person cannot tell the rest
// exist.
func TestOversizedTokensDropEveryControl(t *testing.T) {
	t.Parallel()
	sus := runtime.SuspendRequest{
		Kind:       runtime.SuspendQuestion,
		Options:    []string{"one", "two"},
		ToolCallID: strings.Repeat("x", 64),
	}
	if got := chat.Choices(parked(sus)); got != nil {
		t.Fatalf("Choices = %v, want none when a token would not fit", got)
	}

	// An ordinary provider tool-call ID fits with room to spare.
	ok := runtime.SuspendRequest{
		Kind:       runtime.SuspendApproval,
		ToolCallID: "toolu_01A09q90qw90lq917835lq9",
	}
	for _, c := range chat.Choices(parked(ok)) {
		if len(c.Token) > 64 {
			t.Fatalf("token %q is %d bytes, over Telegram's limit", c.Token, len(c.Token))
		}
	}
}

// A label longer than a platform will draw is cut, and cut on a rune
// boundary — half a rune is not a character.
func TestLongLabelsAreCutOnARuneBoundary(t *testing.T) {
	t.Parallel()
	sus := runtime.SuspendRequest{
		Kind:       runtime.SuspendQuestion,
		Options:    []string{strings.Repeat("é", 60)},
		ToolCallID: "call_6",
	}
	got := chat.Choices(parked(sus))
	if len(got) != 1 {
		t.Fatalf("got %d controls, want 1", len(got))
	}
	if len(got[0].Label) > 80 {
		t.Fatalf("label is %d bytes, over Discord's 80", len(got[0].Label))
	}
	if !utf8Valid(got[0].Label) {
		t.Fatalf("label %q ends mid-rune", got[0].Label)
	}
	// The token still resolves: the label was cut, the answer was not.
	answer, ok := chat.Answer(&sus, got[0].Token)
	if !ok || answer[0].Text != sus.Options[0] {
		t.Fatalf("the cut label changed the answer: %v", answer)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
