package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/agent"
)

// initOpts carries the parsed flags of `bonnie init`.
type initOpts struct {
	format string
	title  string
	model  string
	tools  bool
}

// newInitCmd mounts `bonnie init`.
func newInitCmd() *cobra.Command {
	var o initOpts
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold an agent tree",
		Long: `Scaffold an agent tree: a manifest, an instructions file, and the
seed directories. The default scaffold needs no Go at all — edit
instructions.md, set a model, and serve.

  bonnie init my-agent       a new tree
  bonnie init .              adopt this directory — init never overwrites

With --tools the tree gains a Go module, a sample tool, and a main.go that
serves it. The mark3labs modules are public, so run go mod tidy from the
tree and it resolves them from the proxy; no go.work or private access is
needed.

The model goes into the manifest with --model, so the first serve already
knows what to call.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			created, err := agent.Scaffold(dir, agent.InitOptions{
				Format: o.format,
				Title:  o.title,
				Model:  o.model,
				Tools:  o.tools,
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
  edit instructions.md      make the agent yours
  bonnie serve --agent %s   serve it over HTTP
`, dir)
			if o.tools {
				_, _ = fmt.Fprint(os.Stdout, `
The tree is a Go module. The mark3labs modules are public, so run go mod tidy
from the tree and it resolves them from the proxy. Then:

  go mod tidy              fetch bonnie and kit from the proxy
  go build ./...           the fresh scaffold compiles
  ./`+scaffoldName(dir)+`                  serves on :8080
`)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.format, "format", "yaml", "manifest format: yaml, toml, or json")
	f.StringVar(&o.title, "title", "", "agent title (default: the directory's name)")
	f.StringVar(&o.model, "model", "", "model to write into the manifest, for example anthropic/claude-sonnet-4-5")
	f.BoolVar(&o.tools, "tools", false, "add a Go module with one sample tool")
	return cmd
}

// scaffoldName is the directory name for the printed run line. For "init ."
// the current directory's base name is used.
func scaffoldName(dir string) string {
	name := filepath.Base(dir)
	if name == "." {
		if wd, err := os.Getwd(); err == nil {
			name = filepath.Base(wd)
		}
	}
	return name
}
