package shelltest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A wrapped ETXTBSY must be retried, but no other error may be hidden.
func TestWaitReady(t *testing.T) {
	t.Parallel()
	busy := &os.PathError{Op: "fork/exec", Path: "fixture", Err: syscall.ETXTBSY}
	exitErr := &exec.ExitError{}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"permission", syscall.EACCES},
		{"missing", os.ErrNotExist},
		{"exit", exitErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			calls := 0
			err := waitReady(ctx, func() error {
				calls++
				if calls < 3 {
					return busy
				}
				return tc.err
			})
			if err != tc.err || calls != 3 {
				t.Fatalf("got (%v, %d calls), want (%v, 3 calls)", err, calls, tc.err)
			}
		})
	}
}

// A busy inode must not make fixture setup wait without a limit.
func TestWaitReadyDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := waitReady(ctx, func() error { return syscall.ETXTBSY })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline", err)
	}
}

// An already canceled setup must not start another process.
func TestWaitReadyCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitReady(ctx, func() error {
		t.Fatal("probe called with canceled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

// Readiness must not run the body, even if it changes files or exits nonzero.
// Ordinary calls must keep their arguments, output, side effects, and status.
func TestWriteDoesNotRunBody(t *testing.T) {
	t.Parallel()
	if os.PathSeparator != '/' {
		t.Skip("shell fixture needs Unix")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture")
	marker := filepath.Join(dir, "marker")
	Write(t, path, "printf '%s\\n' \"$1\" >> \"$(dirname \"$0\")/marker\"\nprintf '%s\\n' \"$2\"\nexit 9\n")
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe ran body: stat marker: %v", err)
	}
	for range 2 {
		out, err := exec.Command(path, "inspect", "report").CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 9 || string(out) != "report\n" {
			t.Fatalf("body: output %q, error %v", out, err)
		}
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "inspect\ninspect\n" {
		t.Fatalf("body side effects = %q", body)
	}
}
