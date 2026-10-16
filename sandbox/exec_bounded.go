package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

type boundedOutput struct {
	bytes.Buffer
	remaining int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		return 0, fmt.Errorf("bonnie: helper output limit exceeded")
	}
	b.remaining -= len(p)
	return b.Buffer.Write(p)
}
func runCLIBounded(ctx context.Context, limit int, stdin []byte, name string, args ...string) (string, string, int, error) {
	if limit == 0 {
		return runCLI(ctx, stdin, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	out := &boundedOutput{remaining: limit}
	errout := &boundedOutput{remaining: 4096}
	cmd.Stdout = out
	cmd.Stderr = errout
	configureProcess(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.String(), errout.String(), -1, ctx.Err()
	}
	if err == nil {
		return out.String(), errout.String(), 0, nil
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return out.String(), errout.String(), exit.ExitCode(), nil
	}
	return out.String(), errout.String(), -1, err
}
