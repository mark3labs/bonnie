package sandbox

import (
	"testing"
	"time"
)

// TestDockerStopIsPrompt guards the container entrypoint (idleScript).
//
// The entrypoint shell is PID 1, and PID 1 ignores a signal it has no handler
// for. Before the trap, `docker stop` waited out its 10 s grace period and
// then killed the container, so each parked run blocked for 10 s, and this
// suite's reopen case was its slowest by far. A bound far below 10 s catches
// the regression without flaking on a slow daemon.
func TestDockerStopIsPrompt(t *testing.T) {
	t.Parallel()
	p, err := sharedDocker()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	sb := openSandbox(t, p, "stop-prompt")
	ctx := testCtx(t)

	start := time.Now()
	if err := sb.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Stop took %v: the entrypoint ignores SIGTERM, so docker "+
			"waits its grace period before it kills the container", took)
	}
}
