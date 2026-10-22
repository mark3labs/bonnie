// Package agentcmd provides commands shared by the CLI and agent binaries.
package agentcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/internal/runsui"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/wire"
)

// NewRunsCommand makes the commands that inspect and manage durable runs.
// defaultJournal sets the default --journal; defaultAddr sets the default
// --addr of the HTTP channel that actions go to.
//
// Reading needs only the journal, so it works against a stopped server —
// which is exactly when an operator needs it. Actions that change a run go
// over the HTTP channel, like every other client.
func NewRunsCommand(defaultJournal, defaultAddr string) *cobra.Command {
	var f consoleFlags
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "Inspect and manage durable runs",
		Long: `Inspect and manage durable runs.

Without a subcommand, runs opens the interactive console on a terminal:
runs, turns, the conversation, a trace waterfall of each turn, and the raw
journal, refreshed live. Without a terminal it prints the run list.

The console and the read commands read the journal directly, so they work
against a stopped server. Actions (answer, send, cancel, retire, clear,
compact) go to a running server's HTTP channel at --addr.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isTerminal(cmd) {
				return runsList(cmd.Context(), cmd.OutOrStdout(), f.journal, "", false)
			}
			return openConsole(cmd, f, "")
		},
	}
	f.register(cmd, defaultJournal, defaultAddr)
	cmd.AddCommand(
		newRunsListCmd(defaultJournal),
		newRunsShowCmd(defaultJournal),
		newRunsTraceCmd(defaultJournal),
		newRunsInspectCmd(defaultJournal, defaultAddr),
		newRunsAnswerCmd(defaultAddr),
		newRunsSendCmd(defaultAddr),
		newRunsActionCmd(defaultAddr, "cancel", "Cancel the turn in progress",
			"Stop the run's active or parked turn and drop its pending input.\nCompleted steps stay in the journal, so the run can be started again.",
			func(ctx context.Context, c *client.Client, id string) (string, error) {
				res, err := c.RequestCancel(ctx, id, "")
				return "cancel " + res.Status, err
			}),
		newRunsRetireCmd(defaultAddr),
		newRunsActionCmd(defaultAddr, "clear", "Drop the conversation from the model's context",
			"The journal keeps every record; the next message starts from an empty context.",
			func(ctx context.Context, c *client.Client, id string) (string, error) {
				return "context cleared", c.Clear(ctx, id)
			}),
		newRunsActionCmd(defaultAddr, "compact", "Summarise the run's older messages now",
			"A server whose agent cannot compact answers 501.",
			func(ctx context.Context, c *client.Client, id string) (string, error) {
				return "context compacted", c.Compact(ctx, id)
			}),
	)
	return cmd
}

// consoleFlags are the flags of the console: where to read, where to act.
type consoleFlags struct {
	journal, addr, token string
	readOnly             bool
}

func (f *consoleFlags) register(cmd *cobra.Command, defaultJournal, defaultAddr string) {
	fl := cmd.Flags()
	fl.StringVar(&f.journal, "journal", defaultJournal, "journal directory")
	fl.StringVar(&f.addr, "addr", defaultAddr, "HTTP channel address for actions")
	fl.StringVar(&f.token, "token", "", "bearer token for the HTTP channel")
	fl.BoolVar(&f.readOnly, "read-only", false, "do not connect to a server; disable actions")
}

func isTerminal(cmd *cobra.Command) bool {
	in, ok1 := cmd.InOrStdin().(*os.File)
	out, ok2 := cmd.OutOrStdout().(*os.File)
	return ok1 && ok2 && term.IsTerminal(in.Fd()) && term.IsTerminal(out.Fd())
}

func openConsole(cmd *cobra.Command, f consoleFlags, runID string) (err error) {
	journal, err := runtime.OpenSQLiteJournal(f.journal)
	if err != nil {
		return fmt.Errorf("bonnie: open console journal: %w", err)
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("bonnie: close console journal: %w", closeErr))
		}
	}()
	opts := runsui.Options{Journal: journal, JournalPath: journal.Path(), RunID: runID}
	if !f.readOnly {
		opts.Server = newClient(f.addr, f.token)
		opts.ServerAddr = ChatURL(f.addr)
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runsui.Open(ctx, opts)
}

func newClient(addr, token string) *client.Client {
	var opts []client.Option
	if token != "" {
		opts = append(opts, client.WithBearerToken(token))
	}
	return client.New(ChatURL(addr), opts...)
}

// newRunsInspectCmd opens the console with one run selected. It is the
// console's old name, kept so scripts and muscle memory still work.
func newRunsInspectCmd(defaultJournal, defaultAddr string) *cobra.Command {
	var f consoleFlags
	cmd := &cobra.Command{
		Use:   "inspect [run-id]",
		Short: "Open the interactive console, optionally on one run",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var runID string
			if len(args) > 0 {
				runID = args[0]
			}
			return openConsole(cmd, f, runID)
		},
	}
	f.register(cmd, defaultJournal, defaultAddr)
	return cmd
}

// withJournal opens the journal at dir for fn and closes it after.
func withJournal(dir string, fn func(*runtime.SQLiteJournal) error) (err error) {
	journal, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("bonnie: close journal: %w", closeErr))
		}
	}()
	return fn(journal)
}

// readRun replays and summarises one run.
func readRun(ctx context.Context, j *runtime.SQLiteJournal, id string) (runsui.Run, error) {
	recs, err := j.Replay(ctx, id)
	if err != nil {
		return runsui.Run{}, err
	}
	state, err := j.State(ctx, id)
	if err != nil {
		return runsui.Run{}, err
	}
	return runsui.Summarize(id, state, recs), nil
}

func newRunsListCmd(defaultJournal string) *cobra.Command {
	var (
		dir    string
		state  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List durable runs, most recently updated first",
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

func runsList(ctx context.Context, w io.Writer, dir, state string, asJSON bool) error {
	return withJournal(dir, func(journal *runtime.SQLiteJournal) error {
		ids, err := journal.Runs(ctx, runtime.RunState(state))
		if err != nil {
			return err
		}
		var runs []runsui.Run
		for _, id := range ids {
			// Reserved runs hold BONNIE's own bookkeeping, not conversations.
			if runtime.IsReservedRun(id) {
				continue
			}
			r, err := readRun(ctx, journal, id)
			if err != nil {
				return err
			}
			runs = append(runs, r)
		}
		runsui.SortRuns(runs, runsui.SortUpdated)

		type row struct {
			RunID   string           `json:"run_id"`
			Title   string           `json:"title,omitempty"`
			State   runtime.RunState `json:"state"`
			Turns   int              `json:"turns"`
			Steps   int              `json:"steps"`
			Origin  string           `json:"origin,omitempty"`
			Updated time.Time        `json:"updated,omitzero"`
			Waiting string           `json:"waiting,omitempty"`
			Last    string           `json:"last,omitempty"`
		}
		rows := make([]row, 0, len(runs))
		for _, r := range runs {
			waiting := ""
			if r.Suspend != nil {
				waiting = r.Suspend.Prompt
			}
			rows = append(rows, row{RunID: r.ID, Title: r.Title, State: r.State, Turns: len(r.Turns), Steps: r.Steps, Origin: r.Origin, Updated: r.Updated, Waiting: waiting, Last: r.Last})
		}

		if asJSON {
			return writeJSON(w, rows)
		}
		if len(rows) == 0 {
			_, err := fmt.Fprintln(w, "no runs")
			return err
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "RUN\tTITLE\tSTATE\tTURNS\tSTEPS\tUPDATED\tLAST"); err != nil {
			return err
		}
		now := time.Now()
		for _, r := range rows {
			last := r.Last
			if r.Waiting != "" {
				last = "? " + r.Waiting
			}
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", r.RunID, oneLine(r.Title, 40), r.State, r.Turns, r.Steps, since(r.Updated, now), oneLine(last, 60)); err != nil {
				return err
			}
		}
		return tw.Flush()
	})
}

// since is a compact age for the list table.
func since(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
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

func runsShow(ctx context.Context, w io.Writer, dir string, asJSON bool, runID string) error {
	return withJournal(dir, func(journal *runtime.SQLiteJournal) error {
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
		r := runsui.Summarize(runID, state, recs)
		head := fmt.Sprintf("run %s  state=%s  turns=%d  records=%d", runID, state, len(r.Turns), len(recs))
		if r.Title != "" {
			head += "  title=" + strconv.Quote(r.Title)
		}
		if _, err := fmt.Fprintf(w, "%s\n", head); err != nil {
			return err
		}
		if r.Suspend != nil {
			if _, err := fmt.Fprintf(w, "waiting: %s: %s\n", r.Suspend.Kind, r.Suspend.Prompt); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
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
	})
}

func newRunsTraceCmd(defaultJournal string) *cobra.Command {
	var (
		dir    string
		turn   int
		width  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "trace <run-id>",
		Short: "Print a turn's trace as a waterfall, rebuilt from the journal",
		Long: `Print a turn's trace as a waterfall, rebuilt from the journal.

The spans match the ones package observe exports — invoke_agent, chat per
step, execute_tool per call — but their times come from journal records, so
a trace is available without a tracing backend. --turn picks a turn by its
number; the default is the latest. --json prints the spans.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withJournal(dir, func(journal *runtime.SQLiteJournal) error {
				r, err := readRun(cmd.Context(), journal, args[0])
				if err != nil {
					return err
				}
				if len(r.Turns) == 0 {
					return fmt.Errorf("bonnie: run %s has no turns", r.ID)
				}
				t := r.Turns[len(r.Turns)-1]
				if turn != 0 {
					if turn < 1 || turn > len(r.Turns) {
						return fmt.Errorf("bonnie: run %s has turns 1 to %d", r.ID, len(r.Turns))
					}
					t = r.Turns[turn-1]
				}
				tr := runsui.BuildTrace(r, t, time.Now())
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), tr)
				}
				w := width
				if w <= 0 {
					w = 100
					if out, ok := cmd.OutOrStdout().(*os.File); ok && term.IsTerminal(out.Fd()) {
						if tw, _, err := term.GetSize(out.Fd()); err == nil && tw > 40 {
							w = tw
						}
					}
				}
				_, err = lipgloss.Fprintln(cmd.OutOrStdout(), runsui.RenderTrace(tr, w))
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&dir, "journal", defaultJournal, "journal directory")
	f.IntVar(&turn, "turn", 0, "turn number (default: the latest)")
	f.IntVar(&width, "width", 0, "output width (default: the terminal's, or 100)")
	f.BoolVar(&asJSON, "json", false, "print the spans as JSON")
	return cmd
}

// actionFlags are the flags every server action takes.
type actionFlags struct{ addr, token string }

func (f *actionFlags) register(cmd *cobra.Command, defaultAddr string) {
	cmd.Flags().StringVar(&f.addr, "addr", defaultAddr, "HTTP channel address")
	cmd.Flags().StringVar(&f.token, "token", "", "bearer token for the HTTP channel")
}

func newRunsActionCmd(defaultAddr, name, short, long string, do func(context.Context, *client.Client, string) (string, error)) *cobra.Command {
	var f actionFlags
	cmd := &cobra.Command{
		Use:   name + " <run-id>",
		Short: short,
		Long:  short + ".\n\n" + long + "\n\nThe command needs a running server; see --addr.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			done, err := do(cmd.Context(), newClient(f.addr, f.token), args[0])
			if err != nil {
				return fmt.Errorf("bonnie: runs %s: %w", name, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", args[0], done)
			return err
		},
	}
	f.register(cmd, defaultAddr)
	return cmd
}

func newRunsRetireCmd(defaultAddr string) *cobra.Command {
	var reason string
	cmd := newRunsActionCmd(defaultAddr, "retire", "Retire a run for good",
		"A retired run keeps its history and stays readable, but takes no more\nturns, and the address it served starts a fresh run.",
		func(ctx context.Context, c *client.Client, id string) (string, error) {
			return "retired", c.Reset(ctx, id, reason)
		})
	cmd.Flags().StringVar(&reason, "reason", "", "why the run was retired")
	return cmd
}

func newRunsSendCmd(defaultAddr string) *cobra.Command {
	var f actionFlags
	cmd := &cobra.Command{
		Use:   "send <run-id> <message>",
		Short: "Send a message to a run as a new turn, and print the reply",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := newClient(f.addr, f.token).Send(cmd.Context(), args[0], strings.Join(args[1:], " "))
			if err != nil {
				return fmt.Errorf("bonnie: runs send: %w", err)
			}
			return printRun(cmd.OutOrStdout(), run)
		},
	}
	f.register(cmd, defaultAddr)
	return cmd
}

func newRunsAnswerCmd(defaultAddr string) *cobra.Command {
	var (
		f               actionFlags
		approve, reject bool
		turnID          string
	)
	cmd := &cobra.Command{
		Use:   "answer <run-id> [text]",
		Short: "Answer a run that waits for input",
		Long: `Answer a run that waits for input, and print the outcome.

For an approval, pass --approve or --reject; the text, when given, is a note
for the agent. For a question, the text is the answer.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if approve && reject {
				return errors.New("bonnie: runs answer: --approve and --reject exclude each other")
			}
			text := strings.Join(args[1:], " ")
			resp := wire.InputResponse{TurnID: turnID, Text: text}
			switch {
			case approve:
				resp = wire.Approve(text)
			case reject:
				resp = wire.Reject(text)
			case text == "":
				return errors.New("bonnie: runs answer: give an answer, or --approve or --reject")
			}
			resp.TurnID = turnID
			run, err := newClient(f.addr, f.token).RespondWith(cmd.Context(), args[0], []wire.InputResponse{resp})
			if err != nil {
				return fmt.Errorf("bonnie: runs answer: %w", err)
			}
			return printRun(cmd.OutOrStdout(), run)
		},
	}
	f.register(cmd, defaultAddr)
	cmd.Flags().BoolVar(&approve, "approve", false, "approve an approval request")
	cmd.Flags().BoolVar(&reject, "reject", false, "reject an approval request")
	cmd.Flags().StringVar(&turnID, "turn", "", "the suspended turn's ID, so a stale answer is refused")
	return cmd
}

func printRun(w io.Writer, run *wire.Run) error {
	if _, err := fmt.Fprintf(w, "run %s  state=%s\n", run.ID, run.State); err != nil {
		return err
	}
	if run.Suspend != nil {
		_, err := fmt.Fprintf(w, "waiting: %s: %s\n", run.Suspend.Kind, run.Suspend.Prompt)
		return err
	}
	if run.Response != "" {
		_, err := fmt.Fprintf(w, "\n%s\n", run.Response)
		return err
	}
	return nil
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

// oneLine folds s onto one line and caps it at n runes, not bytes.
// A multi-byte character is never split.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:max(0, n-1)]) + "…"
	}
	return s
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
