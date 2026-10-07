package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// newSandboxCmd mounts `bonnie sandbox`.
func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Reclaim the sandboxes of finished runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newSandboxPruneCmd())
	return cmd
}

func newSandboxPruneCmd() *cobra.Command {
	var o pruneOpts
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete the sandboxes of runs that reached a terminal state",
		Long:  "Delete terminal-run sandboxes. Stop the server first; cleanup does not coordinate with another process.",
		RunE:  func(*cobra.Command, []string) error { return runSandboxPrune(o) },
	}
	f := cmd.Flags()
	addJournalFlag(f, &o.journal)
	f.StringVar(&o.kind, "sandbox", "docker", "sandbox backend the runs used: landlock, docker, microsandbox, msb, or local")
	f.StringVar(&o.image, "sandbox-image", "", "sandbox image, only needed to construct the backend")
	f.BoolVar(&o.dryRun, "dry-run", false, "report what would be deleted, delete nothing")
	return cmd
}

// pruneOpts carries the parsed flags of `bonnie sandbox prune`.
type pruneOpts struct {
	journal string
	kind    string
	image   string
	dryRun  bool
}

// runSandboxPrune deletes terminal-run sandboxes through the runtime cleanup
// API. The runtime records successful cleanup so a later pass skips it.
// Run this command only after the server that owns these runs has stopped:
// the runtime run lock does not exclude another process.
func runSandboxPrune(o pruneOpts) error {
	provider, err := sandboxProvider(context.Background(), o.kind, o.image)
	if err != nil {
		return err
	}
	// Use the same working-file storage root as serving with this journal directory.
	switch provider.Name() {
	case "landlock":
		provider = sandbox.Landlock(sandbox.WithLandlockRoot(filepath.Join(o.journal, "workspaces")))
	case "local":
		provider = sandbox.Local(sandbox.WithLocalRoot(filepath.Join(o.journal, "workspaces")))
	}
	if v, ok := provider.(sandbox.RunCleanupValidator); ok {
		if err := v.ValidateRunCleanup(); err != nil {
			return fmt.Errorf("bonnie: validate sandbox cleanup: %w", err)
		}
	}
	reaper, ok := provider.(sandbox.RunDeleter)
	if !ok {
		return fmt.Errorf("the %s backend cannot delete a run's sandbox without opening it", provider.Name())
	}

	journal, err := runtime.OpenSQLiteJournal(o.journal)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	runIDs, err := journal.Runs(ctx, "")
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	_, _ = fmt.Fprintln(tw, "RUN\tSTATE\tACTION")

	reaped := 0
	actions := make(map[string]string)
	var cleanupErr error
	if !o.dryRun {
		runner := runtime.NewRunner(journal, nil)
		// A positive retention enables every terminal state. One nanosecond
		// makes this an immediate operator-requested cleanup pass.
		policy := runtime.SandboxCleanupPolicy{
			CompletedAfter: time.Nanosecond,
			FailedAfter:    time.Nanosecond,
			CancelledAfter: time.Nanosecond,
			RetiredAfter:   time.Nanosecond,
		}
		cleanupErr = runner.CleanupSandboxes(ctx, policy, func(ctx context.Context, runID string) (bool, error) {
			existed, err := reaper.DeleteRun(ctx, runID)
			if err != nil {
				actions[runID] = "cleanup failed"
				return existed, err
			}
			if existed {
				reaped++
				actions[runID] = "deleted sandbox"
			} else {
				actions[runID] = "no sandbox (already gone, or never opened)"
			}
			return existed, nil
		})
	}
	for _, runID := range runIDs {
		// BONNIE's own bookkeeping runs are not agent runs: they hold the
		// address map, never a sandbox, and they stay out of operator
		// output.
		if runtime.IsReservedRun(runID) {
			continue
		}
		state, err := journal.State(ctx, runID)
		if err != nil {
			return fmt.Errorf("bonnie: state of %s: %w", runID, err)
		}
		if !state.IsTerminal() {
			_, _ = fmt.Fprintf(tw, "%s\t%s\tkept (run is not terminal)\n", runID, state)
			continue
		}
		if o.dryRun {
			_, _ = fmt.Fprintf(tw, "%s\t%s\twould delete sandbox\n", runID, state)
			continue
		}
		action := actions[runID]
		if action == "" {
			action = "kept (cleanup already recorded or not eligible)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", runID, state, action)
	}

	if o.dryRun {
		fmt.Fprintln(os.Stderr, "dry run: nothing was deleted")
	} else {
		fmt.Fprintf(os.Stderr, "deleted %d sandbox(es)\n", reaped)
	}
	return cleanupErr
}
