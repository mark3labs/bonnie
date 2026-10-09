package main

import (
	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/internal/agentcmd"
)

func newSchedulesCmd() *cobra.Command {
	return agentcmd.NewSchedulesCommand(bonnie.DefaultAddr)
}
