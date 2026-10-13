// Package flaky disables tests that fail intermittently, without deleting
// them.
//
// A flaky test hides real failures: a red run that "usually passes" teaches
// everyone to re-run instead of read. Skip takes such a test out of the normal
// run until its cause is fixed, and keeps it one variable away from running:
//
//	BONNIE_RUN_FLAKY=1 go test -race -count=20 ./... -run TestName
//
// Each call names the failure seen, so the person who fixes it knows what to
// look for. When a test is fixed, delete its Skip call.
//
// Only test code imports this package.
package flaky

import (
	"os"
	"testing"
)

// EnvVar is the environment variable that runs the disabled tests.
const EnvVar = "BONNIE_RUN_FLAKY"

// Skip skips t unless [EnvVar] is set to a non-empty value. The symptom
// describes the intermittent failure, as it appeared in the test output.
func Skip(t testing.TB, symptom string) {
	t.Helper()
	if os.Getenv(EnvVar) != "" {
		return
	}
	t.Skipf("flaky, disabled until fixed (set %s=1 to run): %s", EnvVar, symptom)
}
