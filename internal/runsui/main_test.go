package runsui

import (
	"os"
	"testing"
)

// TestMain unsets TMUX before any test runs. Inside tmux, selection.Copy
// also runs `tmux load-buffer`, and a test must not change the developer's
// tmux buffers. Unsetting it once here keeps the copy tests parallel.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("TMUX")
	os.Exit(m.Run())
}
