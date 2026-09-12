package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// runSandbox dispatches the sandbox subcommands.
func runSandbox(args []string) error {
	if len(args) == 0 {
		sandboxUsage()
		return nil
	}
	switch args[0] {
	case "prune":
		return runSandboxPrune(args[1:])
	case "help", "-h", "--help":
		sandboxUsage()
		return nil
	default:
		sandboxUsage()
		return fmt.Errorf("bonnie: unknown sandbox command %q", args[0])
	}
}

func sandboxUsage() {
	fmt.Fprint(os.Stderr, `usage: bonnie sandbox <command>

  prune   delete the sandboxes of runs that reached a terminal state

`)
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
func runSandboxPrune(args []string) error {
	fs := flag.NewFlagSet("sandbox prune", flag.ContinueOnError)
	dir := fs.String("journal", ".bonnie", "journal directory")
	kind := fs.String("sandbox", "docker", "sandbox backend the runs used: docker, microsandbox, msb, or local")
	image := fs.String("sandbox-image", "", "sandbox image, only needed to construct the backend")
	dryRun := fs.Bool("dry-run", false, "report what would be deleted, delete nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	provider, err := sandboxProvider(*kind, *image)
	if err != nil {
		return err
	}
	reaper, ok := provider.(sandbox.RunDeleter)
	if !ok {
		return fmt.Errorf("the %s backend cannot delete a run's sandbox without opening it", provider.Name())
	}

	journal, err := runtime.OpenFileJournal(*dir)
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
		if *dryRun {
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

	if *dryRun {
		fmt.Fprintln(os.Stderr, "dry run: nothing was deleted")
	} else {
		fmt.Fprintf(os.Stderr, "deleted %d sandbox(es)\n", reaped)
	}
	return nil
}
