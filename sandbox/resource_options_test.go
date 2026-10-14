//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/shelltest"
)

// Resource options must reach the CLI as separate flag/value arguments.
func TestSandboxResourceCreateOptions(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"docker", "msb"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bin, argsFile := filepath.Join(dir, backend), filepath.Join(dir, "args")
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = inspect ]; then\n if [ %s = docker ]; then echo 'Error: No such object:' >&2; else echo \"error: sandbox not found: $4\" >&2; fi\n exit 1\nfi\nprintf '%%s\\n' \"$@\" >> '%s'\n", backend, argsFile)
			shelltest.Write(t, bin, strings.TrimPrefix(script, "#!/bin/sh\n"))
			var p Provider
			var want []string
			if backend == "docker" {
				p = Docker(WithDockerBinary(bin), WithDockerUser("1000:1000"), WithDockerMemory("512m"))
				want = []string{"--user\n1000:1000\n", "--memory\n512m\n"}
			} else {
				p = Microsandbox(WithMicrosandboxBinary(bin), WithMicrosandboxMemory(512), WithMicrosandboxCPUs(2))
				want = []string{"--memory\n512\n", "--cpus\n2\n"}
			}
			sb, err := p.Open(testCtx(t), "resources")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := sb.Close(); err != nil {
					t.Error(err)
				}
			})
			data, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, flags := range want {
				if !strings.Contains(string(data), flags) {
					t.Errorf("arguments %q omit %q", data, flags)
				}
			}
		})
	}
}

// Adoption must reject either changed resource setting, but accept a match.
func TestDockerResourceOptionsRefuseChangedContainer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, user string
		memory     int64
		mismatch   bool
	}{
		{"match", "1000:1000", 512 * 1024 * 1024, false},
		{"user", "2000:2000", 512 * 1024 * 1024, true},
		{"memory", "1000:1000", 256 * 1024 * 1024, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			bin := filepath.Join(t.TempDir(), "docker")
			report := fmt.Sprintf(`{"Config":{"Image":"test-image","User":%q,"Labels":{"bonnie.run":"resources"}},"HostConfig":{"NetworkMode":"bridge","Memory":%d}}`, c.user, c.memory)
			script := "#!/bin/sh\nif [ \"$3\" = '{{.State.Status}}' ]; then echo running; else printf '%s\\n' '" + report + "'; fi\n"
			shelltest.Write(t, bin, strings.TrimPrefix(script, "#!/bin/sh\n"))
			p := Docker(WithDockerBinary(bin), WithDockerImage("test-image"), WithDockerUser("1000:1000"), WithDockerMemory("512m"))
			sb, err := p.Open(testCtx(t), "resources")
			if c.mismatch {
				if !errors.Is(err, ErrPolicyMismatch) {
					t.Fatalf("Open error = %v, want policy mismatch", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := sb.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
