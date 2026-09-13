package main

import (
	"context"
	"fmt"
	"io"
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
)

// devOpts carries the parsed flags of `bonnie dev`.
type devOpts struct {
	dryRun   bool
	shutdown time.Duration
}

// newDevCmd mounts `bonnie dev`.
func newDevCmd() *cobra.Command {
	var o devOpts
	cmd := &cobra.Command{
		Use:   "dev [dir]",
		Short: "Run an agent tree with hot reload",
		Long: `Run the agent tree at dir locally with hot reload: watch the tree,
regenerate the tool wiring, rebuild, and gracefully restart the serving child.

The loop watches the manifest, instructions, skills/, workspace/, tools/**, and
go.mod and go.sum. A change rebuilds the wrapper and restarts the child with
SIGTERM, waiting out its drain (--shutdown-timeout) before the next starts. A
parked run keeps no compute and lives in the journal; a restarted child resumes
it, and a stream client reconnects with ?cursor=.

The cost is seconds per save on a small module — the honest price of Go — and
it buys a real artifact: the same binary bonnie build ships.

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

	// log carries the child's output. It is os.Stderr for a real dev run; a
	// test sets a buffer to read the child's listen address. env overrides
	// the environment child builds inherit, so a hermetic test can point a
	// temp go.work at the tree.
	log io.Writer
	env []string

	mu    sync.Mutex
	child *exec.Cmd
}

func newDevServer(root string, o devOpts) *devServer {
	return &devServer{
		root:     root,
		bin:      filepath.Join(root, ".bonnie", "dev-agent"),
		shutdown: o.shutdown,
		log:      os.Stderr,
	}
}

// run builds the child and serves it, restarting on tree changes, until ctx
// ends. A dry run is handled by the caller before this.
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
	d.mu.Unlock()
	if old != nil {
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
	cmd := exec.Command(d.bin)
	cmd.Dir = d.root
	cmd.Stdout = d.log
	cmd.Stderr = d.log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("bonnie: dev: start child: %w", err)
	}
	d.mu.Lock()
	d.child = cmd
	d.mu.Unlock()
	fmt.Fprintf(os.Stderr, "bonnie: dev: started child (pid %d)\n", cmd.Process.Pid)
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
	return d.run(ctx)
}
