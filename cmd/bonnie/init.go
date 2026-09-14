package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/agent"
)

// initOpts carries the parsed flags of `bonnie init`.
type initOpts struct {
	model string
	tools bool
}

// newInitCmd mounts `bonnie init`.
func newInitCmd() *cobra.Command {
	var o initOpts
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold an agent tree",
		Long: `Scaffold an agent tree: an instructions file, a Go module, and a main.go
that is one call. The freshly scaffolded tree builds and serves out of the box.

  bonnie init my-agent       a new tree
  bonnie init .              adopt this directory — init never overwrites

  cd my-agent
  go mod tidy                fetch bonnie and kit from the public proxy
  bonnie dev                 serve it with hot reload and a terminal
  bonnie build               compile it into one static binary

There is no manifest. The tree's data lives at its default paths —
instructions.md is the system prompt, workspace/ is the agent's root for
files — and everything else is an option in main.go, so a setting that does
not exist is a compile error rather than a key nothing reads.

--tools adds a sample Go tool (tools/echo) so there is something wired by
codegen to see. The mark3labs modules are public; no go.work or private access
is needed. The model goes into main.go with --model.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			created, err := agent.Scaffold(dir, agent.InitOptions{
				Model:   o.model,
				Tools:   o.tools,
				Version: buildVersion(),
			})
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(os.Stdout, "Scaffolded an agent at %s:\n", dir)
			for _, f := range created {
				_, _ = fmt.Fprintf(os.Stdout, "  %s\n", f)
			}
			_, _ = fmt.Fprintf(os.Stdout, `
Next:
  edit instructions.md     make the agent yours
  go mod tidy              fetch bonnie and kit from the public proxy
  bonnie dev               serve it with hot reload and a terminal
  bonnie build             compile it into one static binary
`)
			if o.tools {
				_, _ = fmt.Fprint(os.Stdout, `
A sample tool lives in tools/echo. bonnie build wires it from codegen; replace
it with tools of your own.
`)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.model, "model", "", "model to write into main.go, for example anthropic/claude-sonnet-4-5")
	f.BoolVar(&o.tools, "tools", false, "add a sample Go tool to the module")
	return cmd
}
