package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
)

// routing is what file_ticket asks the decision model about each ticket.
var routing = []kit.DecisionQuestion{
	kit.ChoiceQuestion("team", "Which team must handle this ticket?",
		kit.DecisionOption{Name: "billing", Description: "Charges, refunds, invoices"},
		kit.DecisionOption{Name: "technical", Description: "Outages, errors, bugs"},
		kit.DecisionOption{Name: "account", Description: "Logins, profile, access"},
		kit.DecisionOption{Name: "spam", Description: "Not a real support request"},
	),
	kit.ScoreQuestion("urgency", "How urgent is this ticket?", "low", "normal", "high", "critical"),
	kit.BooleanQuestion("security", "Does the ticket report a possible account takeover or other security incident?"),
}

// filed is what file_ticket returns to the model.
type filed struct {
	ID      string `json:"id"`
	Queue   string `json:"queue"`
	Urgency string `json:"urgency"`
	Action  string `json:"action"`
}

// fileTicketTools builds the file_ticket tool for one run.
func fileTicketTools(_ context.Context, scope bonnie.ToolScope) ([]kit.Tool, error) {
	type input struct {
		ID   string `json:"id" description:"The ticket ID, for example T-1042."`
		Text string `json:"text" description:"The full text of the ticket."`
	}
	// file_ticket has no effect outside the journal, and its decision is
	// keyed, so a call that a crash interrupted can run again: it gets the
	// stored routing back.
	scope.Session.AddReplaySafeTools("file_ticket")

	tool := kit.NewTool("file_ticket",
		"Route one support ticket to a queue. Returns the queue, the urgency and the action.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			if in.ID == "" || in.Text == "" {
				return kit.ErrorResult("give the ticket ID and its text"), nil
			}
			res, err := scope.Decide(ctx, bonnie.DecisionRequest{
				State:     kit.DecisionState{Text: in.Text},
				Questions: routing,
				// One routing per ticket for the life of the run.
				Key: "ticket:" + in.ID,
			})
			switch {
			case errors.Is(err, runtime.ErrDecisionKeyReused):
				return kit.ErrorResult(fmt.Sprintf("%s was already filed with a different text", in.ID)), nil
			case err != nil:
				// Fail to a human, never to a guess.
				return result(filed{ID: in.ID, Queue: "human", Urgency: "unknown", Action: "route by hand: the decision model is unavailable"})
			}
			return result(route(in.ID, res))
		})
	return []kit.Tool{tool}, nil
}

// route turns the model's probabilities into a queue. The thresholds are
// host policy: the decision model scores, this code decides.
func route(id string, res *kit.DecisionResult) filed {
	team, errTeam := res.Choice("team")
	urgency, errUrgency := res.Score("urgency")
	security, errSecurity := res.Boolean("security")
	if err := errors.Join(errTeam, errUrgency, errSecurity); err != nil {
		// A refused or missing answer goes to a person.
		return filed{ID: id, Queue: "human", Urgency: "unknown", Action: "route by hand: the decision model did not answer"}
	}
	levels := routing[1].Levels
	f := filed{ID: id, Queue: team.Choice, Urgency: levels[urgency.Level], Action: "queue"}
	switch {
	case security.Probability >= 0.5:
		f.Queue, f.Urgency, f.Action = "security", "critical", "page the on-call security engineer"
	case team.Choice == "spam":
		f.Action = "close without a reply"
	case urgency.Level == len(levels)-1:
		f.Action = "page the on-call engineer"
	}
	return f
}

func result(f filed) (kit.ToolOutput, error) {
	out, err := json.Marshal(f)
	if err != nil {
		return kit.ToolOutput{}, err
	}
	return kit.TextResult(string(out)), nil
}
