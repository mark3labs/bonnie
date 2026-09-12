package main

import (
	"context"
	"fmt"
	"os"
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
		RunE:  func(*cobra.Command, []string) error { return runSandboxPrune(o) },
	}
	f := cmd.Flags()
	f.StringVar(&o.journal, "journal", ".bonnie", "journal directory")
	f.StringVar(&o.kind, "sandbox", "docker", "sandbox backend the runs used: docker, microsandbox, msb, or local")
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

// runSandboxPrune reclaims the sandboxes of terminal runs.
//
// Without it a long-lived `bonnie serve` accumulates containers and
// microVMs until the disk fills: a finished run's conversation is durable,
// but nothing recorded that its sandbox existed, so nothing deleted it.
// The sandbox records T-012 added make this command possible — it walks the
// journal, finds terminal runs, and asks the provider to delete each one's
// sandbox without opening it, because opening would create.
//
// This is the cheap answer. The complete one is a background sweep in
// `serve`; a command an operator can run from cron is a deliberate first
// step, and it is the same code either way.
func runSandboxPrune(o pruneOpts) error {
	provider, err := sandboxProvider(o.kind, o.image)
	if err != nil {
		return err
	}
	reaper, ok := provider.(sandbox.RunDeleter)
	if !ok {
		return fmt.Errorf("the %s backend cannot delete a run's sandbox without opening it", provider.Name())
	}

	journal, err := runtime.OpenFileJournal(o.journal)
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
	for _, runID := range runIDs {
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
		existed, err := reaper.DeleteRun(ctx, runID)
		if err != nil {
			return fmt.Errorf("bonnie: delete sandbox of %s: %w", runID, err)
		}
		if existed {
			reaped++
			_, _ = fmt.Fprintf(tw, "%s\t%s\tdeleted sandbox\n", runID, state)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\tno sandbox (already gone, or never opened)\n", runID, state)
		}
	}

	if o.dryRun {
		fmt.Fprintln(os.Stderr, "dry run: nothing was deleted")
	} else {
		fmt.Fprintf(os.Stderr, "deleted %d sandbox(es)\n", reaped)
	}
	return nil
}
