package main

import (
	"context"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/internal/agentcmd"
	"github.com/mark3labs/bonnie/internal/tui"
)

// newChatCmd connects the shared terminal command to the CLI.
func newChatCmd() *cobra.Command {
	return agentcmd.NewChatCommand("127.0.0.1:8080")
}

// runTUIClient lets dev supply its client to the shared terminal interface.
func runTUIClient(ctx context.Context, c tui.Client, address string, logs *tui.LogWriter) error {
	return agentcmd.RunTUIClient(ctx, c, address, logs)
}

func chatURL(addr string) string {
	return agentcmd.ChatURL(addr)
}

// tuiAddress is the conversation key a dev session uses. It derives from the
// agent directory name so two dev sessions in separate trees do not share a
// conversation, while a re-run in the same tree resumes the same run.
func tuiAddress(root string) string {
	return "dev-" + filepath.Base(root)
}
