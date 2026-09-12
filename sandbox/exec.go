package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// runCLI runs a host command and returns its streams. It is the bottom of
// every CLI-driven backend.
//
// The returned error is about the CLI itself failing to run. The command's own
// exit code comes back in the int, so a caller can tell the two apart.
func runCLI(ctx context.Context, stdin []byte, name string, args ...string) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err = cmd.Run()
	stdout, stderr = out.String(), errb.String()

	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout, stderr, 0, nil
	case errors.As(err, &ee):
		// The CLI ran and exited non-zero. That may be the guest command's
		// code or the CLI's own failure; the caller decides using the
		// exit marker.
		return stdout, stderr, ee.ExitCode(), nil
	default:
		return stdout, stderr, -1, fmt.Errorf("bonnie: sandbox: run %s: %w", name, err)
	}
}

// runCLIRaw runs a host command and returns stdout as bytes, unchanged.
//
// File reads use this instead of [runCLI] because the exit marker used by
// execWithMarker would corrupt binary content.
func runCLIRaw(ctx context.Context, stdin []byte, name string, args ...string) (stdout []byte, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err = cmd.Run()

	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.Bytes(), errb.String(), 0, nil
	case errors.As(err, &ee):
		return out.Bytes(), errb.String(), ee.ExitCode(), nil
	default:
		return nil, errb.String(), -1, fmt.Errorf("bonnie: sandbox: run %s: %w", name, err)
	}
}

// markerPrefix tags the line that carries the guest command's exit code.
const markerPrefix = "__bonnie_exit_"

// execWithMarker builds the argv that runs a guest command and reports its
// exit code in a way the host can trust.
//
// The problem it solves: `docker exec` and `msb exec` both exit with the
// guest command's code. So exit 42 is ambiguous — the command failed, or the
// CLI itself did. Getting that wrong means reporting "your build failed" when
// the truth is "the daemon is down", which sends the model off fixing code
// that was never broken.
//
// The fix is to have the guest print its own exit code on a marked line:
//
//	sh -lc '"$@"; printf "\n__bonnie_exit_<nonce>:%d\n" $?' bonnie prog arg1
//
// Passing the argv after the script means the shell quotes it, so no escaping
// is needed and an argument holding a quote or a space is safe. A per-call
// nonce means output that looks like a marker cannot be mistaken for one.
//
// No marker in the output means the CLI failed before the guest ran.
func execWithMarker(cmd Command) (argv []string, nonce string, err error) {
	if err := cmd.validate(); err != nil {
		return nil, "", err
	}
	nonce, err = randomNonce()
	if err != nil {
		return nil, "", err
	}

	script := fmt.Sprintf(`"$@"; printf "\n%s%s:%%d\n" $?`, markerPrefix, nonce)
	argv = append([]string{"sh", "-lc", script, "bonnie"}, cmd.Args...)
	return argv, nonce, nil
}

// parseMarker splits the guest's exit code off the end of stdout.
//
// found is false when the marker is absent, which means the guest command
// never ran.
func parseMarker(stdout, nonce string) (clean string, code int, found bool) {
	marker := markerPrefix + nonce + ":"
	idx := strings.LastIndex(stdout, marker)
	if idx < 0 {
		return stdout, 0, false
	}

	tail := stdout[idx+len(marker):]
	digits := strings.TrimSpace(tail)
	if cut := strings.IndexAny(digits, "\r\n"); cut >= 0 {
		digits = digits[:cut]
	}
	code, err := strconv.Atoi(strings.TrimSpace(digits))
	if err != nil {
		return stdout, 0, false
	}

	// Drop the marker and the newline that printf put in front of it.
	clean = strings.TrimSuffix(stdout[:idx], "\n")
	return clean, code, true
}

// randomNonce returns a short random tag for one exec.
func randomNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("bonnie: sandbox: read random: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// lookPath reports whether a binary is on PATH, wrapping the failure as
// [ErrUnavailable] so a caller can test it with errors.Is.
func lookPath(bin string) error {
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("%w: %s not found on PATH", ErrUnavailable, bin)
	}
	return nil
}

// envArgs renders Command.Env as repeated CLI flags, for example
// "--env" "KEY=value".
func envArgs(flag string, env []string) []string {
	out := make([]string, 0, len(env)*2)
	for _, e := range env {
		out = append(out, flag, e)
	}
	return out
}

// safeName turns a run ID into a container or sandbox name.
//
// The mapping must be injective. Replacing every unsafe character with a dash
// is not: "a.b", "a/b", and "a b" all become "a-b", so two runs would share
// one container and one run would see the other's files. A dot is a legal run
// ID character in the file journal, so this is reachable, not theoretical.
//
// When the mapping loses information, a digest of the original run ID is
// appended to make the name unique again.
func safeName(prefix, runID string) string {
	var b strings.Builder
	b.WriteString(prefix)

	lossy := false
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			// Docker and msb both accept these in a name.
			b.WriteRune(r)
		default:
			b.WriteByte('-')
			lossy = true
		}
	}

	name := b.String()
	if lossy || len(name) > maxNameLen {
		sum := sha256.Sum256([]byte(runID))
		digest := hex.EncodeToString(sum[:4])
		if len(name) > maxNameLen-len(digest)-1 {
			name = name[:maxNameLen-len(digest)-1]
		}
		name += "-" + digest
	}
	return name
}

// maxNameLen keeps a generated name inside the limits every backend accepts.
const maxNameLen = 60
