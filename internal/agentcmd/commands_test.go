package agentcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/runtime"
)

func executeCommand(ctx context.Context, cmd *cobra.Command, w io.Writer, args ...string) error {
	cmd.SetOut(w)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

// Shared run commands must use the supplied journal, context, and writer.
func TestRunsCommandOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	journal, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Checkpoint(t.Context(), "run-1", runtime.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"show", "run-1"}, {"show", "run-1", "--json"}} {
		var out bytes.Buffer
		if err := executeCommand(t.Context(), NewRunsCommand(dir), &out, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "run-1") {
			t.Fatalf("%v output: %q", args, out.String())
		}
	}
}

func TestRunsCommandCancellation(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"list"}, {"show", "run-1"}} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := executeCommand(ctx, NewRunsCommand(t.TempDir()), io.Discard, args...)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v error: %v", args, err)
		}
	}
}

var errWrite = errors.New("writer failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

// Output errors must reach the caller in all output modes, including empty lists.
func TestRunsCommandWriterError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := executeCommand(t.Context(), NewRunsCommand(dir), failingWriter{}, "list"); !errors.Is(err, errWrite) {
		t.Fatalf("empty list error: %v", err)
	}
	journal, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Checkpoint(t.Context(), "run-1", runtime.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"show", "run-1"}, {"show", "run-1", "--json"}} {
		if err := executeCommand(t.Context(), NewRunsCommand(dir), failingWriter{}, args...); !errors.Is(err, errWrite) {
			t.Fatalf("%v error: %v", args, err)
		}
	}
}

func TestRunsCommandDefaultJournal(t *testing.T) {
	t.Parallel()
	cmd := NewRunsCommand("custom-journal")
	for _, child := range cmd.Commands() {
		flag := child.Flags().Lookup("journal")
		if flag == nil || flag.DefValue != "custom-journal" {
			t.Fatalf("%s journal flag: %+v", child.Name(), flag)
		}
	}
}

func TestSchedulesCommandDefaultAddress(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]string{
		"":                        "http://127.0.0.1:8080",
		":9090":                   "http://127.0.0.1:9090",
		"localhost:9090":          "http://localhost:9090",
		"https://example.com/api": "https://example.com/api",
	} {
		cmd := NewSchedulesCommand(addr)
		for _, child := range cmd.Commands() {
			if got := child.Flags().Lookup("url").DefValue; got != want {
				t.Fatalf("%s default for %q: %q, want %q", child.Name(), addr, got, want)
			}
		}
	}
}

// A caller can use the supplied server address without a --url flag.
func TestSchedulesCommandOutput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bonnie/v1/schedules" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"schedules":[]}`)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := executeCommand(t.Context(), NewSchedulesCommand(srv.URL), &out, "list"); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"schedules":[]}` {
		t.Fatalf("output: %q", out.String())
	}
	if err := executeCommand(t.Context(), NewSchedulesCommand(srv.URL), failingWriter{}, "list"); !errors.Is(err, errWrite) {
		t.Fatalf("writer error: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := executeCommand(ctx, NewSchedulesCommand(srv.URL), io.Discard, "list"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error: %v", err)
	}
}
