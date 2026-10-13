package agentcmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/internal/tui"
)

// NewChatCommand makes a terminal chat command for a running HTTP channel.
// defaultAddr sets the default --addr value. The command does not start a server.
// Use --token to send a bearer token on each HTTP request.
func NewChatCommand(defaultAddr string) *cobra.Command {
	return newChatCommand(defaultAddr, runTUIClient)
}

func newChatCommand(defaultAddr string, run func(context.Context, tui.Client, string, bool) error) *cobra.Command {
	var addr, runID, token string
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Interact with an agent over the HTTP channel in a terminal",
		Long: `Open a terminal for one durable conversation with a running agent.

Use --addr to select the HTTP channel and --token to send a bearer token.
This command does not start a server.

Use --run with an existing run ID to load its history and continue that run.
Without --run, the terminal uses the default conversation address.

Keys: enter sends, ctrl+w cancels a running turn, ctrl+c quits.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url := ChatURL(addr)
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "bonnie: chat: connecting to %s\n", url); err != nil {
				return fmt.Errorf("bonnie: chat: write connection message: %w", err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			var opts []client.Option
			if token != "" {
				opts = append(opts, client.WithBearerToken(token))
			}
			target := "tui-default"
			if cmd.Flags().Changed("run") {
				if strings.TrimSpace(runID) == "" {
					return fmt.Errorf("bonnie: chat: --run needs a run ID")
				}
				target = runID
			}
			return run(ctx, client.New(url, opts...), target, cmd.Flags().Changed("run"))
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultAddr, "the HTTP channel address to connect to")
	cmd.Flags().StringVar(&runID, "run", "", "existing run ID to continue with its saved history")
	cmd.Flags().StringVar(&token, "token", "", "bearer token for the HTTP channel")
	return cmd
}

// ChatURL converts a channel address to an HTTP base URL.
// A leading colon uses 127.0.0.1 as the host. An empty address uses
// http://127.0.0.1:8080. An HTTP or HTTPS URL is returned unchanged.
func ChatURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	return "http://" + addr
}

// RunTUI opens the terminal interface against a running HTTP channel at url.
// address is the conversation key that the channel resolves to one run.
// Use RunTUIClient to supply a client with authentication options.
func RunTUI(ctx context.Context, url, address string) error {
	return RunTUIClient(ctx, client.New(url), address)
}

// RunTUIClient opens the terminal interface with c until the user quits or
// ctx is canceled. address is the conversation key for the run.
// It does not start a server.
func RunTUIClient(ctx context.Context, c tui.Client, address string) error {
	return runTUIClient(ctx, c, address, false)
}

func runTUIClient(ctx context.Context, c tui.Client, target string, byID bool) error {
	model := tui.New(c, ctx, target)
	if byID {
		model = tui.NewRun(c, ctx, target)
	}
	p := tea.NewProgram(model, tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("bonnie: chat: %w", err)
	}
	return nil
}
