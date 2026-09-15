package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// An approval answer reaches the model as a verdict, not as whatever words
// happened to come with it.
//
// InputResponse.Approved was declared and read by nothing: only Text ever
// reached the model, so a run parked by ApprovalTool and answered with a
// bare `{"approved": true}` resumed the agent with an EMPTY message. A
// structured approval — a button, a checkbox, an API field — had no way to
// say yes.
func TestApprovalVerdictReachesTheModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		resp InputResponse
		want string
	}{
		{"a bare approval is not silence", Approve(""), "approved"},
		{"a bare rejection is not silence", Reject(""), "rejected"},
		{"the verdict leads and the words follow", Approve("but stage it first"), "approved: but stage it first"},
		{"a refusal carries its reason", Reject("that drops production"), "rejected: that drops production"},
		{"a plain answer is unchanged", InputResponse{Text: "eu-west-1"}, "eu-west-1"},
		{"an empty plain answer stays empty", InputResponse{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := renderResponses([]InputResponse{c.resp}); got != c.want {
				t.Fatalf("renderResponses = %q, want %q", got, c.want)
			}
		})
	}
}

// Rejection and "said nothing about approval" are different answers, and a
// plain bool could not tell them apart: false is its zero value, so an
// omitted field and an explicit refusal were the same value. That is why
// Approved is a pointer.
func TestRejectionIsDistinctFromSilence(t *testing.T) {
	t.Parallel()
	silent := InputResponse{Text: "go on"}
	refused := Reject("go on")

	if renderResponses([]InputResponse{silent}) == renderResponses([]InputResponse{refused}) {
		t.Fatal("a refusal and an answer that never mentioned approval render the same")
	}
	if silent.Approved != nil {
		t.Fatal("an answer that said nothing about approval must leave Approved nil")
	}
	if refused.Approved == nil || *refused.Approved {
		t.Fatalf("Reject built %+v, want an explicit false", refused.Approved)
	}
}

// The three states survive the wire, which is where they matter: a channel
// hands the runtime what a client sent.
func TestApprovalRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`{"text":"yes"}`:                 "yes",
		`{"text":"","approved":true}`:    "approved",
		`{"text":"","approved":false}`:   "rejected",
		`{"text":"ok","approved":true}`:  "approved: ok",
		`{"text":"no","approved":false}`: "rejected: no",
		`{"text":"just words"}`:          "just words",
	}
	for raw, want := range cases {
		var resp InputResponse
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := renderResponses([]InputResponse{resp}); got != want {
			t.Fatalf("%s rendered %q, want %q", raw, got, want)
		}
	}

	// An answer with no verdict does not invent one on the way out.
	out, err := json.Marshal(InputResponse{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "approved") {
		t.Fatalf("encoded %s, want no approval field", out)
	}
}

// Several answers to one suspension each keep their own verdict.
func TestEveryAnswerKeepsItsOwnVerdict(t *testing.T) {
	t.Parallel()
	got := renderResponses([]InputResponse{
		Approve("the migration"),
		Reject("the backfill"),
		{Text: "and use eu-west-1"},
	})
	want := "approved: the migration\nrejected: the backfill\nand use eu-west-1"
	if got != want {
		t.Fatalf("renderResponses =\n%q\nwant\n%q", got, want)
	}
}

// The end-to-end claim: a run parked for approval and answered with a bare
// verdict resumes with that verdict as its message. Before, the agent was
// handed an empty string and had to guess what the human decided.
func TestResumeCarriesABareApprovalToTheTurn(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()

	agent := &fakeAgent{
		turns: []*kit.TurnResult{
			{Response: "may I deploy?", FinalValue: SuspendRequest{
				Kind: SuspendApproval, Prompt: "Deploy to production?",
			}},
			{Response: "deployed"},
		},
	}
	r := NewRunner(journal, func(_ context.Context, s *Session) (Agent, error) {
		agent.session = s
		return agent, nil
	})
	ctx := context.Background()

	parked, err := r.Start(ctx, "approve-1", Input{Text: "deploy"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if parked.State != RunWaiting {
		t.Fatalf("state = %q, want waiting", parked.State)
	}

	// The human clicks "approve" and types nothing.
	if _, err := r.Resume(ctx, "approve-1", []InputResponse{Approve("")}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	recs, err := journal.Replay(ctx, "approve-1")
	if err != nil {
		t.Fatal(err)
	}
	var resume string
	for _, rec := range recs {
		if rec.Kind == RecordResume {
			resume = rec.Text
		}
	}
	if resume != "approved" {
		t.Fatalf("the journalled resume is %q, want %q: a bare approval must not resume with silence", resume, "approved")
	}
}
