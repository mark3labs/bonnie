// Command github-bot is a reference for BONNIE's GitHub channel: the one
// bonnie.New call that turns a program into an agent which speaks on GitHub.
//
// It is a library example, not an agent tree. Like everything under
// examples/, it is a package inside BONNIE's own module, so `go build ./...`
// compiles it and it cannot go stale. Run it the way the module runs:
//
//	go run ./examples/github-bot
//
// Your own agent is a TREE instead — its own Go module from `bonnie init`,
// run with `bonnie dev` and shipped with `bonnie build`. The bonnie.New call
// below is exactly what you put in that tree's main.go; see README.md for the
// whole live-test loop, including how to make the GitHub App by hand.
//
// The credentials come from the environment, because they are secrets:
//
//	GITHUB_APP_ID           the App's numeric ID
//	GITHUB_APP_PRIVATE_KEY  the App's private key, PEM, in full
//	GITHUB_WEBHOOK_SECRET   the secret GitHub signs each delivery with
//	GITHUB_INSTALLATION_ID  optional; only a proactive hand-off needs it
//
// A missing one is a startup error that names the variable.
//
// GitHub must be able to reach the process, so the webhook needs a public
// URL. README.md uses a tunnel for that.
package main

import (
	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channel/github"
)

const systemPrompt = `You are a helpful engineering assistant that lives on GitHub.

You speak in issue comments, pull request comments, and review threads. Keep
the answer short: a comment is a message, not a document. Use GitHub markdown.

When the turn is about a pull request, the diff is given to you in the context
of the message. Read it before you answer, and name the file and the line when
you point at something.

Never claim you changed a repository. You can read and you can answer; you
cannot push.`

func main() {
	bonnie.New(
		// In a scaffolded tree the prompt is instructions.md, and the
		// workspace, skills, and journal are files at their default paths.
		// They are options here only because this example has no tree.
		bonnie.WithSystemPrompt(systemPrompt),
		bonnie.WithName("github-bot"),
		bonnie.WithWorkspace(".bonnie/workspace"),
		bonnie.WithInstructions(""),
		bonnie.WithSkills(""),
		bonnie.WithGitHub(github.Config{
			// The invocation token: a comment containing "@bonnie" reaches
			// the agent. It is a setting, not a secret, so it is code; the
			// credentials come from the environment.
			BotName: "bonnie",

			// A new issue gets an unprompted triage comment. The hook is
			// what makes the agent proactive: without it, the channel
			// answers comments and ignores everything else.
			OnIssue: func(c github.IssueCtx) *chat.Turn {
				return &chat.Turn{
					Text: "A new issue was opened: " + c.Title +
						". Summarise what it asks for in two sentences, and say what " +
						"you would need to know to act on it. Be brief.",
				}
			},
		}),
	).Serve()
}
