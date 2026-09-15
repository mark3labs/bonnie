//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"runtime"
)

// Landlock is a Linux kernel facility, so this backend cannot run here.
//
// The stub exists so the package still builds elsewhere — a developer on
// another platform can compile and run the tests for everything else. BONNIE
// itself targets Linux only.
func landlockSupported() error {
	return fmt.Errorf("%w: the landlock backend needs Linux, this is %s: use --sandbox docker",
		ErrUnavailable, runtime.GOOS)
}

// execJailed refuses rather than running the command unconfined. A backend
// that cannot enforce its promise must fail, never silently downgrade.
func (s *landlockSandbox) execJailed(context.Context, Command, string) (*Result, error) {
	return nil, landlockSupported()
}
