//go:build !unix

package sandbox

import "os/exec"

func cancelJobProcess(c *exec.Cmd) error {
	if c.Process == nil {
		return nil
	}
	return c.Process.Kill()
}
