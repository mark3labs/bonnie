//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func protocolSandbox(t *testing.T, root string) *cliSandbox {
	t.Helper()
	return &cliSandbox{bin: "sh", jobsDir: root, execArgs: func(_ *cliSandbox, argv []string, _ Command) []string {
		return append([]string{"-c", `exec "$@"`, "test"}, argv...)
	}}
}

func awaitProtocol(t *testing.T, c JobSandbox, id string) *JobStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		st, err := c.JobStatus(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Running {
			return st
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Run the same scripts used by Docker and MSB as real guest processes.
func TestCLIJobProtocol(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := protocolSandbox(t, filepath.Join(dir, "jobs"))
	ctx := context.Background()
	id, err := c.StartJob(ctx, Command{Args: []string{"sh", "-c", `printf '%s\n' "$VALUE"; cat; printf err >&2; exit 7`}, Env: []string{"VALUE=quote ' and space"}, Stdin: []byte("input\n"), Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// A new handle reads guest files only, as after a BONNIE restart.
	st := awaitProtocol(t, protocolSandbox(t, c.jobsDir), id)
	if st.ExitCode == nil || *st.ExitCode != 7 {
		t.Fatalf("status: %+v", st)
	}
	out, err := os.ReadFile(st.StdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "quote ' and space\ninput\n" {
		t.Fatalf("stdout %q", out)
	}
}

func TestCLIJobCancelKeepsOtherJob(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := protocolSandbox(t, filepath.Join(dir, "jobs"))
	// Model shells whose kill builtin rejects the non-portable `--` option.
	// Keep the command in this shell so the function intercepts the cancel script.
	c.execArgs = func(_ *cliSandbox, argv []string, _ Command) []string {
		if len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], "kill -TERM") {
			wrapper := `kill() { for arg do [ "$arg" != -- ] || return 2; done; command kill "$@"; }; eval "$1"`
			return []string{"-c", `exec "$@"`, "test", "sh", "-c", wrapper, "test", argv[2]}
		}
		return append([]string{"-c", `exec "$@"`, "test"}, argv...)
	}
	ctx := context.Background()
	id, err := c.StartJob(ctx, Command{Args: []string{"sh", "-c", "sleep 60 & wait"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.StartJob(ctx, Command{Args: []string{"sleep", "60"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CancelJob(ctx, id); _ = c.CancelJob(ctx, other) })
	if err := c.CancelJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	st, err := c.JobStatus(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running {
		t.Fatal("cancelled job still running")
	}
	st, err = c.JobStatus(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running {
		t.Fatal("unrelated job stopped")
	}
}
