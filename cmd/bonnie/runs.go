package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/mark3labs/bonnie/runtime"
)

// runRuns dispatches the runs subcommands. They read the journal directly, so
// they work against a stopped server — which is exactly when an operator needs
// them.
func runRuns(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("runs needs a subcommand: list or show")
	}
	switch args[0] {
	case "list":
		return runsList(args[1:])
	case "show":
		return runsShow(args[1:])
	default:
		return fmt.Errorf("unknown runs subcommand %q: want list or show", args[0])
	}
}

func runsList(args []string) error {
	fs := flag.NewFlagSet("runs list", flag.ContinueOnError)
	dir := fs.String("journal", ".bonnie", "journal directory")
	state := fs.String("state", "", "only list runs in this state")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return err
	}

	journal, err := runtime.OpenFileJournal(*dir)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	ctx := context.Background()
	ids, err := journal.Runs(ctx, runtime.RunState(*state))
	if err != nil {
		return err
	}

	type row struct {
		RunID string           `json:"run_id"`
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
		}
		rows = append(rows, r)
	}

	if *asJSON {
		return writeJSON(os.Stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Println("no runs")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RUN\tSTATE\tSTEPS\tLAST")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", r.RunID, r.State, r.Turns, oneLine(r.Last, 60))
	}
	return tw.Flush()
}

func runsShow(args []string) error {
	fs := flag.NewFlagSet("runs show", flag.ContinueOnError)
	dir := fs.String("journal", ".bonnie", "journal directory")
	asJSON := fs.Bool("json", false, "print the raw records as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("runs show needs exactly one run ID")
	}
	runID := fs.Arg(0)

	journal, err := runtime.OpenFileJournal(*dir)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	ctx := context.Background()
	recs, err := journal.Replay(ctx, runID)
	if err != nil {
		return err
	}
	if *asJSON {
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
