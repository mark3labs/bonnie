// Command slack-bot is a reference for BONNIE's Slack channel: the one
// bonnie.New call that turns a program into an agent which speaks in Slack.
//
// It is a library example, not an agent tree. Like everything under
// examples/, it is a package inside BONNIE's own module, so `go build ./...`
// compiles it and it cannot go stale. Run it the way the module runs:
//
//	go run ./examples/slack-bot
//
// Your own agent is a TREE instead — its own Go module from `bonnie init`,
// run with `bonnie dev` and shipped with `bonnie build`. The bonnie.New call
// below is exactly what you put in that tree's main.go; see README.md for the
// whole live-test loop, including how to make the Slack app by hand.
//
// The credentials come from the environment, because they are secrets:
//
//	SLACK_BOT_TOKEN       the OAuth bot token, starts xoxb-
//	SLACK_SIGNING_SECRET  the secret Slack signs each delivery with
//
// A missing one is a startup error that names the variable.
//
// Slack must be able to reach the process, so the Events API needs a public
// URL. README.md uses a tunnel for that.
package main

import (
	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/channel/slack"
)

const systemPrompt = `You are a helpful engineering assistant that lives in Slack.

You answer in channels the app is mentioned in, in threads you have joined,
and in direct messages. Keep the answer short: a Slack message is a message,
not a document. The reply is plain text, so write markdown the reader can
follow without rendering — a bare URL, not a link; a dash list, not a table.

Never claim you changed anything outside this conversation. You can read and
you can answer; you cannot act on the workspace.`

func main() {
	bonnie.New(
		// In a scaffolded tree the prompt is instructions.md, and the
		// workspace, skills, and journal are files at their default paths.
		// They are options here only because this example has no tree.
		bonnie.WithSystemPrompt(systemPrompt),
		bonnie.WithName("slack-bot"),
		bonnie.WithWorkspace(".bonnie/workspace"),
		bonnie.WithInstructions(""),
		bonnie.WithSkills(""),
		bonnie.WithSlack(slack.Config{
			// One placeholder message per thread shows the agent is working
			// and is edited in place, then deleted when the reply posts. It
			// needs only the chat:write scope the bot already has to answer.
			// ActivityOff shows nothing; ActivityStatus needs Slack's AI
			// assistant surface. See package slack's activity.go.
			Activity: slack.ActivityMessage,
		}),
	).Serve()
}
