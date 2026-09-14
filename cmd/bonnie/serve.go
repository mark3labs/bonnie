package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/channel/discord"
	"github.com/mark3labs/bonnie/channel/slack"
	"github.com/mark3labs/bonnie/channel/telegram"
	"github.com/mark3labs/bonnie/sandbox"
)

// serveOpts carries the parsed flags of `bonnie serve`.
type serveOpts struct {
	addr        string
	journal     string
	model       string
	prompt      string
	sandboxKind string
	sandboxImg  string
	denyNetwork bool
	shutdown    time.Duration
	slack       bool
	discord     bool
	telegram    bool
}

// newServeCmd mounts `bonnie serve`.
func newServeCmd() *cobra.Command {
	var o serveOpts
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Mount the HTTP channel and serve durable runs",
		Long: `Mount the HTTP channel over a file-backed journal and serve durable runs.

  POST /runs                 start a run, or resolve an address to one
  GET  /runs/{id}            report a run's durable state
  POST /runs/{id}            send a message to an existing run
  POST /runs/{id}/respond    answer a suspended run
  POST /runs/{id}/cancel     stop the turn a run is executing
  GET  /runs/{id}/stream     NDJSON event stream, resumable with ?cursor=

serve is the generic host: it runs an agent configured entirely by these
flags, with no agent tree. To serve a tree, run the tree — ` + "`bonnie dev`" + ` while
you work on it, ` + "`bonnie build`" + ` for the binary it graduates into. A tree's
configuration is Go in its own main.go, so serve cannot read it.

Without a sandbox, tool calls run as this process, with its files, network,
and credentials. Use --sandbox docker for a server that is reachable from
outside. See docs/SANDBOX.md.`,
		RunE: func(_ *cobra.Command, _ []string) error { return runServe(o) },
	}
	f := cmd.Flags()
	f.StringVar(&o.addr, "addr", bonnie.DefaultAddr, "address to listen on")
	addJournalFlag(f, &o.journal)
	f.StringVar(&o.model, "model", "", "model to use, for example anthropic/claude-sonnet-4-5")
	f.StringVar(&o.prompt, "system-prompt", "", "system prompt override")
	f.StringVar(&o.sandboxKind, "sandbox", "none", "tool sandbox: none, docker, microsandbox, local, or auto")
	f.StringVar(&o.sandboxImg, "sandbox-image", "", "sandbox image, for example python:3.12-slim")
	f.BoolVar(&o.denyNetwork, "sandbox-deny-network", false, "block all network egress from the sandbox")
	f.DurationVar(&o.shutdown, "shutdown-timeout", 30*time.Second, "how long to wait for in-flight turns on shutdown")
	f.BoolVar(&o.slack, "slack", false, "mount the Slack channel (credentials from the environment)")
	f.BoolVar(&o.discord, "discord", false, "mount the Discord channel (credentials from the environment)")
	f.BoolVar(&o.telegram, "telegram", false, "mount the Telegram channel (credentials from the environment)")
	return cmd
}

// runServe resolves the flags into options and serves until interrupted.
func runServe(o serveOpts) error {
	// SIGINT stops new work and lets in-flight turns reach their next
	// checkpoint. A turn that is cut short still keeps its finished steps —
	// that is what the journal is for — but a clean stop is cheaper.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts, err := serveOptions(ctx, o)
	if err != nil {
		return err
	}
	return bonnie.Run(ctx, opts...)
}

// serveOptions turns the flags into the options [bonnie.Run] takes. It is
// separate from runServe so tests can read the resolution without serving.
//
// serve has no tree, so it takes neither an instructions file nor a
// workspace: the process's own directory stays the root, which is the
// historical behaviour of `bonnie serve`.
func serveOptions(ctx context.Context, o serveOpts) ([]bonnie.Option, error) {
	opts := []bonnie.Option{
		bonnie.WithAddr(o.addr),
		bonnie.WithJournal(o.journal),
		bonnie.WithModel(o.model),
		bonnie.WithSystemPrompt(o.prompt),
		bonnie.WithInstructions(""),
		bonnie.WithWorkspace(""),
		bonnie.WithShutdownTimeout(o.shutdown),
	}

	if o.sandboxKind != "" && o.sandboxKind != "none" {
		p, err := sandboxProvider(ctx, o.sandboxKind, o.sandboxImg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, bonnie.WithSandbox(p))
	}
	if o.denyNetwork {
		opts = append(opts, bonnie.WithNetwork(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}))
	}
	if o.slack {
		opts = append(opts, bonnie.WithSlack(slack.Config{}))
	}
	if o.discord {
		opts = append(opts, bonnie.WithDiscord(discord.Config{}))
	}
	if o.telegram {
		opts = append(opts, bonnie.WithTelegram(telegram.Config{}))
	}
	return opts, nil
}

// sandboxProvider maps the --sandbox flag to a backend.
//
// The image reaches every backend that can honour it, and a backend that
// cannot is refused rather than left to run its default in silence:
// accepting a setting and ignoring it is the same broken promise a backend
// makes when it swallows a network policy (docs/SPEC.md §8, invariant 13).
func sandboxProvider(ctx context.Context, kind, image string) (sandbox.Provider, error) {
	switch kind {
	case "docker":
		var o []sandbox.DockerOption
		if image != "" {
			o = append(o, sandbox.WithDockerImage(image))
		}
		return sandbox.Docker(o...), nil

	case "microsandbox", "msb":
		var o []sandbox.MicrosandboxOption
		if image != "" {
			o = append(o, sandbox.WithMicrosandboxImage(image))
		}
		return sandbox.Microsandbox(o...), nil

	case "local":
		if image != "" {
			return nil, fmt.Errorf("the local sandbox runs no image: drop sandbox-image, or pick --sandbox docker")
		}
		// Say this out loud. A user who picks "local" expecting isolation
		// gets none, and nothing else in the output would tell them.
		fmt.Fprintln(os.Stderr,
			"bonnie: WARNING the local sandbox gives NO isolation: tool calls run "+
				"as this process, with its files, network, and credentials")
		return sandbox.Local(), nil

	case "auto":
		selectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		// Whichever backend auto lands on must run the image the operator
		// asked for. Building the candidates without it made the image a
		// setting that worked or not depending on what the host had
		// installed.
		var (
			msb []sandbox.MicrosandboxOption
			dkr []sandbox.DockerOption
		)
		if image != "" {
			msb = append(msb, sandbox.WithMicrosandboxImage(image))
			dkr = append(dkr, sandbox.WithDockerImage(image))
		}
		// Never auto-select local: falling back from isolation to none must
		// be a decision someone wrote down.
		return sandbox.Select(selectCtx, sandbox.Microsandbox(msb...), sandbox.Docker(dkr...))

	default:
		return nil, fmt.Errorf("unknown sandbox %q: want none, docker, microsandbox, local, or auto", kind)
	}
}
