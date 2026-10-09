// Package agentcmd provides commands shared by the CLI and agent binaries.
package agentcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/internal/inspect"
	"github.com/mark3labs/bonnie/runtime"
)

// NewRunsCommand makes commands that read the local journal.
// defaultJournal sets the default value of each --journal flag.
// These commands also work when the server is stopped.
func NewRunsCommand(defaultJournal string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List and inspect durable runs",
		Long: `List and inspect durable runs.

These read the journal directly, so they work against a stopped server —
which is exactly when an operator needs them.`,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("runs needs a subcommand: list, show, or inspect")
		},
	}
	cmd.AddCommand(newRunsListCmd(defaultJournal), newRunsShowCmd(defaultJournal), newRunsInspectCmd(defaultJournal))
	return cmd
}

// newRunsInspectCmd opens a read-only terminal inspector of the local journal.
func newRunsInspectCmd(defaultJournal string) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "inspect [run-id]",
		Short: "Inspect durable runs in an interactive terminal",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			journal, err := runtime.OpenSQLiteJournal(dir)
			if err != nil {
				return fmt.Errorf("bonnie: open inspector journal: %w", err)
			}
			defer func() {
				if closeErr := journal.Close(); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("bonnie: close inspector journal: %w", closeErr))
				}
			}()
			var runID string
			if len(args) > 0 {
				runID = args[0]
			}
			return inspect.Run(cmd.Context(), journal, runID)
		},
	}
	cmd.Flags().StringVar(&dir, "journal", defaultJournal, "journal directory")
	return cmd
}

func newRunsListCmd(defaultJournal string) *cobra.Command {
	var (
		dir    string
		state  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List durable runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runsList(cmd.Context(), cmd.OutOrStdout(), dir, state, asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&dir, "journal", defaultJournal, "journal directory")
	f.StringVar(&state, "state", "", "only list runs in this state")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

func runsList(ctx context.Context, w io.Writer, dir, state string, asJSON bool) (err error) {
	journal, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("bonnie: close journal: %w", closeErr))
		}
	}()

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
		return writeJSON(w, rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no runs")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "RUN\tTITLE\tSTATE\tSTEPS\tLAST"); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.RunID, oneLine(r.Title, 40), r.State, r.Turns, oneLine(r.Last, 60)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func newRunsShowCmd(defaultJournal string) *cobra.Command {
	var (
		dir    string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Print one run's timeline, or its raw records with --json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runsShow(cmd.Context(), cmd.OutOrStdout(), dir, asJSON, args[0])
		},
	}
	f := cmd.Flags()
	f.StringVar(&dir, "journal", defaultJournal, "journal directory")
	f.BoolVar(&asJSON, "json", false, "print the raw records as JSON")
	return cmd
}

func runsShow(ctx context.Context, w io.Writer, dir string, asJSON bool, runID string) (err error) {
	journal, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("bonnie: close journal: %w", closeErr))
		}
	}()

	recs, err := journal.Replay(ctx, runID)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(w, recs)
	}

	state, err := journal.State(ctx, runID)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "run %s  state=%s  records=%d\n\n", runID, state, len(recs)); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SEQ\tTIME\tKIND\tDETAIL"); err != nil {
		return err
	}
	for _, rec := range recs {
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n",
			rec.Seq,
			rec.Timestamp.Format("15:04:05"),
			rec.Kind,
			oneLine(detail(rec), 80),
		); err != nil {
			return err
		}
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
