// Command serves the github-bot agent tree: an agent that speaks on GitHub.
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
//	GITHUB_APP_ID           the App's numeric ID
//	GITHUB_APP_PRIVATE_KEY  the App's private key, PEM, in full
//	GITHUB_WEBHOOK_SECRET   the secret GitHub signs each delivery with
//	GITHUB_INSTALLATION_ID  optional; only a proactive hand-off needs it
//
// A missing one is a startup error that names the variable. GitHub must be
// able to reach the process, so the webhook needs a public URL; README.md
// uses a tunnel for that.
package main

import (
	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channel/github"
)

func main() {
	bonnie.New(
		// The name GET /bonnie/v1/info reports.
		bonnie.WithName("github-bot"),

		// Every tool call runs in a sandbox. The default is landlock, which
		// has no network, so `git clone` cannot reach GitHub. For the
		// agent-fix coding path, select Docker (its image needs git) and an
		// allow-list network policy:
		//
		//	bonnie.WithSandbox(sandbox.Docker()),
		//	bonnie.WithNetwork(sandbox.NetworkPolicy{
		//		Mode:  sandbox.NetworkAllowList,
		//		Allow: []string{"github.com", "*.githubusercontent.com", "codeload.github.com"},
		//	}),

		bonnie.WithGitHub(github.Config{
			// The invocation token: a comment containing "@bonnie" reaches
			// the agent. It is a setting, not a secret, so it is code; the
			// credentials come from the environment.
			BotName: "bonnie",

			// A new issue gets an unprompted triage comment. The hook is
			// what makes the agent proactive: without it, the channel
			// answers comments and ignores everything else.
			//
			// OnIssue fires for EVERY issues action, so the hook filters.
			// A new issue is triaged; an issue that a maintainer labels
			// "agent-fix" is a request to write the change. The channel
			// adds the checkout descriptor to the turn either way, so the
			// coding branch has what it needs to clone.
			OnIssue: func(c github.IssueCtx) *chat.Turn {
				switch {
				case c.Action == "opened":
					return &chat.Turn{
						Text: "A new issue was opened: " + c.Title +
							". Summarise what it asks for in two sentences, and say what " +
							"you would need to know to act on it. Be brief.",
					}
				case c.Action == "labeled" && c.Label == "agent-fix":
					return &chat.Turn{
						Text: "This issue was labelled agent-fix: " + c.Title +
							". Clone the repository from the checkout descriptor in " +
							"your context, branch from the default branch, make the " +
							"change the issue asks for, run the tests, and describe what " +
							"you did. Do not claim to have opened a pull request unless " +
							"you actually pushed a branch.",
					}
				default:
					// Every other issue action — closed, assigned, another
					// label — is not this agent's business.
					return nil
				}
			},
		}),
	).Serve()
}
