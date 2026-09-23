// Command serves the slack-bot agent tree: an agent that speaks in Slack.
//
// This tree was made with `bonnie init` and is run with the bonnie CLI, the
// same way as your own agent:
//
//	bonnie dev --addr 127.0.0.1:8081 --tui=false   serve with hot reload
//	bonnie build                                   one static binary
//	bonnie runs list --journal .bonnie             inspect the durable runs
//
// This file is yours; BONNIE never rewrites it. The tree's data lives at its
// default paths — instructions.md is the system prompt, workspace/ is the
// agent's root for files — and bonnie_gen.go is the wiring `bonnie dev` and
// `bonnie build` regenerate. Everything else is an option below.
//
// The credentials come from the environment, because they are secrets:
//
//	SLACK_BOT_TOKEN       the OAuth bot token, starts xoxb-
//	SLACK_SIGNING_SECRET  the secret Slack signs each delivery with
//
// A missing one is a startup error that names the variable. Slack must be
// able to reach the process, so the Events API needs a public URL; README.md
// uses a tunnel for that.
package main

import (
	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/channel/slack"
)

func main() {
	bonnie.New(
		// The name GET /bonnie/v1/info reports.
		bonnie.WithName("slack-bot"),

		bonnie.WithSlack(slack.Config{
			// One placeholder message per thread shows the agent is working
			// and is edited in place, then deleted when the reply posts. It
			// needs only the chat:write scope the bot already has to answer.
			// ActivityOff shows nothing; ActivityStatus needs Slack's AI
			// assistant surface.
			Activity: slack.ActivityMessage,
		}),
	).Serve()
}
