// Command bonnie is the BONNIE developer CLI.
//
// The framework is usable as a library without this binary; the CLI exists for
// local development and for inspecting durable runs.
//
// The CLI renders its own help and errors with fang. It ships no
// interactive interface; the framework packages below it render no terminal
// at all today. The hard boundary this repo enforces is the Kit one above.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
)

// version is set by the linker at release time.
var version = "dev"

func main() {
	if err := fang.Execute(context.Background(), newRootCmd(),
		// fang detects the version from build info, which reports "unknown"
		// for a `go run` binary and misses the linker-injected release
		// version. Hand it over explicitly so `--version` and the version
		// subcommand agree.
		fang.WithVersion(buildVersion()),
		fang.WithErrorHandler(fangErrorHandler)); err != nil {
		os.Exit(1)
	}
}

// newRootCmd assembles the command tree. main runs it through fang; tests
// drive it with SetArgs, which exercises the same wiring without the styling.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bonnie",
		Short: "BONNIE — Builder Of Neural Network Intelligence Engines",
		Long: `Durable agent runs for Go.

Survive a crash. Wait days for a human. Answer over HTTP.

The framework is a library first: runtime.NewRunner and channel/http need no
binary at all. This CLI exists for local development and for inspecting
durable runs.

Commands:
  serve    serve an agent tree or a hand-wired library over HTTP
  dev      run an agent tree with hot reload and the built-in TUI
  chat     interact with a running agent in a terminal
  build    compile an agent tree into one static binary
  init     scaffold a new agent tree
  runs     inspect durable runs
  sandbox  manage sandbox workspaces

Planned:
  eval     Run evals against a local or remote agent`,
		Version: buildVersion(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.AddCommand(newServeCmd(), newRunsCmd(), newSandboxCmd(), newInitCmd(), newBuildCmd(), newDevCmd(), newChatCmd(), newVersionCmd())
	return root
}

// fangErrorHandler renders an error through fang with exactly one "bonnie:"
// on the front.
//
// Library errors are already wrapped `bonnie: context: ...` by convention, so
// prefixing unconditionally produced "bonnie: bonnie: ...". Errors raised by
// the CLI itself carry no prefix and need one.
func fangErrorHandler(w io.Writer, styles fang.Styles, err error) {
	fang.DefaultErrorHandler(w, styles, errors.New(prefixed(err)))
}

// prefixed adds the "bonnie:" prefix unless the error already carries one.
func prefixed(err error) string {
	msg := err.Error()
	if strings.HasPrefix(msg, "bonnie:") {
		return msg
	}
	return "bonnie: " + msg
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the BONNIE version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("bonnie", buildVersion())
			return nil
		},
	}
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return version
}
