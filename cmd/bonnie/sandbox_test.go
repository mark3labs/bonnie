package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/runtime"
)

// Prune must use the selected journal's workspace root and record cleanup.
// A second CLI pass shares only the journal and must not delete again.
func TestSandboxPruneUsesRuntimeCleanup(t *testing.T) {
	for _, backend := range []string{"landlock", "local"} {
		t.Run(backend, func(t *testing.T) {
			t.Chdir(t.TempDir())
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "journal")
			j, err := runtime.OpenSQLiteJournal(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = j.Close() }()
			states := map[string]runtime.RunState{
				"completed": runtime.RunCompleted,
				"failed":    runtime.RunFailed,
				"cancelled": runtime.RunCancelled,
				"retired":   runtime.RunRetired,
				"waiting":   runtime.RunWaiting,
			}
			for id, state := range states {
				if err := j.Checkpoint(ctx, id, state); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(root, "workspaces", id), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// The default root is unrelated to the selected journal.
			decoy := filepath.Join(".bonnie", "workspaces", "completed")
			if err := os.MkdirAll(decoy, 0o700); err != nil {
				t.Fatal(err)
			}
			capture(t, func() error {
				return execute(newSandboxPruneCmd(), "--journal", root, "--sandbox", backend, "--dry-run")
			})
			for id := range states {
				recs, err := j.Replay(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				for _, rec := range recs {
					if rec.Kind == runtime.RecordWorkspaceDeleted {
						t.Fatal("dry run recorded cleanup")
					}
				}
			}
			out := capture(t, func() error {
				return execute(newSandboxPruneCmd(), "--journal", root, "--sandbox", backend)
			})
			if strings.Count(out, "deleted sandbox") != 4 {
				t.Fatalf("cleanup output:\n%s", out)
			}
			for id, state := range states {
				_, err := os.Stat(filepath.Join(root, "workspaces", id))
				if state.IsTerminal() && !os.IsNotExist(err) {
					t.Fatalf("%s workspace survived: %v", id, err)
				}
				if !state.IsTerminal() && err != nil {
					t.Fatalf("%s workspace lost: %v", id, err)
				}
				got, err := j.State(ctx, id)
				if err != nil || got != state {
					t.Fatalf("%s state = %s, %v", id, got, err)
				}
				recs, err := j.Replay(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				deleted := 0
				for _, rec := range recs {
					if rec.Kind == runtime.RecordWorkspaceDeleted {
						deleted++
					}
				}
				want := 0
				if state.IsTerminal() {
					want = 1
				}
				if deleted != want {
					t.Fatalf("%s cleanup records = %d, want %d", id, deleted, want)
				}
			}
			if _, err := os.Stat(decoy); err != nil {
				t.Fatalf("prune touched the default root: %v", err)
			}
			// Recreate a directory to prove the receipt prevents a second delete.
			kept := filepath.Join(root, "workspaces", "completed")
			if err := os.MkdirAll(kept, 0o700); err != nil {
				t.Fatal(err)
			}
			out = capture(t, func() error {
				return execute(newSandboxPruneCmd(), "--journal", root, "--sandbox", backend)
			})
			if strings.Contains(out, "deleted sandbox") || !strings.Contains(out, "cleanup already recorded") {
				t.Fatalf("second cleanup output:\n%s", out)
			}
			if _, err := os.Stat(kept); err != nil {
				t.Fatalf("second pass deleted a recorded workspace: %v", err)
			}
		})
	}
}

func TestSandboxPruneHelpIncludesLandlock(t *testing.T) {
	t.Parallel()
	flag := newSandboxPruneCmd().Flags().Lookup("sandbox")
	if flag == nil || !strings.Contains(flag.Usage, "landlock") {
		t.Fatal("sandbox help does not list landlock")
	}
}
