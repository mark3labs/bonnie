package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// runServe mounts the HTTP channel over a file-backed journal.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "address to listen on")
	dir := fs.String("journal", ".bonnie", "journal directory")
	model := fs.String("model", "", "model to use, for example anthropic/claude-sonnet-4-5")
	prompt := fs.String("system-prompt", "", "system prompt override")
	sandboxKind := fs.String("sandbox", "none", "tool sandbox: none, docker, microsandbox, local, or auto")
	sandboxImage := fs.String("sandbox-image", "", "sandbox image, for example python:3.12-slim")
	denyNet := fs.Bool("sandbox-deny-network", false, "block all network egress from the sandbox")
	timeout := fs.Duration("shutdown-timeout", 30*time.Second, "how long to wait for in-flight turns on shutdown")
	if err := fs.Parse(args); err != nil {
		return err
	}

	journal, err := runtime.OpenFileJournal(*dir)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	var opts []kit.Option
	if *model != "" {
		opts = append(opts, kit.WithModel(*model))
	}
	if *prompt != "" {
		opts = append(opts, kit.WithSystemPrompt(*prompt))
	}

	factory, err := agentFactory(*sandboxKind, *sandboxImage, *denyNet, opts)
	if err != nil {
		return err
	}

	runner := runtime.NewRunner(journal, factory)
	channel := bonniehttp.New(runner)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           channel.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// A turn can wait on a model for minutes, and a stream waits for as
		// long as the client cares to listen, so neither gets a write
		// deadline.
	}

	// SIGINT stops new work and lets in-flight turns reach their next
	// checkpoint. A turn that is cut short still keeps its finished steps —
	// that is what the journal is for — but a clean stop is cheaper.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "bonnie: serving on %s, journal %s\n", *addr, *dir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "bonnie: shutting down, letting in-flight turns checkpoint")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return <-errs
}

// agentFactory builds the agent factory the server runs, with or without a
// tool sandbox.
//
// The default is "none", which runs Kit's core tools in the BONNIE process.
// That is the right default for a local developer and the wrong one for a
// server reachable from outside, so the banner says which mode is active
// rather than leaving an operator to guess.
func agentFactory(kind, image string, denyNetwork bool, opts []kit.Option) (runtime.AgentFactory, error) {
	if kind == "" || kind == "none" {
		if denyNetwork {
			return nil, fmt.Errorf("--sandbox-deny-network needs a sandbox; pass --sandbox docker")
		}
		return runtime.KitAgent(opts...), nil
	}

	provider, err := sandboxProvider(kind, image)
	if err != nil {
		return nil, err
	}
	if denyNetwork {
		net, ok := provider.(sandbox.Networked)
		if !ok {
			return nil, fmt.Errorf("the %s sandbox cannot control the network", provider.Name())
		}
		if err := net.SetNetworkPolicy(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := provider.Available(ctx); err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "bonnie: tools run in the %s sandbox\n", provider.Name())
	return sandbox.Agent(provider, opts...), nil
}

// sandboxProvider maps the flag to a backend.
func sandboxProvider(kind, image string) (sandbox.Provider, error) {
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
		// Say this out loud. A user who picks "local" expecting isolation
		// gets none, and nothing else in the output would tell them.
		fmt.Fprintln(os.Stderr,
			"bonnie: WARNING the local sandbox gives NO isolation: tool calls run "+
				"as this process, with its files, network, and credentials")
		return sandbox.Local(), nil

	case "auto":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Never auto-select local: falling back from isolation to none must
		// be a decision someone wrote down.
		return sandbox.Select(ctx, sandbox.Microsandbox(), sandbox.Docker())

	default:
		return nil, fmt.Errorf("unknown sandbox %q: want none, docker, microsandbox, local, or auto", kind)
	}
}
