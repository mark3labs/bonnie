package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Registry failures must fail preparation, not silently skip Docker tests.
// A public mirror can supply the same image under the test suite's local name.
func TestCIDockerImage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode           string
		wantPulls            int
		wantTag, wantFailure bool
	}{
		{name: "hub", mode: "hub", wantPulls: 1},
		{name: "retry", mode: "retry", wantPulls: 2},
		{name: "mirror", mode: "mirror", wantPulls: 4, wantTag: true},
		{name: "unavailable", mode: "unavailable", wantPulls: 6, wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			docker := `#!/bin/sh
printf '%s\n' "$*" >> "$CALLS"
case "$1" in
  pull)
    case "$MODE" in
      hub) exit 0 ;;
      retry) [ "$(grep -c '^pull ' "$CALLS")" -ge 2 ] ;;
      mirror) [ "$2" = mirror.gcr.io/library/alpine:3.19 ] ;;
      *) exit 1 ;;
    esac ;;
  *) exit 0 ;;
esac
`
			for name, body := range map[string]string{"docker": docker, "sleep": "#!/bin/sh\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "bash", "ci-docker-image.sh")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+log, "MODE="+tc.mode)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantFailure {
				t.Fatalf("preparation error = %v, output = %s", err, out)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(calls), "pull "); got != tc.wantPulls {
				t.Fatalf("got %d pulls, want %d: %s", got, tc.wantPulls, calls)
			}
			if got := strings.Contains(string(calls), "tag mirror.gcr.io/library/alpine:3.19 alpine:3.19"); got != tc.wantTag {
				t.Fatalf("mirror tag = %v, want %v: %s", got, tc.wantTag, calls)
			}
			if !tc.wantFailure && !strings.Contains(string(calls), "image inspect alpine:3.19") {
				t.Fatalf("prepared image not verified: %s", calls)
			}
		})
	}
}
