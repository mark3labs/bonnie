//go:build eval

// The evals of refund-desk. Run them with
//
//	bonnie eval --judge openrouter/typesafe/jev-1.14
//
// The build constraint keeps them out of a plain go test, which must never
// call a live model. Each eval uses the agent's chat model, and the judges
// use the model that --judge (BONNIE_EVAL_JUDGE) and --llm-judge
// (BONNIE_EVAL_LLM_JUDGE) name.
package main

import (
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/eval"
	"github.com/mark3labs/bonnie/runtime"
)

// orderInput matches a tool input that names the order id.
func orderInput(id string) eval.Matcher {
	return eval.Matches(`(?i)"order_id"\s*:\s*"` + id + `"`)
}

// outcomes are the two classes that the outcome judge chooses between.
var outcomes = []kit.DecisionOption{
	{Name: "refunded", Description: "The response confirms a refund"},
	{Name: "declined", Description: "The response refuses the refund"},
}

// refundCases is a dataset: one subtest for each case, each with a new agent.
var refundCases = []struct {
	name    string
	input   string
	order   string
	outcome string
	// cites is a phrase that a refusal must contain: the rule it names.
	cites string
}{
	{
		name:    "inside window",
		input:   "Hi, my headphones from order A-1001 stopped working. Can I get a refund?",
		order:   "A-1001",
		outcome: "refunded",
	},
	{
		name:    "outside window",
		input:   "I want to return the keyboard from order A-1003 and get my money back.",
		order:   "A-1003",
		outcome: "declined",
		cites:   "30 days",
	},
	{
		name:    "not delivered",
		input:   "Refund order A-1004 please, I do not want it any more.",
		order:   "A-1004",
		outcome: "declined",
	},
}

// TestRefundPolicy grades the policy on each case. The run checks are
// gates: they read the journal, so they are exact and free. The judge is a
// soft score: a miss is reported, and fails only with --strict.
func TestRefundPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range refundCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := eval.New(t, newAgent())

			turn := e.Send(tc.input).
				Succeeded().
				CalledToolWith("lookup_order", orderInput(tc.order)).
				NotCalledTool("request_approval").
				MaxSteps(6)
			if tc.outcome == "refunded" {
				turn.CalledToolWith("issue_refund", orderInput(tc.order)).NoFailedTools()
				e.Check(turn.Reply(), eval.Contains("R-"+tc.order))
			} else {
				// The policy is in the prompt; the agent must apply it, not
				// learn it from a refund that issue_refund refuses.
				turn.NotCalledTool("issue_refund")
			}
			if tc.cites != "" {
				e.Check(turn.Reply(), eval.ContainsFold(tc.cites))
			}

			e.Judge(eval.Choice("outcome", "Classify the response to the customer.", tc.outcome, outcomes...)).
				AtLeast(0.8)
		})
	}
}

// TestAsksForTheOrderOnce grades a conversation of two turns: the agent
// must ask for the order ID, and then refund without asking again.
func TestAsksForTheOrderOnce(t *testing.T) {
	t.Parallel()
	e := eval.New(t, newAgent())

	e.Send("My headphones arrived broken. I want my money back.").
		Succeeded().
		NotCalledTool("issue_refund")
	e.Judge(eval.Bool("asks_for_order", "Does the response ask the customer for the order ID?")).
		AtLeast(0.8).Gate()

	e.Send("Sorry, it is A-1001.").
		Succeeded().
		CalledToolWith("issue_refund", orderInput("A-1001"))

	// Grade the whole conversation, not only the latest turn.
	scores := e.JudgeAll([]eval.Question{
		eval.Bool("asked_once", "Does the assistant ask for the order ID only once?"),
		eval.Rubric("tone", "Grade the tone of the assistant.", "rude", "neutral", "polite"),
	}, eval.On(e.Transcript()))
	scores["asked_once"].AtLeast(0.8)
	scores["tone"].AtLeast(0.5)
}

// TestLargeRefundWaitsForApproval is particular to a durable agent. A
// refund above 100 dollars must wait for an operator, the wait must survive
// a restart, and the refund must follow the approval.
func TestLargeRefundWaitsForApproval(t *testing.T) {
	t.Parallel()
	e := eval.New(t, newAgent())

	e.Send("Please refund my standing desk, order A-1002. It is too big for my room.").
		Suspended().
		CalledTool("request_approval").
		NotCalledTool("issue_refund")

	// A new runtime that shares only the journal, as after a crash.
	e.Restart()

	turn := e.Respond(runtime.Approve("approved by the eval")).
		Succeeded().
		CalledToolWith("issue_refund", orderInput("A-1002")).
		NoFailedTools()
	e.Check(turn.Reply(), eval.Contains("R-A-1002"))
}

// TestRejectedRefundIsNotMade answers the approval with a rejection. The
// language-model judge grades the reply and gives a reason when it misses.
func TestRejectedRefundIsNotMade(t *testing.T) {
	t.Parallel()
	e := eval.New(t, newAgent())

	e.Send("Refund order A-1002, the standing desk.").
		Suspended().
		CalledTool("request_approval")

	e.Respond(runtime.Reject("the desk was damaged by the customer")).
		Succeeded().
		NotCalledTool("issue_refund")

	e.JudgeLLM("The response tells the customer that the refund was not approved, and promises no refund.").
		AtLeast(0.7)
}
