// Command serves the triage-desk agent tree: a support triage agent that
// routes tickets with a decision model.
//
// This tree was made with `bonnie init` and is run with the bonnie CLI, the
// same way as your own agent:
//
//	bonnie dev                       serve with hot reload and a terminal
//	bonnie build                     one static binary
//	bonnie runs list --journal .bonnie   inspect the durable runs
//
// This file is yours; BONNIE never rewrites it. The tree's data lives at its
// default paths — instructions.md is the system prompt, context/ holds the
// inbox copied into each run — and bonnie_gen.go is the wiring `bonnie dev`
// and `bonnie build` regenerate. Everything else is an option below.
//
// A decision model answers typed questions with probabilities, not text.
// This tree uses one in two ways:
//
//   - file_ticket (triage.go) asks it where each ticket goes. The call is
//     journalled and keyed on the ticket ID, so filing a ticket again, or
//     recovering the run after a crash, gives the same routing without a
//     second model call.
//   - WithDecisionTool gives the chat model the decide tool, to ask its own
//     questions, for example whether a ticket is spam.
//
// The decision model reads its key from the environment, as a chat model
// does: OPENROUTER_API_KEY for the model below.
package main

import (
	"github.com/mark3labs/bonnie"
)

// decisionModel is the decision model, in "provider/model" form. A local
// Ollama model needs no key, for example "ollama/pplx-decider". List the
// others with kit.GetDecisionModels.
const decisionModel = "openrouter/perplexity/pplx-decider-flash-v1.2"

func main() {
	bonnie.New(
		// The name GET /bonnie/v1/info reports.
		bonnie.WithName("triage-desk"),

		bonnie.WithDecisionModel(decisionModel),
		bonnie.WithDecisionTool(),
		bonnie.WithToolFactory(fileTicketTools),
	).Serve()
}
