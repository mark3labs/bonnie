//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
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
	for _, tc := range []struct {
		name       string
		code       int
		missingDir bool
	}{
		{name: "nonzero", code: 7},
		{name: "zero", code: 0},
		{name: "workdir failure", code: 125, missingDir: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			work := dir
			if tc.missingDir {
				work = filepath.Join(dir, "missing")
			}
			id, err := c.StartJob(ctx, Command{Args: []string{"sh", "-c", `printf '%s\n' "$VALUE"; cat; printf err >&2; exit "$1"`, "test", fmt.Sprint(tc.code)}, Env: []string{"VALUE=quote ' and space"}, Stdin: []byte("input\n"), Dir: work})
			if err != nil {
				t.Fatal(err)
			}
			// A new handle reads guest files only, as after a BONNIE restart.
			st := awaitProtocol(t, protocolSandbox(t, c.jobsDir), id)
			if st.ExitCode == nil || *st.ExitCode != tc.code {
				t.Fatalf("status: %+v", st)
			}
			for path, want := range map[string]string{st.StdoutPath: "quote ' and space\ninput\n", st.StderrPath: "err"} {
				if tc.missingDir {
					want = ""
				}
				out, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(out) != want {
					t.Fatalf("%s: %q, want %q", path, out, want)
				}
			}
		})
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, err := c.StartJob(ctx, Command{Args: []string{"sh", "-c", "sleep 60 & wait"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.CancelJob(cleanupCtx, id)
	})
	other, err := c.StartJob(ctx, Command{Args: []string{"sleep", "60"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.CancelJob(cleanupCtx, other)
	})
	if err := c.CancelJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	st, err := c.JobStatus(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running || st.ExitCode == nil || *st.ExitCode != 143 {
		t.Fatalf("cancelled status: %+v", st)
	}
	st, err = c.JobStatus(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running {
		t.Fatal("unrelated job stopped")
	}
}

// Publish completion during the identity read, without sleeps or retries.
func TestCLIJobCompletionDuringIdentityRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	id := strings.Repeat("a", 32)
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"pid": "2147483647\n", "start": "1\n", "boot": string(boot)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := protocolSandbox(t, root)
	c.execArgs = func(_ *cliSandbox, argv []string, _ Command) []string {
		wrapper := `sed() { printf '7\n' >"$d/exit.tmp" && mv "$d/exit.tmp" "$d/exit"; command sed "$@"; }; eval "$1"`
		return []string{"-c", wrapper, "test", argv[2]}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := c.JobStatus(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != id || st.Running || st.ExitCode == nil || *st.ExitCode != 7 || st.StdoutPath != dir+"/stdout" || st.StderrPath != dir+"/stderr" {
		t.Fatalf("status: %+v", st)
	}
}

func TestCLIJobStatusFixtures(t *testing.T) {
	t.Parallel()
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		files   map[string]string
		wantErr error
		invalid bool
	}{
		{name: "missing", wantErr: ErrJobNotFound},
		{name: "missing identity", files: map[string]string{}, wantErr: ErrJobUnknown},
		{name: "dead identity", files: map[string]string{"pid": "2147483647\n", "boot": string(boot), "start": "1\n"}, wantErr: ErrJobUnknown},
		{name: "mismatched identity", files: map[string]string{"pid": "1\n", "boot": string(boot), "start": "not a start time\n"}, wantErr: ErrJobUnknown},
		{name: "mismatched boot", files: map[string]string{"pid": "1\n", "boot": "stale\n", "start": "1\n"}, wantErr: ErrJobUnknown},
		{name: "temporary record", files: map[string]string{"exit.tmp": "7\n"}, wantErr: ErrJobUnknown},
		{name: "completed stale identity", files: map[string]string{"pid": "2147483647\n", "boot": "stale\n", "exit": "7\n"}},
		{name: "empty completion", files: map[string]string{"exit": ""}, invalid: true},
		{name: "malformed completion", files: map[string]string{"exit": "not a code\n"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			id := strings.Repeat("b", 32)
			dir := filepath.Join(root, id)
			if tc.files != nil {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				for name, value := range tc.files {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st, err := protocolSandbox(t, root).JobStatus(ctx, id)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "invalid exit record") {
					t.Fatalf("status: %+v, error: %v", st, err)
				}
			} else if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error: %v, want %v", err, tc.wantErr)
				}
			} else if err != nil || st.Running || st.ExitCode == nil || *st.ExitCode != 7 {
				t.Fatalf("status: %+v, error: %v", st, err)
			}
		})
	}
}
