//go:build !unix

package sandbox

import (
	"os/exec"
	"time"
)

// configureProcess bounds pipe waits on platforms without process groups.
func configureProcess(c *exec.Cmd) { c.WaitDelay = time.Second }
