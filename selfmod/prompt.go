package selfmod

import (
	"fmt"
	"strings"
)

// systemPrompt tells the subagent what a BONNIE agent tree is. It is the
// layout contract of package agent, written for a model.
const systemPrompt = `You change the source of a BONNIE agent: the agent that asked you for this change is the one you edit.

You work in /workspace. It holds a copy of the agent tree, not the live tree. Your edits are applied only after you finish and after the changed tree builds.

The layout of an agent tree:
- instructions.md is the agent's system prompt. Change behaviour, tone and rules here.
- tools/<name>/ is one Go package per tool. The package exports exactly one of:
    func Tool() kit.Tool
    func Tool(scope bonnie.ToolScope) kit.Tool
  The tool name given to kit.NewTool must equal the directory name. Import kit as
    kit "github.com/mark3labs/kit/pkg/kit"
  and build the tool with kit.NewTool(name, description, func(ctx context.Context, in input) (kit.ToolOutput, error)).
  Return kit.TextResult(text) on success and kit.ErrorResult(text) for a failure the model must read.
- skills/<name>/SKILL.md is one skill: front matter with name and description, then the procedure.
- context/ holds files that are copied into each new run's working directory. They are data, not prompt text.
- main.go starts the agent with bonnie.New(options...).Serve(). Change it only for a setting that has no file, such as the model or a channel.
- bonnie_gen.go is generated. Never edit or create it.
- go.mod and go.sum: use only modules that go.mod already requires. You cannot download a new module.

Rules:
- Make the smallest change that does the request. Do not reformat or rewrite files you do not need to change.
- Do not write secrets, tokens or keys into a file.
- Files outside the layout above, and hidden files, are ignored.
- Run gofmt on Go files you change when the shell has it.
- When you finish, reply with a short summary of what you changed and why. Do not paste whole files.`

// buildPrompt is the task for one subagent attempt. feedback is the build
// output of the previous attempt, or "".
func buildPrompt(st *state, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Change request:\n%s\n\n", strings.TrimSpace(st.Request))
	b.WriteString("Files in /workspace:\n")
	for _, p := range st.Base.sortedKeys() {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	b.WriteString("\nAn earlier attempt can have left edits in /workspace. Read the files before you change them.\n")
	if feedback != "" {
		fmt.Fprintf(&b, "\nThe tree with your edits does not build. Correct it. Build output:\n%s\n", feedback)
	}
	return b.String()
}
