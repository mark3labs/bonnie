//go:build unix

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureProcess kills the process group on cancellation. This includes
// shell children that still hold the output pipes. A child that creates a new
// session is not contained by this mechanism; Local is not an isolation layer.
func configureProcess(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	c.WaitDelay = time.Second
}
