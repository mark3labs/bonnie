package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mark3labs/bonnie/internal/treetest"
)

// A failed initial build must return before a TUI waits for readiness.
func TestDevInitialFailureReturns(t *testing.T) {
	t.Parallel()
	done := make(chan error, 1)
	go func() { done <- runDev(t.TempDir(), devOpts{tui: true}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("missing module accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dev hung after initial failure")
	}
}

// Static builds must override inherited CGO settings without changing input.
func TestStaticBuildEnvironment(t *testing.T) {
	t.Parallel()
	env := []string{"PATH=/bin", "CGO_ENABLED=1", "CGO_ENABLED=1"}
	got := staticBuildEnv(env)
	if strings.Join(got, ";") != "PATH=/bin;CGO_ENABLED=0" {
		t.Fatalf("environment = %v", got)
	}
	if env[1] != "CGO_ENABLED=1" {
		t.Fatal("input changed")
	}
}

// New nested directories need their own watches to report later edits.
func TestWatchNewInputDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := watchTree(root, workspaceDir(root), w); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "tools", "new")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := watchTree(filepath.Join(root, "tools"), workspaceDir(root), w); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "tool.go")
	if err := os.WriteFile(file, []byte("package new"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-w.Events:
			if ev.Name == file {
				for _, output := range []string{".bonnie", ".git", "workspace"} {
					if watched(filepath.Join(root, output, "child"), workspaceDir(root)) {
						t.Fatalf("output %s watched", output)
					}
				}
				return
			}
		case err := <-w.Errors:
			t.Fatal(err)
		case <-deadline:
			t.Fatal("new nested file event not received")
		}
	}
}

// Cleanup closes parent streams before it stops and reaps the child.
func TestDevStopChildClosesStreams(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDevCleanupChild$", "--", "cleanup-child")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closed := false
	d := &devServer{child: cmd, shutdown: 5 * time.Second, beforeStop: func() { closed = true }}
	d.stopChild()
	if !closed || d.child != nil || cmd.ProcessState == nil {
		t.Fatalf("cleanup: streams closed=%v child=%v state=%v", closed, d.child, cmd.ProcessState)
	}
	d.stopChild()
}

// This test binary also supplies a child that waits for termination.
func TestDevCleanupChild(t *testing.T) {
	if slices.Contains(os.Args, "cleanup-child") {
		time.Sleep(time.Minute)
		return
	}
	t.Parallel()
}

// A directory removed before watch registration is not a loop failure.
func TestWatchRemovedDirectory(t *testing.T) {
	t.Parallel()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	root := t.TempDir()
	if err := watchTree(filepath.Join(root, "removed"), workspaceDir(root), w); err != nil {
		t.Fatal(err)
	}
}

// The local Go module cache is not an input to the dev build.
func TestWatchSkipsDirenv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cache := filepath.Join(root, ".direnv", "go")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := watchTree(root, workspaceDir(root), w); err != nil {
		t.Fatal(err)
	}
	for _, path := range w.WatchList() {
		if underDir(path, filepath.Join(root, ".direnv")) {
			t.Fatalf("cache watched: %s", path)
		}
	}
	if watched(filepath.Join(cache, "module.go"), workspaceDir(root)) {
		t.Fatal("cache event selected")
	}
}

// The loop must join child cleanup before it returns after cancellation.
func TestDevLoopCancellationReapsChild(t *testing.T) {
	t.Parallel()
	root := buildHermeticTree(t)
	d := newDevServer(root, devOpts{shutdown: 5 * time.Second})
	d.env = treetest.BuildEnv()
	var log syncBuffer
	d.log = &log
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.run(ctx) }()
	select {
	case <-d.ready:
	case err := <-done:
		t.Fatalf("loop ended before start: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("child did not start")
	}
	d.mu.Lock()
	child := d.child
	d.mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("loop did not finish cleanup")
	}
	if d.child != nil || child.ProcessState == nil {
		t.Fatalf("child not reaped: child=%v state=%v", d.child, child.ProcessState)
	}
}
