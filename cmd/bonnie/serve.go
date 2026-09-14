package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/discord"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/channel/slack"
	"github.com/mark3labs/bonnie/channel/telegram"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
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
	agentDir    string
	configPath  string
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

With --agent DIR, serve reads the agent tree at DIR — its manifest, its
instructions file, its sandbox and channel settings — and serves it with no
build and no Go toolchain. Flags override the manifest, and the startup
banner names the source that won. A tree that carries Go tools cannot be
honored by serve and is refused: build it instead with bonnie build.

Without a sandbox, tool calls run as this process, with its files, network,
and credentials. Use --sandbox docker for a server that is reachable from
outside. See docs/SANDBOX.md.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runServe(cmd.Flags(), o) },
	}
	f := cmd.Flags()
	f.StringVar(&o.addr, "addr", ":8080", "address to listen on")
	addJournalFlag(f, &o.journal)
	f.StringVar(&o.model, "model", "", "model to use, for example anthropic/claude-sonnet-4-5")
	f.StringVar(&o.prompt, "system-prompt", "", "system prompt override")
	f.StringVar(&o.sandboxKind, "sandbox", "none", "tool sandbox: none, docker, microsandbox, local, or auto")
	f.StringVar(&o.sandboxImg, "sandbox-image", "", "sandbox image, for example python:3.12-slim")
	f.BoolVar(&o.denyNetwork, "sandbox-deny-network", false, "block all network egress from the sandbox")
	f.DurationVar(&o.shutdown, "shutdown-timeout", 30*time.Second, "how long to wait for in-flight turns on shutdown")
	f.StringVar(&o.agentDir, "agent", "", "serve the agent tree at this directory")
	f.StringVar(&o.configPath, "config", "", "path of the manifest, instead of discovering one in the agent root")
	return cmd
}

// settingSource names where an effective setting came from. It is "flag",
// "default", or the manifest file's name. The banner prints it, because an
// operator must never guess which value won.
type settingSource string

const (
	srcFlag    settingSource = "flag"
	srcDefault settingSource = "default"
)

// serveConfig is the resolved configuration of one serve run: every setting
// after precedence, the sandbox network policy, and the banner lines.
type serveConfig struct {
	model        string
	prompt       string
	addr         string
	journal      string
	sandboxKind  string
	sandboxImage string
	network      *sandbox.NetworkPolicy
	title        string

	// workspace is the directory the agent's files live in: the working
	// directory of every file tool without a sandbox, and the seed mirrored
	// into [sandbox.Workspace] with one. Empty when serving no tree, which
	// leaves the process's own directory as the root.
	workspace string

	telegram *telegram.Config
	slack    *slack.Config
	discord  *discord.Config

	banner []string
}

// runServe resolves the configuration and serves until interrupted.
func runServe(flags *pflag.FlagSet, o serveOpts) error {
	cfg, err := resolveServe(flags, o)
	if err != nil {
		return err
	}

	// SIGINT stops new work and lets in-flight turns reach their next
	// checkpoint. A turn that is cut short still keeps its finished steps —
	// that is what the journal is for — but a clean stop is cheaper.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return serveHTTP(ctx, cfg, o.shutdown, nil)
}

// resolveServe loads the manifest when one is named or discovered, refuses
// trees it cannot fully honor, applies precedence — flags over manifest
// over defaults — and returns the effective configuration and its banner.
//
// The strictness of precedence is one code path (pick): the same rule
// decides every setting, so no setting can silently drift to its own
// precedence order.
func resolveServe(flags *pflag.FlagSet, o serveOpts) (*serveConfig, error) {
	changed := func(name string) bool { return flags.Changed(name) }
	val := func(name string) string { v, _ := flags.GetString(name); return v }

	// The manifest, when serving an agent tree.
	var manifest *agent.Manifest
	manifestPath := ""
	root := "."
	if o.agentDir != "" || o.configPath != "" {
		var err error
		if o.configPath != "" {
			manifestPath = o.configPath
			if o.agentDir != "" {
				root = o.agentDir
			} else {
				root = filepath.Dir(manifestPath)
			}
			manifest, err = agent.LoadFile(manifestPath)
		} else {
			root = o.agentDir
			manifest, manifestPath, err = agent.Load(root)
		}
		if err != nil {
			return nil, err
		}
		if err := refuseGoTree(root); err != nil {
			return nil, err
		}
	}
	manifestSrc := settingSource(filepath.Base(manifestPath))

	// pick resolves one string setting: flag over manifest over default.
	pick := func(flagName, manVal, def string) (string, settingSource) {
		switch {
		case changed(flagName):
			return val(flagName), srcFlag
		case manVal != "":
			return manVal, manifestSrc
		default:
			return def, srcDefault
		}
	}

	cfg := &serveConfig{}
	var addrSrc, journalSrc, modelSrc, sandboxSrc, imageSrc settingSource
	cfg.journal, journalSrc = pick("journal", "", defaultJournalDir)
	cfg.addr, addrSrc = pick("addr", manifestChannelAddr(manifest), ":8080")
	cfg.model, modelSrc = pick("model", manifestModel(manifest), "")
	sb := manifestSandbox(manifest)
	cfg.sandboxKind, sandboxSrc = pick("sandbox", sb.Kind, "none")
	cfg.sandboxImage, imageSrc = pick("sandbox-image", sb.Image, "")
	if manifest != nil {
		cfg.title = manifest.Title
	}

	// The workspace: the agent's root for files. A tree always has one, so
	// a model's write lands beside the seed files an author wrote and never
	// in the tree itself — where the manifest, the instructions, and the
	// journal live. Without a tree there is nothing to anchor to, and the
	// process's own directory stays the root.
	workspaceSrc := srcDefault
	if manifest != nil {
		if manifest.Workspace != "" {
			workspaceSrc = manifestSrc
		}
		abs, err := filepath.Abs(manifest.WorkspaceDir(root))
		if err != nil {
			return nil, fmt.Errorf("bonnie: workspace path: %w", err)
		}
		cfg.workspace = abs
	}

	// The system prompt: the flag wins outright. Otherwise the tree's
	// instructions file is the prompt, read fresh at start.
	if o.prompt != "" {
		cfg.prompt = o.prompt
	} else if manifest != nil {
		path, src := manifest.Instructions, manifestSrc
		if path == "" {
			path, src = "instructions.md", srcDefault
		}
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, fmt.Errorf("bonnie: the instructions file could not be read: %w", err)
		}
		cfg.prompt = string(b)
		cfg.banner = append(cfg.banner, fmt.Sprintf("bonnie: instructions %s (%s)", path, src))
	}

	// The network policy: the deny flag wins, then the manifest's policy.
	// An explicit allow-all is the default and requests no control at all.
	networkSrc := srcDefault
	switch {
	case o.denyNetwork:
		cfg.network = &sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}
		networkSrc = srcFlag
	case sb.Network != nil && sb.Network.Mode != "":
		networkSrc = manifestSrc
		switch sb.Network.Mode {
		case "deny-all":
			cfg.network = &sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}
		case "allow-list":
			cfg.network = &sandbox.NetworkPolicy{Mode: sandbox.NetworkAllowList, Allow: sb.Network.Allow}
		}
	}

	// A restrictive policy without a sandbox is a control nothing can
	// apply — the invariant is the same one a backend enforces one level
	// down. Refuse with the fix in the message.
	if cfg.network != nil && (cfg.sandboxKind == "" || cfg.sandboxKind == "none") {
		return nil, fmt.Errorf("bonnie: a network policy needs a sandbox: pass --sandbox docker, or set sandbox.kind in the manifest")
	}

	// Chat channels: a manifest key enables one, and its credentials come
	// from the environment — secrets never live in the manifest. A missing
	// secret is a startup error that names the variable, because a channel
	// that receives without verifying is a door with no lock.
	if m := manifest; m != nil && m.Channels != nil {
		if tg := m.Channels.Telegram; tg != nil {
			cfg.telegram = &telegram.Config{
				Token:    os.Getenv("TELEGRAM_BOT_TOKEN"),
				Secret:   os.Getenv("TELEGRAM_WEBHOOK_SECRET"),
				Username: tg.Username,
				Command:  tg.Command,
				APIURL:   os.Getenv("TELEGRAM_API_URL"),
				Path:     tg.Path,
			}
			if err := requireEnv("telegram", "TELEGRAM_BOT_TOKEN", "TELEGRAM_WEBHOOK_SECRET"); err != nil {
				return nil, err
			}
		}
		if s := m.Channels.Slack; s != nil {
			cfg.slack = &slack.Config{
				BotToken:      os.Getenv("SLACK_BOT_TOKEN"),
				SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
				APIURL:        os.Getenv("SLACK_API_URL"),
				Path:          s.Path,
			}
			if err := requireEnv("slack", "SLACK_BOT_TOKEN", "SLACK_SIGNING_SECRET"); err != nil {
				return nil, err
			}
		}
		if d := m.Channels.Discord; d != nil {
			cfg.discord = &discord.Config{
				BotToken:  os.Getenv("DISCORD_BOT_TOKEN"),
				PublicKey: os.Getenv("DISCORD_PUBLIC_KEY"),
				Command:   d.Command,
				APIURL:    os.Getenv("DISCORD_API_URL"),
				Path:      d.Path,
			}
			if err := requireEnv("discord", "DISCORD_BOT_TOKEN", "DISCORD_PUBLIC_KEY"); err != nil {
				return nil, err
			}
		}
	}

	// The banner: the setting and the source that won, one line each.
	line := func(label, value string, src settingSource) {
		cfg.banner = append(cfg.banner, fmt.Sprintf("bonnie: %s %s (%s)", label, value, src))
	}
	if cfg.title != "" {
		line("agent", cfg.title, manifestSrc)
	}
	line("serving on", cfg.addr, addrSrc)
	line("journal", cfg.journal, journalSrc)
	if cfg.model != "" {
		line("model", cfg.model, modelSrc)
	}
	line("sandbox", cfg.sandboxKind, sandboxSrc)
	if cfg.sandboxImage != "" {
		line("sandbox image", cfg.sandboxImage, imageSrc)
	}
	line("network", networkLabel(cfg.network), networkSrc)
	if cfg.workspace != "" {
		line("workspace", cfg.workspace, workspaceSrc)
	}
	if cfg.slack != nil {
		line("channel slack", displayPath(cfg.slack.Path, slack.DefaultPath), manifestSrc)
	}
	if cfg.discord != nil {
		line("channel discord", displayPath(cfg.discord.Path, discord.DefaultPath), manifestSrc)
	}
	if cfg.telegram != nil {
		line("channel telegram", displayPath(cfg.telegram.Path, telegram.DefaultPath), manifestSrc)
	}
	if cfg.sandboxKind == "" || cfg.sandboxKind == "none" {
		// The no-isolation warning, printed once at startup. The manifest
		// must not make "no sandbox" quieter than the flag does.
		cfg.banner = append(cfg.banner,
			"bonnie: WARNING no sandbox: tool calls run as this process, with its files, network, and credentials")
	}
	return cfg, nil
}

// networkLabel is the effective network mode for the banner.
func networkLabel(p *sandbox.NetworkPolicy) string {
	if p == nil {
		return "allow-all"
	}
	return string(p.Mode)
}

// refuseGoTree rejects a tree serve cannot fully honor, naming bonnie
// build. Silently skipping the user's tools would be the failure, not a
// graceful degradation: the model would run without the tools the author
// wrote, and nothing would say so.
func refuseGoTree(root string) error {
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		return fmt.Errorf("bonnie: this tree carries a Go module, which serve cannot load at run time — build it instead (bonnie build), or run its own main.go")
	}
	if entries, err := os.ReadDir(filepath.Join(root, "tools")); err == nil && len(entries) > 0 {
		return fmt.Errorf("bonnie: this tree carries Go tools (tools/), which serve cannot load at run time — build it instead (bonnie build)")
	}
	return nil
}

// serveHTTP opens the journal, wires the runner and channel, and serves
// until the context ends or the listener fails. The listener is injectable
// so tests can bind :0 and learn the port.
func serveHTTP(ctx context.Context, cfg *serveConfig, shutdown time.Duration, ln net.Listener) error {
	for _, l := range cfg.banner {
		fmt.Fprintln(os.Stderr, l)
	}

	journal, err := runtime.OpenFileJournal(cfg.journal)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	var opts []kit.Option
	if cfg.model != "" {
		opts = append(opts, kit.WithModel(cfg.model))
	}
	if cfg.prompt != "" {
		opts = append(opts, kit.WithSystemPrompt(cfg.prompt))
	}
	// The workspace anchors the agent's files. How it is applied depends on
	// the mode, so agentFactory owns it: host tools take a working
	// directory, a sandbox takes a seed. Mixing the two would put host tools
	// inside a sandboxed agent.
	if cfg.workspace != "" {
		if err := os.MkdirAll(cfg.workspace, 0o755); err != nil {
			return fmt.Errorf("bonnie: workspace: %w", err)
		}
	}

	factory, err := agentFactory(cfg.sandboxKind, cfg.sandboxImage, cfg.network, cfg.workspace, opts)
	if err != nil {
		return err
	}

	runner := runtime.NewRunner(journal, factory)

	// One mux carries every enabled channel: the HTTP transport always,
	// then the chat channels the manifest enabled.
	mux := http.NewServeMux()
	httpCh := bonniehttp.New(runner)
	mount(mux, httpCh)
	if cfg.telegram != nil {
		mount(mux, telegram.New(runner, *cfg.telegram))
	}
	if cfg.slack != nil {
		mount(mux, slack.New(runner, *cfg.slack))
	}
	if cfg.discord != nil {
		d, err := discord.New(runner, *cfg.discord)
		if err != nil {
			return err
		}
		mount(mux, d)
	}

	srv := &http.Server{
		Handler:           closeStreamsOnShutdown(ctx, mux),
		ReadHeaderTimeout: 10 * time.Second,
		// A turn can wait on a model for minutes, and a stream waits for as
		// long as the client cares to listen, so neither gets a write
		// deadline.
	}

	if ln == nil {
		ln, err = net.Listen("tcp", cfg.addr)
		if err != nil {
			return fmt.Errorf("bonnie: listen on %s: %w", cfg.addr, err)
		}
	}

	errs := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdown)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return <-errs
}

// closeStreamsOnShutdown closes long-lived event streams when server shutdown
// starts. Other requests keep their original context and can finish a
// checkpoint during the shutdown timeout.
func closeStreamsOnShutdown(ctx context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/stream") {
			next.ServeHTTP(w, r)
			return
		}
		streamCtx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		next.ServeHTTP(w, r.WithContext(streamCtx))
	})
}

// requireEnv refuses to serve a channel whose verification credentials are
// missing. The names are the fix: set the variables and start again.
func requireEnv(channel string, names ...string) error {
	for _, n := range names {
		if os.Getenv(n) == "" {
			return fmt.Errorf("bonnie: the %s channel needs %s in the environment; a webhook that does not verify its caller is a door with no lock", channel, n)
		}
	}
	return nil
}

// displayPath is a channel's webhook path for the banner.
func displayPath(p, def string) string {
	if p == "" {
		return def
	}
	return p
}

// mount puts a channel's routes on the serve mux, with the channel itself
// as the inbound side. Every BONNIE adapter is both: the webhook is its
// route, and the address map is its inbound surface.
func mount(mux *http.ServeMux, ch interface {
	channel.Channel
	channel.Inbound
}) {
	for _, rt := range ch.Routes() {
		handler := rt.Handler
		mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(w, r, ch)
		})
	}
}

// agentFactory builds the agent factory the server runs, with or without a
// tool sandbox.
//
// The default is "none", which runs Kit's core tools in the BONNIE process.
// That is the right default for a local developer and the wrong one for a
// server reachable from outside, so the banner says which mode is active
// rather than leaving an operator to guess.
//
// A nil policy means no control was requested — allow-all, the default. A
// policy a backend cannot enforce is refused, never stored and ignored.
//
// workspace, when set, is the directory the agent's files live in. Each mode
// applies it its own way and they must never be mixed: without a sandbox the
// host's file tools are rebuilt with it as their working directory; with one
// it is the seed mirrored into [sandbox.Workspace]. Handing the host tools to
// a sandboxed agent would give the model a shell on this machine, so the
// option is added only on the branch that has no sandbox.
func agentFactory(kind, image string, policy *sandbox.NetworkPolicy, workspace string, opts []kit.Option) (runtime.AgentFactory, error) {
	if kind == "" || kind == "none" {
		if policy != nil {
			return nil, fmt.Errorf("a network policy needs a sandbox; pass --sandbox docker")
		}
		return runtime.KitAgent(append(opts, hostWorkspaceOptions(workspace)...)...), nil
	}

	provider, err := sandboxProvider(kind, image)
	if err != nil {
		return nil, err
	}
	if policy != nil {
		net, ok := provider.(sandbox.Networked)
		if !ok {
			return nil, fmt.Errorf("the %s sandbox cannot control the network", provider.Name())
		}
		if err := net.SetNetworkPolicy(*policy); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := provider.Available(ctx); err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "bonnie: tools run in the %s sandbox\n", provider.Name())
	// The manifest's workspace is a seed: its files are mirrored into every
	// sandbox before the model's first command, and an edit the model made is
	// never reverted on resume. Wrapping here is what makes the key real
	// rather than accepted and ignored.
	if workspace != "" {
		provider = sandbox.Seeded(provider, workspace)
	}
	return sandbox.Agent(provider, opts...), nil
}

// hostWorkspaceOptions returns the options that root the host's file tools
// in the workspace, or nothing when there is no workspace.
//
// Kit's default core tools take no working directory: the option is a
// [kit.ToolOption] and [kit.Options] has no field that forwards one. So the
// core set is rebuilt with the workdir applied and supplied through
// [kit.WithTools]. [kit.AllTools] is that same default set, so no tool is
// lost.
//
// It is called on the no-sandbox branch only, and that placement is the
// security property: Kit honours Options.Tools even when DisableCoreTools is
// set, and [sandbox.Agent] applies a caller's options after its own — so
// using these options on a sandboxed agent would hand the model host tools
// inside the sandbox. Guard test: TestSandboxedAgentGetsNoHostTools.
func hostWorkspaceOptions(workspace string) []kit.Option {
	if workspace == "" {
		return nil
	}
	return []kit.Option{kit.WithTools(kit.AllTools(kit.WithWorkDir(workspace))...)}
}

// sandboxProvider maps the flag to a backend.
//
// The image reaches every backend that can honour it, and a backend that
// cannot is refused rather than left to run its default in silence: the
// manifest says the key "overrides the backend's default image", so
// accepting it and ignoring it would be the same broken promise a backend
// makes when it swallows a network policy (docs/SPEC.md §8, invariant 13).
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		return sandbox.Select(ctx, sandbox.Microsandbox(msb...), sandbox.Docker(dkr...))

	default:
		return nil, fmt.Errorf("unknown sandbox %q: want none, docker, microsandbox, local, or auto", kind)
	}
}

// manifestModel reads the model from a manifest, tolerating no manifest.
func manifestModel(m *agent.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Model
}

// manifestSandbox reads the sandbox config, tolerating no manifest and no
// sandbox key.
func manifestSandbox(m *agent.Manifest) agent.SandboxConfig {
	if m == nil || m.Sandbox == nil {
		return agent.SandboxConfig{}
	}
	return *m.Sandbox
}

// manifestChannelAddr reads the HTTP channel binding, tolerating no
// manifest.
func manifestChannelAddr(m *agent.Manifest) string {
	if m == nil || m.Channels == nil || m.Channels.HTTP == nil {
		return ""
	}
	return m.Channels.HTTP.Addr
}
