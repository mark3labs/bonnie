//go:build integration

package eval

import (
	"os"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// liveDecisionModel returns the decision model to judge with, or "" when the
// machine has no credential. BONNIE_TEST_DECISION_MODEL overrides it.
func liveDecisionModel() string {
	if m := os.Getenv("BONNIE_TEST_DECISION_MODEL"); m != "" {
		return m
	}
	if os.Getenv("OPENROUTER_API_KEY") != "" {
		return "openrouter/perplexity/pplx-decider-flash-v1.2"
	}
	return ""
}

// liveChatModel returns the chat model of the language-model judge, or ""
// when the machine has no credential. BONNIE_TEST_MODEL overrides it.
func liveChatModel() string {
	if m := os.Getenv("BONNIE_TEST_MODEL"); m != "" {
		return m
	}
	switch {
	case os.Getenv("OPENCODE_API_KEY") != "" || os.Getenv("OPENCODE_ZEN_API_KEY") != "":
		return "opencode/kimi-k2.5"
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		return "anthropic/claude-sonnet-4-5"
	case os.Getenv("OPENAI_API_KEY") != "":
		return "openai/gpt-4.1"
	case os.Getenv("GEMINI_API_KEY") != "":
		return "google/gemini-2.5-pro"
	}
	return ""
}

// The real kit.Decide path: a live decision model grades a scripted reply.
// A clear refusal must score high on "refuses" and low on "gives_refund".
func TestLiveDecisionJudge(t *testing.T) {
	model := liveDecisionModel()
	if model == "" {
		t.Skip("no decision model credential (set OPENROUTER_API_KEY or BONNIE_TEST_DECISION_MODEL)")
	}
	e := New(t, testAgent(t, fakemodel.New(fakemodel.Say("I am sorry, but a refund is not possible after 30 days."))), WithJudge(model))
	e.Send("Can I get a refund after 40 days?").Succeeded()
	scores := e.JudgeAll([]Question{
		Bool("refuses", "Does the response refuse the refund?"),
		Bool("gives_refund", "Does the response promise a refund?"),
	})
	scores["refuses"].AtLeast(0.7).Gate()
	scores["gives_refund"].AtMost(0.3).Gate()
}

// The real kit.CreateProvider path: a live chat model gives a score and a
// reason that the eval parses.
func TestLiveLLMJudge(t *testing.T) {
	model := liveChatModel()
	if model == "" {
		t.Skip("no chat model credential (set a provider key or BONNIE_TEST_MODEL)")
	}
	e := New(t, testAgent(t, fakemodel.New(fakemodel.Say("I am sorry, but a refund is not possible after 30 days."))), WithLLMJudge(model))
	e.Send("Can I get a refund after 40 days?").Succeeded()
	s := e.JudgeLLM("The response refuses the refund and gives the 30-day limit as the reason.").AtLeast(0.7).Gate()
	if s.Reason() == "" {
		t.Error("the judge gave no reason")
	}
}
