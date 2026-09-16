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

	// Load a .env before anything reads the environment: the provider key the
	// agent factory needs and the channel credentials resolved below both come
	// from os.Getenv, so the file has to fill the gaps first. An exported
	// variable still wins over the file.
	dotenv, err := loadDotenv()
	if err != nil {
		return err
	}

	prompt, err := c.systemPrompt()
	if err != nil {
		return err
	}
	workspace, err := c.workspaceDir()
	if err != nil {
		return err
	}
	skills, err := c.skillsDir()
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

	factory, err := c.agentFactory(ctx, workspace, c.kitOptions(prompt, skills))
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
	httpOpts := []bonniehttp.Option{bonniehttp.WithInfo(info)}
	if c.auth != nil {
		httpOpts = append(httpOpts, bonniehttp.WithAuthenticator(c.auth))
	}
	mount(mux, bonniehttp.New(runner, httpOpts...), out)
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

	c.banner(ln.Addr().String(), workspace, skills, dotenv)
	return c.serve(ctx, mux, ln)
}

// kitOptions is the Kit configuration one run resolved to, with prompt and
// skills already resolved from the tree by [config.systemPrompt] and
// [config.skillsDir].
//
// A host's own options come first, so a setting BONNIE resolved from the tree
// wins over the same setting passed through [WithKit]. It is a method rather
// than a block inside [Agent.Run] so a test can read the options the agent
// will really be built with, which is the only place the tree's data becomes
// Kit configuration.
func (c *config) kitOptions(prompt, skills string) []kit.Option {
	opts := append([]kit.Option{}, c.kitOpts...)
	if c.model != "" {
		opts = append(opts, kit.WithModel(c.model))
	}
	if prompt != "" {
		opts = append(opts, kit.WithSystemPrompt(prompt))
	}

	// The tree's skills are the agent's whole skill set.
	//
	// Naming the directory also turns Kit's auto-discovery off, which is half
	// the point: left to itself Kit loads ~/.agents/skills and the
	// .agents/skills under Options.SessionDir, so a served agent would inherit
	// whatever skills the operator keeps for their own editor, and a sandboxed
	// run — sandbox.Agent points SessionDir at the sandbox root — would take
	// instructions from a directory it merely sits beside. A tree with no
	// skills therefore says so, rather than leaving the question open.
	//
	// Kit reads the directory during construction, which is why the path has
	// to be resolved before the agent is built: the activate_skill tool is
	// registered only when at least one skill loaded, so a skill added to a
	// running Kit would reach the catalog with no tool to open it.
	opts = append(opts, func(o *kit.Options) {
		if skills == "" {
			// A host that configured skills itself through WithKit keeps
			// them: an absent directory in the tree is BONNIE having
			// nothing to say, not an instruction to drop what was asked for.
			if o.SkillsDir == "" && len(o.Skills) == 0 {
				o.NoSkills = true
			}
			return
		}
		// Options.Skills is an explicit list Kit consults BEFORE SkillsDir,
		// and NoSkills silences both. Clearing them is what keeps the tree's
		// own directory from being accepted and then quietly shadowed.
		o.SkillsDir, o.Skills, o.NoSkills = skills, nil, false
	})
	if extra := append(append([]kit.Tool{}, Registered().Tools...), c.tools...); len(extra) > 0 {
		opts = append(opts, kit.WithExtraTools(extra...))
	}
	return opts
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

// skillsDir is the directory Kit scans for the tree's skills, absolute, or
// "" when the tree has none. Empty is not an error: instructions.md is a
// tree's one required file, and a tree with no skill is the normal case.
//
// The precedence is the one the instructions file follows — the tree on disk
// first, the copy codegen embedded second — but the fallback has to land on
// disk, because Kit takes a path. A built binary on a bare host therefore
// writes the embedded skills beside its journal and points Kit at that.
func (c *config) skillsDir() (string, error) {
	if c.skillsPath == "" {
		return "", nil
	}
	abs, err := filepath.Abs(c.skillsPath)
	if err != nil {
		return "", fmt.Errorf("bonnie: skills path: %w", err)
	}
	if hasSkillFiles(abs) {
		return abs, nil
	}
	return unpackSkills(Registered().Skills, filepath.Join(c.journal, DefaultSkills))
}

// hasSkillFiles reports whether dir holds a file that is not the scaffold's
// .gitkeep marker. A directory holding only the marker is the fresh scaffold's
// empty slot, not a skill set, and must not win over an embedded copy.
func hasSkillFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == ".gitkeep" {
			continue
		}
		return true
	}
	return false
}

// unpackSkills writes the skills codegen embedded into dest and returns dest
// absolute, or "" when the binary embedded none. dest is BONNIE's own
// directory beside the journal, never the tree's skills/ — a built binary is
// the only caller, and it has no tree.
//
// Unlike the workspace seed this replaces what is there. A skill is authored
// data that only the tree can change: the model never writes one, so a copy
// left by an older binary is stale rather than precious, and keeping it would
// leave a deleted skill in the prompt for as long as the directory survives.
func unpackSkills(files fs.FS, dest string) (string, error) {
	if embedIsEmpty(files) {
		return "", nil // nothing embedded: a tree with no skills
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("bonnie: skills path: %w", err)
	}
	if err := os.RemoveAll(abs); err != nil {
		return "", fmt.Errorf("bonnie: unpack skills: %w", err)
	}
	err = fs.WalkDir(files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == "." || entry.IsDir() {
			return err
		}
		target := filepath.Join(abs, filepath.FromSlash(embedRel(path)))
		b, err := fs.ReadFile(files, path)
		if err != nil {
			return fmt.Errorf("bonnie: read embedded skill %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("bonnie: unpack skills: %w", err)
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return fmt.Errorf("bonnie: unpack skills: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return abs, nil
}

// agentFactory builds the factory the runner executes turns with.
//
// A host that supplied its own factory owns the agent outright, so every
// option that would have configured the one BONNIE builds is refused rather
// than ignored — the same rule a backend follows for a network policy it
// cannot enforce.
//
// **Every other run is sandboxed.** There is no host-tools mode: [WithSandbox]
// selects a backend, it does not enable one, and leaving it out selects
// [sandbox.Landlock] rather than the host. BONNIE used to default to running
// Kit's core tools in this process, rooted at the workspace with
// kit.WithWorkDir; a live agent walked out of that root with an absolute path
// and read its own journal, because a working directory is a base and not a
// jail.
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
// agent did exactly that and listed its own journal.
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
func seedFromEmbed(files fs.FS, dest string) error {
	if embedIsEmpty(files) {
		return nil // nothing embedded: a tree run from its own source
	}
	return fs.WalkDir(files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == "." || entry.IsDir() {
			return err
		}
		target := filepath.Join(dest, filepath.FromSlash(embedRel(path)))
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

// embedIsEmpty reports whether a codegen embed carries nothing.
//
// The zero [embed.FS] — the slot a generated file declares when the plan found
// no directory to embed — reads as an existing but EMPTY root, not as an
// error. Testing the error alone would therefore call it non-empty, and a
// caller that acts on that materialises an empty directory and hands it on as
// if it were a real one.
func embedIsEmpty(files fs.FS) bool {
	entries, err := fs.ReadDir(files, ".")
	return err != nil || len(entries) == 0
}

// embedRel is one embedded path with the embed's own root directory stripped.
//
// A generated embed binds one directory (//go:embed workspace, //go:embed
// skills), so every path it yields begins with that directory's name. Only
// that first element is stripped — not every leading directory — so a file in
// a subdirectory keeps its place, which is what a skill bundled as
// skills/<name>/SKILL.md depends on.
func embedRel(path string) string {
	_, rel, ok := strings.Cut(path, "/")
	if !ok {
		return path
	}
	return rel
}

// banner prints the effective configuration at startup, with addr the address
// the listener really bound. An operator must never have to guess what is
// running.
//
// There is no no-sandbox warning any more, because there is no unsandboxed
// run to warn about. The backend is always named instead: the warning existed
// to make a dangerous default visible, and naming the confinement in force is
// what replaces it.
func (c *config) banner(addr, workspace, skills string, dotenv bool) {
	if c.quiet {
		return
	}
	line := func(label, value string) {
		fmt.Fprintf(os.Stderr, "bonnie: %s %s\n", label, value)
	}
	if dotenv {
		line("env", "loaded "+DefaultDotenv)
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
	if skills != "" {
		line("skills", skills)
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
