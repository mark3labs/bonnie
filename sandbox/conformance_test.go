package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// backend is one adapter under test in the conformance suite.
//
// Add an adapter here and it inherits every case below. That is the cheapest
// way to keep a later backend honest: the contract lives in one place, and a
// new adapter either satisfies it or fails loudly.
type backend struct {
	name string
	// open returns a provider, or skips the test when the backend cannot
	// run on this machine.
	open func(t *testing.T) Provider
	// isolated is false for a backend that shares the host filesystem.
	isolated bool
}

func backends() []backend {
	return []backend{
		{
			name:     "local",
			isolated: false,
			open: func(t *testing.T) Provider {
				t.Helper()
				return Local(WithLocalRoot(t.TempDir()), WithLocalCleanup())
			},
		},
		{
			name:     "docker",
			isolated: true,
			open: func(t *testing.T) Provider {
				t.Helper()
				p := Docker()
				if err := p.Available(context.Background()); err != nil {
					t.Skipf("docker unavailable: %v", err)
				}
				return p
			},
		},
		{
			name:     "microsandbox",
			isolated: true,
			open: func(t *testing.T) Provider {
				t.Helper()
				p := Microsandbox()
				if err := p.Available(context.Background()); err != nil {
					t.Skipf("microsandbox unavailable: %v", err)
				}
				return p
			},
		},
	}
}

// eachBackend runs fn against every adapter, skipping those not usable here.
func eachBackend(t *testing.T, fn func(t *testing.T, b backend, p Provider)) {
	t.Helper()
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			p := b.open(t)
			fn(t, b, p)
		})
	}
}

// openSandbox opens a sandbox for a test and removes it afterwards.
//
// Cleanup matters: Close deliberately leaves a sandbox alive so a later Open
// can reattach, so a test that only closes leaks a container. Delete is the
// one that removes it.
func openSandbox(t *testing.T, p Provider, runID string) Sandbox {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sb, err := p.Open(ctx, runID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, c := context.WithTimeout(context.Background(), time.Minute)
		defer c()
		// Reopen: the test may have closed the handle, and a closed handle
		// cannot delete the sandbox it left running.
		fresh, err := p.Open(cleanupCtx, runID)
		if err != nil {
			_ = sb.Close()
			return
		}
		if d, ok := fresh.(Deleter); ok {
			_ = d.Delete(cleanupCtx)
			return
		}
		_ = fresh.Close()
	})
	return sb
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestExecReturnsOutput(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-output")
		ctx := testCtx(t)

		res, err := sb.Exec(ctx, Shell("echo hello sandbox"))
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !res.OK() {
			t.Fatalf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "hello sandbox") {
			t.Fatalf("stdout = %q", res.Stdout)
		}
	})
}

// TestNonZeroExitIsNotAnError is the contract an agent loop depends on. A
// failed build is information the model must act on, not a transport fault.
func TestNonZeroExitIsNotAnError(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-nonzero")
		ctx := testCtx(t)

		res, err := sb.Exec(ctx, Shell("echo to-stdout; echo to-stderr >&2; exit 42"))
		if err != nil {
			t.Fatalf("a non-zero exit must not be a Go error, got: %v", err)
		}
		if res.ExitCode != 42 {
			t.Fatalf("exit = %d, want 42", res.ExitCode)
		}
		if res.OK() {
			t.Fatal("OK() must be false for a non-zero exit")
		}
		if !strings.Contains(res.Stdout, "to-stdout") {
			t.Fatalf("stdout = %q", res.Stdout)
		}
		if !strings.Contains(res.Stderr, "to-stderr") {
			t.Fatalf("stderr = %q", res.Stderr)
		}
	})
}

// TestExitCodesRoundTrip walks the range, because a CLI backend that reports
// its own failure as the guest's code usually gets one specific value wrong.
func TestExitCodesRoundTrip(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-codes")
		ctx := testCtx(t)

		for _, want := range []int{0, 1, 2, 42, 126, 127} {
			res, err := sb.Exec(ctx, Shell(fmt.Sprintf("exit %d", want)))
			if err != nil {
				t.Fatalf("exit %d: %v", want, err)
			}
			if res.ExitCode != want {
				t.Fatalf("exit = %d, want %d", res.ExitCode, want)
			}
		}
	})
}

// TestEveryCallExecutes is the anti-caching contract. A backend that memoizes
// would return the first answer twice, and the model would never know a side
// effect did not happen. See docs/SANDBOX.md.
func TestEveryCallExecutes(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-nocache")
		ctx := testCtx(t)

		// Append to a file and read it back. A cached second call would
		// report one line; a real one reports two.
		const script = "echo tick >> /tmp/ledger && wc -l < /tmp/ledger"
		first, err := sb.Exec(ctx, Shell(script))
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		second, err := sb.Exec(ctx, Shell(script))
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}

		if strings.TrimSpace(first.Stdout) == strings.TrimSpace(second.Stdout) {
			t.Fatalf("both calls reported %q: the backend cached a tool call, "+
				"so a repeated side effect is invisible to the model",
				strings.TrimSpace(first.Stdout))
		}
	})
}

func TestFileRoundTrip(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "file-roundtrip")
		ctx := testCtx(t)

		const body = "line one\nline two\n"
		if err := sb.WriteFile(ctx, "notes/todo.txt", []byte(body)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := sb.ReadFile(ctx, "notes/todo.txt")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != body {
			t.Fatalf("read %q, want %q", got, body)
		}

		// A relative path and its absolute form must name one file.
		abs, err := sb.ReadFile(ctx, Workspace+"/notes/todo.txt")
		if err != nil {
			t.Fatalf("ReadFile(absolute): %v", err)
		}
		if string(abs) != body {
			t.Fatalf("absolute read %q, want %q", abs, body)
		}
	})
}

// TestBinaryFileRoundTrip guards the file path against the exit marker and any
// other text mangling.
func TestBinaryFileRoundTrip(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "file-binary")
		ctx := testCtx(t)

		payload := make([]byte, 1024)
		for i := range payload {
			payload[i] = byte(i % 256)
		}
		if err := sb.WriteFile(ctx, "blob.bin", payload); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := sb.ReadFile(ctx, "blob.bin")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("binary round trip corrupted %d bytes", len(payload))
		}
	})
}

func TestReadMissingFile(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "file-missing")
		ctx := testCtx(t)

		if _, err := sb.ReadFile(ctx, "does/not/exist.txt"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestWorkspaceIsTheWorkingDirectory pins the one namespace that lets a
// conversation move between backends and still find its files.
func TestWorkspaceIsTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, b backend, p Provider) {
		sb := openSandbox(t, p, "workspace-cwd")
		ctx := testCtx(t)

		if err := sb.WriteFile(ctx, "here.txt", []byte("x")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		res, err := sb.Exec(ctx, Shell("cat here.txt"))
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !res.OK() || !strings.Contains(res.Stdout, "x") {
			t.Fatalf("a relative path did not resolve from the workspace: %+v", res)
		}

		// An isolated backend reports the workspace as its cwd. The local
		// backend maps the workspace onto a host directory, so it does not.
		if b.isolated {
			pwd, err := sb.Exec(ctx, Shell("pwd"))
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if strings.TrimSpace(pwd.Stdout) != Workspace {
				t.Fatalf("pwd = %q, want %q", strings.TrimSpace(pwd.Stdout), Workspace)
			}
		}
	})
}

func TestFilesPersistAcrossCommands(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "persist-commands")
		ctx := testCtx(t)

		if _, err := sb.Exec(ctx, Shell("echo persisted > state.txt")); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		res, err := sb.Exec(ctx, Shell("cat state.txt"))
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(res.Stdout, "persisted") {
			t.Fatalf("a file written by one command was gone in the next: %+v", res)
		}
	})
}

// TestReopenKeepsWorkspace is the durability contract that matters to BONNIE:
// a run parks, the handle goes away, and the files are still there when it
// resumes.
func TestReopenKeepsWorkspace(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		ctx := testCtx(t)
		const runID = "reopen-workspace"

		sb := openSandbox(t, p, runID)
		if err := sb.WriteFile(ctx, "memo.txt", []byte("from the first turn")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		// Stop releases compute; the workspace must survive it.
		if err := sb.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		again, err := p.Open(ctx, runID)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		got, err := again.ReadFile(ctx, "memo.txt")
		if err != nil {
			t.Fatalf("ReadFile after reopen: %v", err)
		}
		if string(got) != "from the first turn" {
			t.Fatalf("after a stop and reopen the file said %q", got)
		}
	})
}

func TestSeparateRunsAreSeparateWorkspaces(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		ctx := testCtx(t)

		a := openSandbox(t, p, "isolation-a")
		b := openSandbox(t, p, "isolation-b")

		if err := a.WriteFile(ctx, "secret.txt", []byte("run a only")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := b.ReadFile(ctx, "secret.txt"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("run b could read run a's file: err = %v", err)
		}
	})
}

func TestExecWithEnvAndDir(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-env")
		ctx := testCtx(t)

		if _, err := sb.Exec(ctx, Shell("mkdir -p sub")); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		res, err := sb.Exec(ctx, Command{
			Args: []string{"sh", "-lc", "echo $GREETING"},
			Env:  []string{"GREETING=hello-env"},
			Dir:  "sub",
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(res.Stdout, "hello-env") {
			t.Fatalf("env not visible: %q", res.Stdout)
		}
	})
}

// TestArgvNeedsNoQuoting checks the marker wrapper does not break an argument
// that holds a space or a quote.
func TestArgvNeedsNoQuoting(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-quoting")
		ctx := testCtx(t)

		tricky := `a b "c" 'd' $e`
		res, err := sb.Exec(ctx, Command{Args: []string{"echo", tricky}})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if strings.TrimSpace(res.Stdout) != tricky {
			t.Fatalf("argument mangled: got %q, want %q", strings.TrimSpace(res.Stdout), tricky)
		}
	})
}

func TestExecStdin(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-stdin")
		ctx := testCtx(t)

		cmd := Shell("cat")
		cmd.Stdin = []byte("piped in")
		res, err := sb.Exec(ctx, cmd)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(res.Stdout, "piped in") {
			t.Fatalf("stdin not delivered: %q", res.Stdout)
		}
	})
}

func TestEmptyCommandIsRejected(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-empty")
		if _, err := sb.Exec(testCtx(t), Command{}); err == nil {
			t.Fatal("want an error for a command with no arguments")
		}
	})
}

func TestClosedSandboxRefusesWork(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		ctx := testCtx(t)
		// openSandbox registers the cleanup. Opening directly would leak a
		// container, because Close deliberately leaves the sandbox alive for
		// a later reattach.
		sb := openSandbox(t, p, "closed-sandbox")
		if err := sb.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := sb.Exec(ctx, Shell("echo hi")); !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	})
}

// TestConcurrentExecIsRaceClean matters because an agent can run tools in
// parallel.
func TestConcurrentExecIsRaceClean(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		sb := openSandbox(t, p, "exec-concurrent")
		ctx := testCtx(t)

		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() {
				res, err := sb.Exec(ctx, Shell(fmt.Sprintf("echo worker-%d", i)))
				if err != nil {
					t.Errorf("Exec: %v", err)
					return
				}
				if !strings.Contains(res.Stdout, fmt.Sprintf("worker-%d", i)) {
					t.Errorf("worker %d got %q", i, res.Stdout)
				}
			})
		}
		wg.Wait()
	})
}

func TestOpenTwiceGivesSameWorkspace(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		ctx := testCtx(t)
		const runID = "open-twice"

		first := openSandbox(t, p, runID)
		if err := first.WriteFile(ctx, "shared.txt", []byte("same place")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		second, err := p.Open(ctx, runID)
		if err != nil {
			t.Fatalf("second Open: %v", err)
		}
		got, err := second.ReadFile(ctx, "shared.txt")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "same place" {
			t.Fatalf("two Opens gave different workspaces: %q", got)
		}
	})
}
