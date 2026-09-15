package bonnie

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Agent is a configured agent: the tree's defaults with the options applied
// over them. Build one with [New], then [Agent.Serve] it.
//
// Nothing is opened, bound, or read until it serves, so building an agent
// cannot fail and [New] returns no error. A setting that cannot apply — a
// network policy with no sandbox to enforce it, a model beside a
// host-supplied agent factory — is refused when serving starts, which is the
// first moment the whole configuration is known.
type Agent struct {
	cfg *config
}

// New builds an agent from the tree's default layout and the options.
//
//	func main() { bonnie.New().Serve() }
//
// With no options that is a complete agent: instructions.md is the system
// prompt, workspace/ is the agent's root for files, .bonnie is the journal,
// the tools under tools/ are wired by codegen, and the HTTP channel is
// served on :8080. Each [Option] replaces one of those.
func New(opts ...Option) *Agent {
	c := defaults()
	for _, o := range opts {
		o(c)
	}
	return &Agent{cfg: c}
}

// Serve runs the agent until the process is interrupted, then exits.
//
// It owns the process, which is what makes a one-line main possible: it
// parses the operator flags a serving binary accepts, installs the signal
// handler, drains in-flight turns on SIGINT or SIGTERM, and exits non-zero
// after writing the error to stderr. A host that owns its own process calls
// [Agent.Run] instead.
//
// The flags are -addr and -model, and each wins over the matching option, so
// an operator can move a built binary to another port or model without
// rebuilding it. `bonnie dev` starts a tree's binary with -addr, which is the
// whole contract between the dev loop and the child.
func (a *Agent) Serve() {
	addr := flag.String("addr", "", "address to listen on")
	model := flag.String("model", "", "model to use, for example anthropic/claude-sonnet-4-5")
	flag.Parse()

	// The flags are applied after the author's options, so they win.
	if *addr != "" {
		WithAddr(*addr)(a.cfg)
	}
	if *model != "" {
		WithModel(*model)(a.cfg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Run serves the agent until ctx ends, then drains in-flight turns and
// returns. It is [Agent.Serve] without the process: no flags, no signal
// handler, no exit — for a host that already owns those.
//
// A run that parks holds no compute and lives in the journal, so stopping
// here is never destructive.
func (a *Agent) Run(ctx context.Context) error {
	c := a.cfg

	prompt, err := c.systemPrompt()
	if err != nil {
		return err
	}
	workspace, err := c.workspaceDir()
	if err != nil {
		return err
	}

	// A built binary has no tree beside it, so the workspace seed files come
	// from the copies codegen embedded. Seeding never overwrites: a file the
	// model already wrote is the agent's work, not the author's input.
	if workspace != "" {
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			return fmt.Errorf("bonnie: workspace: %w", err)
		}
		if err := seedFromEmbed(Registered().Workspace, workspace); err != nil {
			return err
		}
	}

	kitOpts := append([]kit.Option{}, c.kitOpts...)
	if c.model != "" {
		kitOpts = append(kitOpts, kit.WithModel(c.model))
	}
	if prompt != "" {
		kitOpts = append(kitOpts, kit.WithSystemPrompt(prompt))
	}
	if extra := append(append([]kit.Tool{}, Registered().Tools...), c.tools...); len(extra) > 0 {
		kitOpts = append(kitOpts, kit.WithExtraTools(extra...))
	}

	factory, err := c.agentFactory(ctx, workspace, kitOpts)
	if err != nil {
		return err
	}

	journal, err := runtime.OpenSQLiteJournal(c.journal)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	runner := runtime.NewRunner(journal, factory)

	// One mux carries every channel: the HTTP transport always, then
	// whatever an option added. The others are built first so the HTTP
	// channel's info route can name them, and each is refused a route in
	// the framework's namespace before anything is served.
	var channels []Channel
	for _, build := range c.channels {
		ch, err := build(runner)
		if err != nil {
			return err
		}
		if err := refuseReserved(ch); err != nil {
			return err
		}
		channels = append(channels, ch)
	}
	info := bonniehttp.Info{Agent: c.name, Channels: []string{"http"}}
	for _, ch := range channels {
		info.Channels = append(info.Channels, ch.Name())
	}

	// The outbound registry: the channels that implement [channel.Receiver]
	// answer a hand-off from any route handler.
	out := outbound{byName: make(map[string]channel.Receiver)}
	for _, ch := range channels {
		if r, ok := ch.(channel.Receiver); ok {
			out.byName[ch.Name()] = r
		}
	}

	mux := http.NewServeMux()
	mount(mux, bonniehttp.New(runner, bonniehttp.WithInfo(info)), out)
	for _, ch := range channels {
		mount(mux, ch, out)
	}

	// Bind before the banner, so the banner reports the address the process
	// actually listens on. A configured ":0" is a request for any free port,
	// and printing it back would tell an operator nothing.
	ln := c.listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", c.addr)
		if err != nil {
			return fmt.Errorf("bonnie: listen on %s: %w", c.addr, err)
		}
	}

	c.banner(ln.Addr().String(), workspace)
	return c.serve(ctx, mux, ln)
}

// systemPrompt resolves the agent's system prompt: the option when one was
// given, else the instructions file read fresh from disk, else the copy
// codegen embedded — which is what a built binary on a bare host has.
//
// A tree's instructions file is its one required file. When the path is set
// and neither the file nor an embedded copy exists, Run refuses: a silently
// prompt-less agent is a control nothing applied.
func (c *config) systemPrompt() (string, error) {
	if c.prompt != "" {
		return c.prompt, nil
	}
	if c.instrPath == "" {
		return "", nil
	}
	b, err := os.ReadFile(c.instrPath)
	if err == nil {
		return string(b), nil
	}
	if embedded := Registered().Instructions; embedded != "" {
		return embedded, nil
	}
	return "", fmt.Errorf("bonnie: the instructions file could not be read: %w", err)
}

// workspaceDir is the agent's root for files, absolute. Empty means the
// process's own directory stays the root, which is what a host with no tree
// asks for with WithWorkspace("").
func (c *config) workspaceDir() (string, error) {
	if c.workspace == "" {
		return "", nil
	}
	abs, err := filepath.Abs(c.workspace)
	if err != nil {
		return "", fmt.Errorf("bonnie: workspace path: %w", err)
	}
	return abs, nil
}

// agentFactory builds the factory the runner executes turns with.
//
// A host that supplied its own factory owns the agent outright, so every
// option that would have configured the one BONNIE builds is refused rather
// than ignored — the same rule a backend follows for a network policy it
// cannot enforce (docs/SPEC.md §8, invariant 13).
//
// **Every other run is sandboxed.** There is no host-tools mode: [WithSandbox]
// selects a backend, it does not enable one, and leaving it out selects
// [sandbox.Landlock] rather than the host. BONNIE used to default to running
// Kit's core tools in this process, rooted at the workspace with
// kit.WithWorkDir; a live agent walked out of that root with an absolute path
// and read its own journal, because a working directory is a base and not a
// jail. docs/SPEC.md §4.9.1 records the incident.
//
// workspace, when set, is the seed mirrored into [sandbox.Workspace] — never
// a working directory for host tools, which no longer exist.
func (c *config) agentFactory(ctx context.Context, workspace string, opts []kit.Option) (runtime.AgentFactory, error) {
	if c.factory != nil {
		if conflict := c.agentConflicts(); conflict != "" {
			return nil, fmt.Errorf("bonnie: WithAgentFactory owns the agent, so %s cannot apply: drop one of the two", conflict)
		}
		return c.factory, nil
	}

	provider := c.sandbox
	if provider == nil {
		provider = c.defaultSandbox()
	}
	if c.network != nil {
		net, ok := provider.(sandbox.Networked)
		if !ok {
			return nil, fmt.Errorf("bonnie: the %s sandbox cannot control the network: "+
				"pass bonnie.WithSandbox(sandbox.Docker()) or --sandbox docker, "+
				"which can", provider.Name())
		}
		if err := net.SetNetworkPolicy(*c.network); err != nil {
			return nil, err
		}
	}

	availCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := provider.Available(availCtx); err != nil {
		return nil, err
	}

	// The workspace is a seed here: its files are mirrored into every
	// sandbox before the model's first command, and an edit the model made
	// is never reverted on resume.
	if workspace != "" {
		provider = sandbox.Seeded(provider, workspace)
	}
	return sandbox.Agent(provider, opts...), nil
}

// agentConflicts names the option that cannot apply beside a host-supplied
// agent factory, or "" when there is none.
func (c *config) agentConflicts() string {
	switch {
	case c.model != "":
		return "WithModel"
	case c.prompt != "":
		return "WithSystemPrompt"
	case c.sandbox != nil:
		return "WithSandbox"
	case c.network != nil:
		return "WithNetwork"
	case len(c.tools) > 0:
		return "WithTools"
	case len(c.kitOpts) > 0:
		return "WithKit"
	}
	return ""
}

// hostWorkspaceOptions is gone, and its absence is the point.
//
// It rebuilt Kit's core tools with kit.WithWorkDir(workspace) so a run with no
// sandbox wrote into the workspace instead of the process's directory. That
// made the accident rarer without making the escape harder: WithWorkDir sets
// the base for a RELATIVE path, and the shell tool never resolves one — a
// model that writes an absolute path reaches the whole filesystem. A live
// agent did exactly that and listed its own journal (docs/SPEC.md §4.9.1).
//
// Every run is sandboxed now, so there are no host tools to root. Do not
// reintroduce this: Kit honours Options.Tools even when DisableCoreTools is
// set, and [sandbox.Agent] applies a caller's options after its own, so host
// tools reaching a sandboxed agent would hand the model a real host shell
// inside the sandbox. Guard test: TestNoHostToolsReachTheAgent.

// defaultSandbox is the backend a run gets when the host chose none.
//
// Landlock is the floor because it needs nothing installed: no daemon, no
// image, no KVM, no root. That matters more than it sounds. The alternative
// was to require Docker, and a floor that breaks `bonnie init && bonnie dev`
// on a bare machine is a floor people switch off — which is how the old
// no-sandbox default survived as long as it did.
//
// It confines the filesystem and the environment, NOT the network. See
// [sandbox.LandlockProvider] for the full statement of what it does not do.
//
// Workspaces live beside the journal, so one directory holds everything a run
// owns and `bonnie sandbox prune` has one place to look.
func (c *config) defaultSandbox() sandbox.Provider {
	return sandbox.Landlock(sandbox.WithLandlockRoot(filepath.Join(c.journal, "workspaces")))
}

// seedFromEmbed materialises the workspace files codegen embedded into dest.
// It never overwrites: a file that is already there is either the author's
// seed from a previous start or the model's own work, and both outrank a
// copy compiled in months ago.
//
// A generated embed binds one directory, so every path it yields begins with
// that directory's name. Only that first element is stripped — not every
// leading directory — so a seed file in a subdirectory keeps its place.
func seedFromEmbed(files fs.FS, dest string) error {
	if _, err := fs.ReadDir(files, "."); err != nil {
		return nil // nothing embedded: a tree run from its own source
	}
	return fs.WalkDir(files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == "." || entry.IsDir() {
			return err
		}
		_, rel, ok := strings.Cut(path, "/")
		if !ok {
			rel = path
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		b, err := fs.ReadFile(files, path)
		if err != nil {
			return fmt.Errorf("bonnie: read embedded seed %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("bonnie: seed workspace: %w", err)
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return fmt.Errorf("bonnie: seed workspace: %w", err)
		}
		return nil
	})
}

// banner prints the effective configuration at startup, with addr the address
// the listener really bound. An operator must never have to guess what is
// running.
//
// There is no no-sandbox warning any more, because there is no unsandboxed
// run to warn about. The backend is always named instead: the warning existed
// to make a dangerous default visible, and naming the confinement in force is
// what replaces it.
func (c *config) banner(addr, workspace string) {
	if c.quiet {
		return
	}
	line := func(label, value string) {
		fmt.Fprintf(os.Stderr, "bonnie: %s %s\n", label, value)
	}
	line("serving on", addr)
	line("journal", c.journal)
	if c.model != "" {
		line("model", c.model)
	}
	switch {
	case c.factory != nil:
		// A host-supplied factory owns the agent, so BONNIE cannot claim
		// anything about what its tools reach.
		line("agent", "supplied by the host")
	case c.sandbox != nil:
		line("sandbox", c.sandbox.Name())
	default:
		line("sandbox", c.defaultSandbox().Name()+" (default)")
	}
	line("network", networkLabel(c.network))
	if workspace != "" {
		line("workspace", workspace)
	}
}

// networkLabel is the effective network mode, for the banner.
func networkLabel(p *sandbox.NetworkPolicy) string {
	if p == nil {
		return "allow-all"
	}
	return string(p.Mode)
}

// serve runs the HTTP server on ln until ctx ends, then drains.
func (c *config) serve(ctx context.Context, mux http.Handler, ln net.Listener) error {
	srv := &http.Server{
		Handler:           closeStreamsOnShutdown(ctx, mux),
		ReadHeaderTimeout: 10 * time.Second,
		// A turn can wait on a model for minutes, and a stream waits for as
		// long as the client cares to listen, so neither gets a write
		// deadline.
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), c.shutdown)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("bonnie: shutdown: %w", err)
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

// mount puts a channel's routes on the mux, with the channel itself as the
// inbound side and the mounted channels as the outbound registry. Every
// BONNIE adapter is both: the webhook is its route, and the address map is
// its inbound surface.
func mount(mux *http.ServeMux, ch Channel, out channel.Outbound) {
	for _, rt := range ch.Routes() {
		handler := rt.Handler
		mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(w, r, ch, out)
		})
	}
}

// outbound is the [channel.Outbound] registry Run builds from the mounted
// channels.
type outbound struct{ byName map[string]channel.Receiver }

func (o outbound) To(name string) (channel.Receiver, bool) {
	r, ok := o.byName[name]
	return r, ok
}

// refuseReserved rejects a channel that wants a route in the framework's
// namespace. The error names the channel and the path, which a mux panic
// would not; and it is returned before the listener opens, so the operator
// reads it instead of a client.
func refuseReserved(ch Channel) error {
	for _, rt := range ch.Routes() {
		if strings.HasPrefix(rt.Path, channel.ReservedPathPrefix) {
			return fmt.Errorf("%w: channel %q mounts %s %s", channel.ErrReservedPath, ch.Name(), rt.Method, rt.Path)
		}
	}
	return nil
}
