package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/cmd/bonnie/tui"
)

// devOpts carries the parsed flags of `bonnie dev`.
type devOpts struct {
	dryRun   bool
	shutdown time.Duration
	tui      bool
	// addr is the explicit address to bind. Empty starts at :8080 and
	// walks 8081, 8082, … until one is free.
	addr string
}

// newDevCmd mounts `bonnie dev`.
func newDevCmd() *cobra.Command {
	var o devOpts
	cmd := &cobra.Command{
		Use:   "dev [dir]",
		Short: "Run an agent tree with hot reload and the built-in TUI",
		Long: `Run the agent tree at dir locally with hot reload and the built-in TUI: watch the
tree, regenerate the tool wiring, rebuild, and gracefully restart the serving child —
while you interact with the agent in a terminal.

The loop watches the manifest, instructions, skills/, workspace/, tools/**, and go.mod
and go.sum. A change rebuilds the wrapper and restarts the child with SIGTERM, waiting
out its drain (--shutdown-timeout) before the next starts. A parked run keeps no
compute and lives in the journal; a restarted child resumes it, and the TUI reconnects
to the stream (the journal is the durable record).

The TUI connects to the same HTTP channel the child serves, so the transcript you see
is what any client sees. --no-tui runs the serve loop alone for CI and non-interactive
hosts.

--dry-run prints the discovery plan without watching or building.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runDev(dir, o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.dryRun, "dry-run", false, "print the discovery plan without watching or building")
	f.BoolVar(&o.tui, "tui", true, "open the built-in terminal interface against the child")
	f.StringVar(&o.addr, "addr", "", "address to bind (empty = :8080, then :8081, …)")
	f.DurationVar(&o.shutdown, "shutdown-timeout", 30*time.Second, "how long to wait for in-flight turns on restart")
	return cmd
}

// devServer runs one dev loop. It builds the child binary, serves it, and
// restarts it on a tree change. The restart steps are separate methods so a
// test can drive a restart directly, crossing a process boundary without the
// timing of a file watch.
type devServer struct {
	root     string
	bin      string
	shutdown time.Duration

	// addr is the loopback address the child is told to bind. Empty means
	// walk 8080, 8081, 8082, … until one is free. An explicit --addr is
	// bound verbatim.
	addr string

	// served is the address the child bound. It is set once at first start
	// and reused on every restart, so the TUI stays connected.
	served string

	// log carries the child's output. It is os.Stderr for a real dev run.
	// env overrides the environment child builds inherit, so a hermetic test
	// can point a temporary go.work at the tree.
	log io.Writer
	env []string

	// beforeStop closes parent-owned connections to the child before a hot
	// reload. It is nil when dev runs without the TUI.
	beforeStop func()

	// ready closes after the first child process starts. The caller then
	// waits until that child accepts connections before it opens the TUI.
	ready     chan struct{}
	readyOnce sync.Once

	mu    sync.Mutex
	child *exec.Cmd
}

func newDevServer(root string, o devOpts) *devServer {
	return &devServer{
		root:     root,
		bin:      filepath.Join(root, ".bonnie", "dev-agent"),
		shutdown: o.shutdown,
		addr:     o.addr,
		log:      os.Stderr,
		ready:    make(chan struct{}),
	}
}

// pickListenAddr returns the first free loopback address. An explicit --addr
// is used as-is. Otherwise it tries :8080, then :8081, then :8082, and so on.
func pickListenAddr(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	for port := 8080; port < 8080+100; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = ln.Close()
		return addr, nil
	}
	return "", fmt.Errorf("bonnie: dev: no free port in 8080–8179")
}

// waitForListen dials addr until the child answers, or the context ends.
func waitForListen(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("bonnie: dev: child never listened on %s", addr)
}

// servedURL returns the address selected for the child and the TUI.
func (d *devServer) servedURL() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.served != "" {
		return d.served
	}
	return d.addr
}

func (d *devServer) run(ctx context.Context) error {
	if err := d.restart(); err != nil {
		return err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("bonnie: dev: watcher: %w", err)
	}
	defer func() { _ = w.Close() }()
	if err := watchTree(d.root, w); err != nil {
		d.stopChild()
		return err
	}

	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	for {
		select {
		case <-ctx.Done():
			d.stopChild()
			return nil
		case <-debounce.C:
			fmt.Fprintln(os.Stderr, "bonnie: dev: change detected, rebuilding")
			if err := d.restart(); err != nil {
				// A change that does not build must not take the serving
				// child down. Keep it running and keep watching.
				fmt.Fprintf(os.Stderr, "bonnie: dev: %v\n", err)
				continue
			}
		case ev, ok := <-w.Events:
			if !ok {
				d.stopChild()
				return nil
			}
			if !watched(ev.Name) {
				continue
			}
			debounce.Reset(300 * time.Millisecond)
		case err, ok := <-w.Errors:
			if !ok {
				d.stopChild()
				return nil
			}
			d.stopChild()
			return fmt.Errorf("bonnie: dev: watch: %w", err)
		}
	}
}

// restart regenerates the wiring, rebuilds the child, and re-forks it behind a
// graceful stop of the current child. This is what a tree change causes; a
// test calls it directly to cross a process boundary deterministically.
func (d *devServer) restart() error {
	content, plan, err := agent.Generate(d.root)
	if err != nil {
		return err
	}
	gen := filepath.Join(d.root, plan.OutFile)
	if err := os.WriteFile(gen, content, 0o644); err != nil {
		return fmt.Errorf("bonnie: dev: write %s: %w", plan.OutFile, err)
	}

	if err := d.build(); err != nil {
		return err
	}

	d.mu.Lock()
	old := d.child
	beforeStop := d.beforeStop
	d.mu.Unlock()
	if old != nil {
		if beforeStop != nil {
			beforeStop()
		}
		if err := stopGracefully(old, d.shutdown); err != nil {
			fmt.Fprintf(os.Stderr, "bonnie: dev: stopping previous child: %v\n", err)
		}
	}

	return d.start()
}

// build compiles the tree into the dev binary.
func (d *devServer) build() error {
	if err := os.MkdirAll(filepath.Dir(d.bin), 0o755); err != nil {
		return fmt.Errorf("bonnie: dev: journal dir: %w", err)
	}
	build := exec.Command("go", "build", "-o", d.bin, ".")
	build.Dir = d.root
	build.Env = d.env
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("bonnie: dev: go build: %w", err)
	}
	return nil
}

// start forks the child serving the tree. It returns after the child is
// started; the dev loop does not wait for it to exit.
func (d *devServer) start() error {
	// Reuse the address the first child bound so a hot reload keeps the TUI
	// connected. The first start walks 8080, 8081, … until one is free.
	d.mu.Lock()
	addr := d.served
	d.mu.Unlock()
	if addr == "" {
		var err error
		addr, err = pickListenAddr(d.addr)
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.served = addr
		d.mu.Unlock()
	}

	cmd := exec.Command(d.bin, "-addr", addr)
	cmd.Dir = d.root
	cmd.Stdout = d.log
	cmd.Stderr = d.log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("bonnie: dev: start child: %w", err)
	}
	fmt.Fprintf(os.Stderr, "bonnie: dev: started child (pid %d) on %s\n", cmd.Process.Pid, addr)
	d.mu.Lock()
	d.child = cmd
	d.mu.Unlock()
	d.readyOnce.Do(func() { close(d.ready) })
	return nil
}

// stopChild stops the current child, if any.
func (d *devServer) stopChild() {
	d.mu.Lock()
	child := d.child
	d.child = nil
	d.mu.Unlock()
	if child != nil {
		_ = stopGracefully(child, d.shutdown)
	}
}

// stopGracefully sends SIGTERM and waits out the drain, then kills. A turn that
// is cut short still keeps its finished steps — the journal is what survives.
func stopGracefully(cmd *exec.Cmd, shutdown time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(shutdown):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("child did not drain in %s; killed", shutdown)
	}
}

// watchTree adds every directory under root to the watcher, so a new file in a
// subdirectory is seen, not only edits to already-watched paths.
func watchTree(root string, w *fsnotify.Watcher) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		// The generated directory and the dev binary's home are the loop's
		// own output, not input; watching them causes a restart-forever loop.
		if entry.Name() == ".bonnie" || entry.Name() == ".git" {
			return filepath.SkipDir
		}
		return w.Add(path)
	})
}

// watched reports whether a changed path should trigger a rebuild. The
// generated file is the loop's own output, not its input — ignoring it prevents
// a restart-forever loop.
func watched(path string) bool {
	return filepath.Base(path) != "bonnie_gen.go"
}

// runDev is the bonnie dev entry, separated from cobra for testing.
func runDev(root string, o devOpts) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("bonnie: resolve dev dir: %w", err)
	}
	if o.dryRun {
		p, err := agent.Discover(root)
		if err != nil {
			return err
		}
		fmt.Print(p.String())
		return nil
	}
	d := newDevServer(root, o)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The serve loop owns the child and the journal; it runs in the
	// background. The TUI, when enabled, is the foreground: it talks to the
	// child over HTTP, so a hot reload of the child does not kill the
	// conversation — the journal is what survives.
	loopErr := make(chan error, 1)
	go func() { loopErr <- d.run(ctx) }()

	if o.tui {
		// Wait for the child to bind, then take over the foreground until
		// the user leaves the TUI.
		select {
		case <-d.ready:
		case <-ctx.Done():
			return <-loopErr
		}
		url := chatURL(d.servedURL())
		if err := waitForListen(ctx, d.servedURL()); err != nil {
			return err
		}
		client := tui.NewHTTP(url, nil)
		d.mu.Lock()
		d.beforeStop = client.CloseStreams
		d.mu.Unlock()
		if err := runTUIClient(ctx, client, tuiAddress(d.root)); err != nil {
			return err
		}
		// The TUI ended; stop the serve loop.
		stop()
		return <-loopErr
	}

	select {
	case err := <-loopErr:
		return err
	case <-ctx.Done():
		return nil
	}
}
