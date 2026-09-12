package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// execute runs a command with args. These are the same commands main runs
// through fang; the tests drive cobra directly and assert on plain output.
func execute(c *cobra.Command, args ...string) error {
	c.SetArgs(args)
	return c.Execute()
}

// seedJournal writes a run that looks like a real one: a question, a
// suspension, and the state to match.
func seedJournal(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	j, err := runtime.OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	ctx := context.Background()

	s := runtime.NewSession("run-1", j)
	if _, err := s.AppendMessage(kitUser("deploy the app")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := j.Append(ctx, runtime.Record{
		RunID: "run-1", Kind: runtime.RecordSuspend, Text: "Which region?",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := j.Checkpoint(ctx, "run-1", runtime.RunWaiting); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := j.Checkpoint(ctx, "run-2", runtime.RunCompleted); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// A reserved run holds BONNIE's own bookkeeping and must stay out of
	// operator listings.
	if _, err := j.Append(ctx, runtime.Record{
		RunID: runtime.ReservedRunPrefix + "addresses",
		Kind:  runtime.RecordExtensionData, ExtType: "channel.address", Text: "slack:C1",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dir
}

// capture runs fn with stdout redirected and returns what it printed.
func capture(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	runErr := fn()

	os.Stdout = orig
	_ = w.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	_ = r.Close()

	if runErr != nil {
		t.Fatalf("command: %v", runErr)
	}
	return sb.String()
}

func TestRunsList(t *testing.T) {
	dir := seedJournal(t)

	out := capture(t, func() error { return execute(newRunsListCmd(), "--journal", dir) })
	if !strings.Contains(out, "run-1") || !strings.Contains(out, "waiting") {
		t.Fatalf("list output:\n%s", out)
	}
	if !strings.Contains(out, "run-2") {
		t.Fatalf("list dropped a run:\n%s", out)
	}
	if strings.Contains(out, runtime.ReservedRunPrefix) {
		t.Fatalf("list showed a reserved run:\n%s", out)
	}
}

func TestRunsListFiltersByState(t *testing.T) {
	dir := seedJournal(t)

	out := capture(t, func() error {
		return execute(newRunsListCmd(), "--journal", dir, "--state", "waiting")
	})
	if !strings.Contains(out, "run-1") {
		t.Fatalf("filter dropped the waiting run:\n%s", out)
	}
	if strings.Contains(out, "run-2") {
		t.Fatalf("filter kept a completed run:\n%s", out)
	}
}

func TestRunsListJSON(t *testing.T) {
	dir := seedJournal(t)

	out := capture(t, func() error {
		return execute(newRunsListCmd(), "--journal", dir, "--json")
	})
	var rows []struct {
		RunID string `json:"run_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %s", len(rows), out)
	}
}

func TestRunsShowTimeline(t *testing.T) {
	dir := seedJournal(t)

	out := capture(t, func() error { return execute(newRunsShowCmd(), "--journal", dir, "run-1") })
	for _, want := range []string{"run-1", "waiting", "message", "suspend", "deploy the app"} {
		if !strings.Contains(out, want) {
			t.Fatalf("timeline is missing %q:\n%s", want, out)
		}
	}
}

func TestRunsShowJSON(t *testing.T) {
	dir := seedJournal(t)

	out := capture(t, func() error {
		return execute(newRunsShowCmd(), "--journal", dir, "--json", "run-1")
	})
	var recs []runtime.Record
	if err := json.Unmarshal([]byte(out), &recs); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(recs) == 0 {
		t.Fatal("no records in the JSON output")
	}
	// The payload must survive the trip, or --json is useless for debugging a
	// replay problem.
	var sawPayload bool
	for _, rec := range recs {
		if rec.Kind == runtime.RecordMessage && len(rec.Payload) > 0 {
			sawPayload = true
		}
	}
	if !sawPayload {
		t.Fatal("message records lost their typed payload")
	}
}

func TestRunsShowUnknownRun(t *testing.T) {
	dir := seedJournal(t)

	err := execute(newRunsShowCmd(), "--journal", dir, "nope")
	if err == nil {
		t.Fatal("want an error for an unknown run")
	}
}

func TestRunsRejectsUnknownSubcommand(t *testing.T) {
	if err := execute(newRunsCmd(), "frobnicate"); err == nil {
		t.Fatal("want an error for an unknown subcommand")
	}
	if err := execute(newRunsCmd()); err == nil {
		t.Fatal("want an error when no subcommand is given")
	}
}

// TestRootRejectsUnknownCommand keeps the top-level tree honest: a mistyped
// command is an error, not a silent no-op.
func TestRootRejectsUnknownCommand(t *testing.T) {
	if err := execute(newRootCmd(), "frobnicate"); err == nil {
		t.Fatal("want an error for an unknown command")
	}
}

// TestVersionCommandPrints pins the scriptable form. `--version` on the root
// is fang's rendering; this subcommand is the one scripts can rely on.
func TestVersionCommandPrints(t *testing.T) {
	out := capture(t, func() error { return execute(newVersionCmd()) })
	if !strings.HasPrefix(out, "bonnie ") {
		t.Fatalf("version output: %q", out)
	}
}

// TestServeRejectsBadFlags keeps the flag set wired up without starting a
// listener.
func TestServeRejectsBadFlags(t *testing.T) {
	if err := execute(newServeCmd(), "--nope"); err == nil {
		t.Fatal("want an error for an unknown flag")
	}
}

// TestJournalDirIsCreated documents the zero-setup path: pointing at a
// directory that does not exist yet must work.
func TestJournalDirIsCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")

	out := capture(t, func() error { return execute(newRunsListCmd(), "--journal", dir) })
	if !strings.Contains(out, "no runs") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs")); err != nil {
		t.Fatalf("journal directory was not created: %v", err)
	}
}

// kitUser builds a user message without naming Kit's model layer twice.
func kitUser(text string) kit.LLMMessage { return kit.NewLLMUserMessage(text) }

// TestRunsShowDisplaysTheSandbox pins the T-012 timeline: a run that opened
// a sandbox shows which backend it used and which sandbox held its
// workspace, so an operator can find the compute behind a run.
func TestRunsShowDisplaysTheSandbox(t *testing.T) {
	dir := t.TempDir()
	j, err := runtime.OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	ctx := context.Background()
	s := runtime.NewSession("run-sbx", j)
	if _, err := s.AppendMessage(kitUser("deploy the app")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.RecordSandboxOpen(ctx, "docker", "bonnie-run-sbx"); err != nil {
		t.Fatalf("RecordSandboxOpen: %v", err)
	}
	if err := s.NoteSandboxUnavailable(ctx, "docker", "bonnie-run-sbx", true); err != nil {
		t.Fatalf("NoteSandboxUnavailable: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := capture(t, func() error { return execute(newRunsShowCmd(), "--journal", dir, "run-sbx") })
	for _, want := range []string{
		"backend docker",
		"bonnie-run-sbx",
		"sandbox opened",
		"sandbox gone",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("runs show output missing %q:\n%s", want, out)
		}
	}
}

// TestSandboxPruneReclaimsTerminalRuns covers the reconciler end to end with
// the local backend: a finished run's workspace is deleted, a waiting run's
// is kept, and --dry-run deletes nothing. It runs with t.Chdir, because the
// local provider's root is relative to the working directory, and a test
// that changes directory must not run in parallel.
func TestSandboxPruneReclaimsTerminalRuns(t *testing.T) {
	t.Chdir(t.TempDir())

	ctx := context.Background()
	j, err := runtime.OpenFileJournal(".bonnie")
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}

	// A finished run with a workspace, and a parked run with one.
	for _, runID := range []string{"done-run", "parked-run"} {
		s := runtime.NewSession(runID, j)
		if err := s.RecordSandboxOpen(ctx, "local", runID); err != nil {
			t.Fatalf("RecordSandboxOpen: %v", err)
		}
	}
	if err := j.Checkpoint(ctx, "done-run", runtime.RunCompleted); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := j.Checkpoint(ctx, "parked-run", runtime.RunWaiting); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	doneDir := filepath.Join(".bonnie", "workspaces", "done-run")
	parkedDir := filepath.Join(".bonnie", "workspaces", "parked-run")
	for _, d := range []string{doneDir, parkedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
	}

	// Dry run: nothing is deleted.
	capture(t, func() error {
		return execute(newSandboxPruneCmd(), "--journal", ".bonnie", "--sandbox", "local", "--dry-run")
	})
	if _, err := os.Stat(doneDir); err != nil {
		t.Fatal("dry run deleted a workspace")
	}

	// Real run: the terminal run's workspace goes, the parked one stays.
	out := capture(t, func() error {
		return execute(newSandboxPruneCmd(), "--journal", ".bonnie", "--sandbox", "local")
	})
	if _, err := os.Stat(doneDir); !os.IsNotExist(err) {
		t.Fatalf("the terminal run's workspace survived prune: %v", err)
	}
	if _, err := os.Stat(parkedDir); err != nil {
		t.Fatalf("prune deleted the workspace of a run that is not terminal: %v", err)
	}
	if !strings.Contains(out, "deleted sandbox") || !strings.Contains(out, "kept") {
		t.Fatalf("prune output:\n%s", out)
	}
}
