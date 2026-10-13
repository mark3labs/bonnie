package flaky

import (
	"testing"
)

// TestSkipHonoursEnvVar checks both directions: unset skips, set runs. A Skip
// that never ran a test would hide a fixed test forever; one that never
// skipped would leave the suite flaky.
func TestSkipHonoursEnvVar(t *testing.T) {
	t.Run("unset skips", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		ran := false
		t.Run("inner", func(t *testing.T) {
			Skip(t, "probe")
			ran = true
		})
		if ran {
			t.Fatal("Skip did not skip with the variable unset")
		}
	})
	t.Run("set runs", func(t *testing.T) {
		t.Setenv(EnvVar, "1")
		ran := false
		t.Run("inner", func(t *testing.T) {
			Skip(t, "probe")
			ran = true
		})
		if !ran {
			t.Fatal("Skip skipped with the variable set")
		}
	})
}
