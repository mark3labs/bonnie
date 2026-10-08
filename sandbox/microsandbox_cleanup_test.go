package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Only an exact missing-name diagnostic proves absence. A prefix match,
// extra error details, or an invalid report must not permit a cleanup receipt.
func TestMicrosandboxCleanupUncertainInspect(t *testing.T) {
	t.Parallel()
	for _, report := range []struct {
		name, output string
		code         int
	}{
		{"absent", "error: sandbox not found: bonnie-run", 1},
		{"other name", "error: sandbox not found: bonnie-run-other", 1},
		{"nested error", "error: lookup failed\n  → sandbox not found: bonnie-run", 1},
		{"extra error", "error: sandbox not found: bonnie-run\n  → database locked", 1},
		{"empty json", "{}", 0},
		{"invalid json", "not json", 0},
	} {
		t.Run(report.name, func(t *testing.T) {
			t.Parallel()
			bin := filepath.Join(t.TempDir(), "msb")
			script := "#!/bin/sh\nif [ \"$1\" != inspect ]; then exit 99; fi\n" + "printf '%s\\n' '" + report.output + "' >&2\nexit 1\n"
			if report.code == 0 {
				script = "#!/bin/sh\nprintf '%s\\n' '" + report.output + "'\n"
			}
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			p := Microsandbox(WithMicrosandboxBinary(bin))
			existed, err := p.DeleteRun(context.Background(), "run")
			if report.name == "absent" {
				if existed || err != nil {
					t.Fatalf("absent: %v, %v", existed, err)
				}
			} else if err == nil {
				t.Fatal("uncertain inspect allowed cleanup")
			}
		})
	}
}
