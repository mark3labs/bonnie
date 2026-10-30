// Command serves the refund-desk agent tree: the refund assistant of a small
// shop, with evals that grade it.
//
// This tree was made with `bonnie init` and is run with the bonnie CLI, the
// same way as your own agent:
//
//	bonnie dev                       serve with hot reload and a terminal
//	bonnie eval --judge <model>      run the evals in refund_eval_test.go
//	bonnie build                     one static binary
//
// This file is yours; BONNIE never rewrites it. instructions.md is the
// system prompt and holds the refund policy; orders.go has the tools, and
// bonnie_gen.go is the wiring `bonnie dev` and `bonnie build` regenerate.
//
// The agent is built in newAgent, not in main. main serves it, and each eval
// builds its own with it, so the evals grade the options this binary ships.
package main

import (
	"github.com/mark3labs/bonnie"
)

func main() {
	newAgent().Serve()
}

// newAgent builds the agent. main serves it, and each eval in a
// *_eval_test.go file builds its own with it, so an eval tests the agent
// this binary ships. The bonnie eval command runs them.
func newAgent() *bonnie.Agent {
	return bonnie.New(
		// The name GET /bonnie/v1/info reports.
		bonnie.WithName("refund-desk"),

		// lookup_order and issue_refund. request_approval is built in.
		bonnie.WithToolFactory(orderTools),
	)
}
