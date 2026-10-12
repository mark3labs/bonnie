//go:build unix

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Process-group cancellation is best effort. A child can create a new session.
func cancelJobProcess(c *exec.Cmd) error {
	if c.Process == nil {
		return nil
	}
	err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
