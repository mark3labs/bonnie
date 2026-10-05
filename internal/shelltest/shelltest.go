// Package shelltest makes shell fixtures for tests. Production code must not
// use this package.
package shelltest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const probeArg = "--bonnie-test-fixture-ready"

// Write writes a shell fixture and waits until its inode can be executed.
// The reserved probe argument exits before body, without fixture side effects.
// A separate probe inode cannot check for write handles on the fixture inode.
func Write(t *testing.T, path, body string) {
	t.Helper()
	// A concurrent fork can inherit the writer until its child calls exec,
	// even with CLOEXEC (https://go.dev/issue/22315). Close, chmod, sync, and
	// rename do not release that inherited handle. Probe the same inode after
	// closing our writer; never retry execution of the fixture body.
	script := "#!/bin/sh\nif [ \"$1\" = \"" + probeArg + "\" ]; then exit 0; fi\n" + body
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("shelltest: write fixture: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("shelltest: chmod fixture: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitReady(ctx, func() error {
		cmd := exec.CommandContext(ctx, path, probeArg)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Wait()
	}); err != nil {
		t.Fatalf("shelltest: fixture %s not ready: %v", filepath.Base(path), err)
	}
}

// waitReady retries only the kernel's refusal to execute a busy inode. An
// exit error, or any other start error, is a fixture defect and must fail.
func waitReady(ctx context.Context, probe func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("probe deadline: %w", err)
		}
		err := probe()
		if !errors.Is(err, syscall.ETXTBSY) {
			return err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("probe still busy (%v): %w", err, ctx.Err())
		case <-timer.C:
		}
	}
}
