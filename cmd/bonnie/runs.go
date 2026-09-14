package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/runtime"
)

// newRunsCmd mounts `bonnie runs`. Its subcommands read the journal directly,
// so they work against a stopped server — which is exactly when an operator
// needs them.
func newRunsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List and inspect durable runs",
		Long: `List and inspect durable runs.

These read the journal directly, so they work against a stopped server —
which is exactly when an operator needs them.`,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("runs needs a subcommand: list or show")
		},
	}
	cmd.AddCommand(newRunsListCmd(), newRunsShowCmd())
	return cmd
}

func newRunsListCmd() *cobra.Command {
	var (
		dir    string
		state  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List durable runs",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runsList(dir, state, asJSON)
		},
	}
	f := cmd.Flags()
	addJournalFlag(f, &dir)
	f.StringVar(&state, "state", "", "only list runs in this state")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

func runsList(dir, state string, asJSON bool) error {
	journal, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	ctx := context.Background()
	ids, err := journal.Runs(ctx, runtime.RunState(state))
	if err != nil {
		return err
	}

	type row struct {
		RunID string           `json:"run_id"`
		Title string           `json:"title,omitempty"`
		State runtime.RunState `json:"state"`
		Turns int              `json:"turns"`
		Last  string           `json:"last,omitempty"`
	}

	rows := make([]row, 0, len(ids))
	for _, id := range ids {
		// Reserved runs hold BONNIE's own bookkeeping, not conversations.
		if runtime.IsReservedRun(id) {
			continue
		}
		recs, err := journal.Replay(ctx, id)
		if err != nil {
			return err
		}
		s, err := journal.State(ctx, id)
		if err != nil {
			return err
		}
		r := row{RunID: id, State: s}
		for _, rec := range recs {
			if rec.Kind == runtime.RecordStep {
				r.Turns++
			}
			if rec.Text != "" && (rec.Kind == runtime.RecordMessage || rec.Kind == runtime.RecordSuspend) {
				r.Last = rec.Text
			}
			if rec.Kind == runtime.RecordExtensionData && rec.ExtType == runtime.ExtTitle {
				r.Title = rec.Text
			}
		}
		rows = append(rows, r)
	}

	if asJSON {
		return writeJSON(os.Stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Println("no runs")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RUN\tTITLE\tSTATE\tSTEPS\tLAST")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.RunID, oneLine(r.Title, 40), r.State, r.Turns, oneLine(r.Last, 60))
	}
	return tw.Flush()
}

func newRunsShowCmd() *cobra.Command {
	var (
		dir    string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Print one run's timeline, or its raw records with --json",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runsShow(dir, asJSON, args[0])
		},
	}
	f := cmd.Flags()
	addJournalFlag(f, &dir)
	f.BoolVar(&asJSON, "json", false, "print the raw records as JSON")
	return cmd
}

func runsShow(dir string, asJSON bool, runID string) error {
	journal, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	ctx := context.Background()
	recs, err := journal.Replay(ctx, runID)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(os.Stdout, recs)
	}

	state, err := journal.State(ctx, runID)
	if err != nil {
		return err
	}
	fmt.Printf("run %s  state=%s  records=%d\n\n", runID, state, len(recs))

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SEQ\tTIME\tKIND\tDETAIL")
	for _, rec := range recs {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n",
			rec.Seq,
			rec.Timestamp.Format("15:04:05"),
			rec.Kind,
			oneLine(detail(rec), 80),
		)
	}
	return tw.Flush()
}

// detail renders the human-facing part of a record. Record.Text is a display
// projection of the payload, which is exactly what this needs.
func detail(rec runtime.Record) string {
	switch rec.Kind {
	case runtime.RecordState:
		return string(rec.State)
	case runtime.RecordSandbox:
		// The Text projection carries what the timeline needs: "sandbox
		// opened: backend docker, id bonnie-x". The payload behind it is
		// machine-readable in --json.
		return rec.Text
	case runtime.RecordMessage:
		if rec.Role != "" {
			return rec.Role + ": " + rec.Text
		}
		return rec.Text
	case runtime.RecordExtensionData:
		return rec.ExtType + ": " + rec.Text
	default:
		return rec.Text
	}
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
