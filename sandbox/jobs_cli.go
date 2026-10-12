package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const guestJobsRoot = "/workspace/.bonnie/jobs"

func (c *cliSandbox) jobsRoot() string {
	if c.jobsDir != "" {
		return c.jobsDir
	}
	return guestJobsRoot
}

// StartJob starts a command in a detached guest session. Its streams and exit
// status stay in the guest job directory. Timeout is rejected because the
// detached guest process cannot be safely timed by the CLI request context.
func (c *cliSandbox) StartJob(ctx context.Context, cmd Command) (string, error) {
	if err := cmd.validate(); err != nil {
		return "", err
	}
	if cmd.Timeout > 0 {
		return "", errors.New("bonnie: sandbox: asynchronous job timeout is unsupported")
	}
	release, err := c.operation(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if err := c.ensure(ctx); err != nil {
		return "", err
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", fmt.Errorf("bonnie: sandbox: generate job ID: %w", err)
	}
	id := hex.EncodeToString(idBytes[:])
	dir := c.jobsRoot() + "/" + id
	script := jobStartScript(dir, cmd)
	_, stderr, code, err := runCLIRaw(ctx, nil, c.bin, c.execArgs(c, []string{"sh", "-c", script}, Command{})...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("bonnie: sandbox: start job: %s", firstLine(stderr))
	}
	return id, nil
}

func jobStartScript(dir string, cmd Command) string {
	// Pass environment and argv as distinct positional parameters. env's
	// assignments must precede its command; a separator is not an argv boundary.
	worker := `d=$1; work=$2; shift 2; trap 'printf "143\n" >"$d/exit"; exit 143' TERM; pid=$$; printf '%s\n' "$pid" >"$d/pid"; sed 's/^.*) //' "/proc/$pid/stat" | awk '{print $20}' >"$d/start"; cat /proc/sys/kernel/random/boot_id >"$d/boot"; touch "$d/ready"; cd "$work" || { echo 125 >"$d/exit"; exit 125; }; env -- "$@" <"$d/stdin" >>"$d/stdout" 2>>"$d/stderr"; rc=$?; printf '%s\n' "$rc" >"$d/exit"; exit "$rc"`
	parts := []string{"d=" + shellQuote(dir), "mkdir -p \"$d\" || exit 1", " : >\"$d/stdin\"", " : >\"$d/stdout\"", " : >\"$d/stderr\""}
	if len(cmd.Stdin) > 0 {
		parts = append(parts, "printf %s "+shellQuote(string(cmd.Stdin))+" >\"$d/stdin\"")
	}
	parts = append(parts, "nohup setsid sh -c "+shellQuote(worker)+" sh \"$d\" "+shellQuote(cmd.workdir())+jobEnvArgs(cmd.Env)+" "+jobArgv(cmd.Args)+" </dev/null >/dev/null 2>&1 &", "launcher=$!", "i=0; while [ ! -f \"$d/ready\" ] && [ $i -lt 50 ]; do sleep .02; i=$((i+1)); done", "[ -f \"$d/ready\" ] || { kill \"$launcher\" 2>/dev/null; exit 1; }")
	return strings.Join(parts, "\n")
}

func jobEnvArgs(env []string) string {
	var out strings.Builder
	for _, e := range env {
		out.WriteByte(' ')
		out.WriteString(shellQuote(e))
	}
	return out.String()
}
func jobArgv(args []string) string {
	var out strings.Builder
	for _, a := range args {
		out.WriteByte(' ')
		out.WriteString(shellQuote(a))
	}
	return out.String()
}

// JobStatus checks the guest completion record or verifies process identity.
func (c *cliSandbox) JobStatus(ctx context.Context, id string) (*JobStatus, error) {
	if err := validateJobID(id); err != nil {
		return nil, err
	}
	release, err := c.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	dir := c.jobsRoot() + "/" + id
	script := "d=" + shellQuote(dir) + "; if [ ! -d \"$d\" ]; then echo MISSING; elif [ -f \"$d/exit\" ]; then printf 'DONE '; cat \"$d/exit\"; elif [ -f \"$d/pid\" ] && p=$(cat \"$d/pid\") && [ \"$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)\" = \"$(cat \"$d/boot\" 2>/dev/null)\" ] && [ \"$(sed 's/^.*) //' /proc/$p/stat 2>/dev/null | awk '{print $20}')\" = \"$(cat \"$d/start\" 2>/dev/null)\" ]; then echo RUNNING; else echo UNKNOWN; fi"
	out, stderr, code, err := runCLIRaw(ctx, nil, c.bin, c.execArgs(c, []string{"sh", "-c", script}, Command{})...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("bonnie: sandbox: job status: %s", firstLine(stderr))
	}
	status := &JobStatus{ID: id, StdoutPath: dir + "/stdout", StderrPath: dir + "/stderr"}
	switch strings.TrimSpace(string(out)) {
	case "RUNNING":
		status.Running = true
	case "MISSING":
		return nil, ErrJobNotFound
	case "UNKNOWN":
		return nil, ErrJobUnknown
	default:
		value := strings.TrimSpace(strings.TrimPrefix(string(out), "DONE "))
		var exit int
		if _, err := fmt.Sscanf(value, "%d", &exit); err != nil {
			return nil, errors.New("bonnie: sandbox: job state unknown; invalid exit record")
		}
		status.ExitCode = &exit
	}
	return status, nil
}

// CancelJob signals only a process whose boot ID and start time match its record.
// It does not stop the sandbox or claim to contain detached child processes.
func (c *cliSandbox) CancelJob(ctx context.Context, id string) error {
	if err := validateJobID(id); err != nil {
		return err
	}
	release, err := c.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := c.ensure(ctx); err != nil {
		return err
	}
	dir := c.jobsRoot() + "/" + id
	script := "d=" + shellQuote(dir) + "; [ -d \"$d\" ] || exit 3; p=$(cat \"$d/pid\" 2>/dev/null) || exit 0; [ \"$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)\" = \"$(cat \"$d/boot\" 2>/dev/null)\" ] || exit 0; [ \"$(sed 's/^.*) //' /proc/$p/stat 2>/dev/null | awk '{print $20}')\" = \"$(cat \"$d/start\" 2>/dev/null)\" ] || exit 0; kill -TERM -- \"-$p\" 2>/dev/null || exit 0; i=0; while kill -0 \"$p\" 2>/dev/null && [ $i -lt 20 ]; do sleep .1; i=$((i+1)); done; if [ \"$(sed 's/^.*) //' /proc/$p/stat 2>/dev/null | awk '{print $20}')\" = \"$(cat \"$d/start\" 2>/dev/null)\" ]; then kill -KILL -- \"-$p\" 2>/dev/null || true; fi"
	_, stderr, code, err := runCLIRaw(ctx, nil, c.bin, c.execArgs(c, []string{"sh", "-c", script}, Command{})...)
	if err != nil {
		return err
	}
	if code == 3 {
		return ErrJobNotFound
	}
	if code != 0 {
		return fmt.Errorf("bonnie: sandbox: cancel job: %s", firstLine(stderr))
	}
	return nil
}
