package main

import (
	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/internal/agentcmd"
)

// newRunsCmd uses the same journal commands as compiled agents.
func newRunsCmd() *cobra.Command {
	return agentcmd.NewRunsCommand(defaultJournalDir)
}

// Keep these constructors for CLI tests that execute one command at a time.
func newRunsListCmd() *cobra.Command { return runsSubcommand("list") }
func newRunsShowCmd() *cobra.Command { return runsSubcommand("show") }

func runsSubcommand(name string) *cobra.Command {
	root := newRunsCmd()
	for _, cmd := range root.Commands() {
		if cmd.Name() == name {
			root.RemoveCommand(cmd)
			return cmd
		}
	}
	panic("unknown runs command: " + name)
}
