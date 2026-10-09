package main

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/internal/agentcmd"
	"github.com/mark3labs/bonnie/internal/tui"
)

// chatOpts carries the parsed flags of `bonnie chat`.
//
//nolint:unused // Keep the CLI helper API for tests and dev callers.
type chatOpts struct {
	addr string
	run  string
}

// newChatCmd connects the shared terminal command to the CLI.
func newChatCmd() *cobra.Command {
	return agentcmd.NewChatCommand("127.0.0.1:8080")
}

// runChat connects to the channel and runs the TUI until the user leaves.
//
//nolint:unused // Keep the existing CLI helper signature.
func runChat(o chatOpts) error {
	cmd := newChatCmd()
	cmd.SetArgs([]string{"--addr", o.addr, "--run", o.run})
	return cmd.Execute()
}

// runTUI opens the terminal interface against a running channel.
//
//nolint:unused // Keep the existing dev helper signature.
func runTUI(ctx context.Context, url, address string) error {
	return agentcmd.RunTUI(ctx, url, address)
}

// runTUIClient lets dev supply its client to the shared terminal interface.
func runTUIClient(ctx context.Context, c tui.Client, address string) error {
	return agentcmd.RunTUIClient(ctx, c, address)
}

func chatURL(addr string) string {
	return agentcmd.ChatURL(addr)
}

//nolint:unused // Keep the existing address helper signature.
func trimColon(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

// tuiAddress is the conversation key a dev session uses. It derives from the
// agent directory name so two dev sessions in separate trees do not share a
// conversation, while a re-run in the same tree resumes the same run.
func tuiAddress(root string) string {
	return "dev-" + filepath.Base(root)
}
