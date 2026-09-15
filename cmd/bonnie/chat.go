package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/cmd/bonnie/tui"
)

// chatOpts carries the parsed flags of `bonnie chat`.
type chatOpts struct {
	addr string
	run  string
}

// newChatCmd mounts `bonnie chat` — the built-in terminal interface.
//
// It connects to a running bonnie HTTP channel and gives you one durable
// conversation in a terminal: type a message, watch the tool calls and the
// answer stream in, answer a parked run as the agent asks. `dev` launches it
// against its own child; this command talks to any server.
func newChatCmd() *cobra.Command {
	var o chatOpts
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Interact with an agent over the HTTP channel in a terminal",
		Long: `Open a terminal for one durable conversation with an agent.

  bonnie chat                 connect to http://127.0.0.1:8080
  bonnie chat --addr :9090    connect to a different channel
  bonnie dev                  the same TUI, plus hot reload

The conversation is one run, journalled by the server. Kill this terminal and
the run survives: re-open the TUI with the same --run address and the
conversation continues from the journal.

Keys: enter sends, ctrl+w cancels a running turn, ctrl+c quits.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runChat(o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.addr, "addr", "127.0.0.1:8080", "the HTTP channel address to connect to")
	f.StringVar(&o.run, "run", "tui-default", "conversation address (the run the server resolves it to)")
	return cmd
}

// runChat connects to the channel and runs the TUI until the user leaves.
func runChat(o chatOpts) error {
	url := chatURL(o.addr)
	fmt.Fprintf(os.Stderr, "bonnie: chat: connecting to %s\n", url)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runTUI(ctx, url, o.run)
}

// runTUI opens the built-in terminal interface against a running channel. It
// is shared by `bonnie chat` and `bonnie dev`. address is the conversation
// key the channel resolves to one run.
func runTUI(ctx context.Context, url, address string) error {
	return runTUIClient(ctx, client.New(url), address)
}

func runTUIClient(ctx context.Context, c tui.Client, address string) error {
	model := tui.New(c, ctx, address)
	p := tea.NewProgram(model)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("bonnie: chat: %w", err)
	}
	return nil
}

// chatURL normalises an address into a base URL the HTTP client accepts. A
// bare ":8080" becomes "http://127.0.0.1:8080"; a full URL passes through.
func chatURL(addr string) string {
	if len(addr) > 7 && (addr[:7] == "http://" || addr[:8] == "https://") {
		return addr
	}
	host := addr
	// Strip a leading colon, the listen form ":8080".
	host = trimColon(host)
	if host == "" {
		host = "127.0.0.1:8080"
	}
	return "http://" + host
}

func trimColon(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}

// tuiAddress is the conversation key a dev session uses. It derives from the
// agent directory name so two dev sessions in separate trees do not share a
// conversation, while a re-run in the same tree resumes the same run.
func tuiAddress(root string) string {
	name := filepath.Base(root)
	return "dev-" + name
}
