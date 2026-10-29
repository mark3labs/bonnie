package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/internal/evalui"
)

// evalBuildTag is the build tag of an eval file.
const evalBuildTag = "eval"

// defaultEvalTimeout bounds one `bonnie eval`. Evals call live models, so
// it is longer than the go test default.
const defaultEvalTimeout = 30 * time.Minute

// defaultEvalKeep is how many saved eval runs a tree keeps.
const defaultEvalKeep = 10

// evalOpts carries the parsed flags of `bonnie eval`.
type evalOpts struct {
	run      string
	strict   bool
	judge    string
	llmJudge string
	count    int
	parallel int
	timeout  time.Duration
	json     bool
	tui      bool
	keep     int

	// env overrides the environment go test inherits. It is test-only
	// plumbing: a hermetic test points the child build at a local checkout.
	env []string
}

// newEvalCmd mounts `bonnie eval`.
func newEvalCmd() *cobra.Command {
	var o evalOpts
	cmd := &cobra.Command{
		Use:   "eval [dir]",
		Short: "Run the evals of an agent tree and report the scores",
		Long: `Run the evals of the agent tree at dir and report each eval and score.

An eval is a Go test in a file that ends in _eval_test.go and starts with the
build constraint //go:build eval. It drives the agent with package
github.com/mark3labs/bonnie/eval and grades it with checks, a decision-model
judge, or a language-model judge.

eval generates the tool wiring as build does, then runs
go test -tags eval -json ./... in the tree. On a terminal it shows the evals
live, then lets you read each score, failure and conversation. Press o on an
eval to open its journal in the run console, r to run it again, and R to run
each failed eval again; each rerun is a new saved run. With --tui=false, or when the
output is not a terminal, it prints a plain table.

Each run is kept in .bonnie/evals/<id>: the results and the journal of each
eval. "bonnie eval show" opens a saved run again, and "bonnie eval list"
lists them. The newest --keep runs are kept.

The exit code is 0 only when every eval passes its gates. With --strict, a
score that misses a soft threshold fails its eval too. A tree with no evals
is an error, so CI cannot pass without a check.

--judge and --llm-judge set the judge models for each eval that does not set
its own, through BONNIE_EVAL_JUDGE and BONNIE_EVAL_LLM_JUDGE. A judgment with
no model fails its eval.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			opts := o
			opts.tui = o.tui && !o.json && isTerminalCmd(cmd)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runEval(ctx, dir, opts, cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.run, "run", "", "run only the evals whose names match this regular expression (go test -run)")
	f.BoolVar(&o.strict, "strict", false, "fail an eval when a score misses a soft threshold")
	f.StringVar(&o.judge, "judge", "", "decision model of the judge, as provider/model")
	f.StringVar(&o.llmJudge, "llm-judge", "", "language model of the LLM judge, as provider/model")
	f.IntVar(&o.count, "count", 1, "run each eval this many times")
	f.IntVar(&o.parallel, "parallel", 0, "maximum evals that run at the same time (default GOMAXPROCS)")
	f.DurationVar(&o.timeout, "timeout", defaultEvalTimeout, "fail when the evals take longer")
	f.BoolVar(&o.json, "json", false, "print each score as one JSON line in place of the table; turns off the interface")
	f.BoolVar(&o.tui, "tui", true, "show the evals in the terminal interface; use --tui=false for a plain table")
	f.IntVar(&o.keep, "keep", defaultEvalKeep, "saved runs to keep in .bonnie/evals; 0 keeps all")
	cmd.AddCommand(newEvalShowCmd(), newEvalListCmd())
	return cmd
}

// isTerminalCmd reports whether the command reads and writes a terminal.
func isTerminalCmd(cmd *cobra.Command) bool {
	in, ok1 := cmd.InOrStdin().(*os.File)
	out, ok2 := cmd.OutOrStdout().(*os.File)
	return ok1 && ok2 && term.IsTerminal(in.Fd()) && term.IsTerminal(out.Fd())
}

// evalStore is the directory of a tree's saved eval runs.
func evalStore(root string) string {
	return filepath.Join(root, bonnie.DefaultJournal, evalui.Dir)
}

// evalTarget is one go test invocation of a run: the packages and the
// -run pattern.
type evalTarget struct {
	pkg string
	run string
}

// runEval is the `bonnie eval` implementation, separated from the cobra
// wiring so tests drive it directly.
func runEval(ctx context.Context, root string, o evalOpts, out io.Writer) error {
	r, err := newEvalRunner(ctx, root, o)
	if err != nil {
		return err
	}
	a, err := r.start([]evalTarget{{pkg: "./...", run: o.run}}, nil, nil)
	if err != nil {
		return err
	}
	return r.finish(out, a.sess, "")
}

// evalRunner runs the saved runs of one command: the first, and each rerun
// that the interface asks for. One run is active at a time.
type evalRunner struct {
	ctx  context.Context
	root string
	o    evalOpts

	mu  sync.Mutex
	cur *evalActive
}

// evalActive is a run that has started. done receives once, when the run
// has ended and is saved.
type evalActive struct {
	sess   *evalui.Session
	cancel context.CancelFunc
	done   chan evalDone
}

type evalDone struct {
	// runErr is the go test error; err is why the run could not be saved.
	runErr, err error
}

func newEvalRunner(ctx context.Context, root string, o evalOpts) (*evalRunner, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("bonnie: resolve eval dir: %w", err)
	}
	if o.count < 1 {
		return nil, fmt.Errorf("bonnie: --count must be 1 or more")
	}
	return &evalRunner{ctx: ctx, root: root, o: o}, nil
}

// start runs targets as a new saved run, in the background. A rerun gives
// prev, the run it follows, and rerun, the evals it runs again: the new
// session starts with the results of prev (see evalui.Session.Seed).
func (r *evalRunner) start(targets []evalTarget, prev *evalui.Session, rerun []evalui.Result) (*evalActive, error) {
	// An eval tests the agent a build ships, so it uses the same wiring. It
	// is generated again for each rerun, which follows a change to the tree.
	content, plan, err := agent.Generate(r.root)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(r.root, plan.OutFile), content, 0o644); err != nil {
		return nil, fmt.Errorf("bonnie: write %s: %w", plan.OutFile, err)
	}
	store := evalStore(r.root)
	id, dir, err := evalui.NewRun(store, time.Now())
	if err != nil {
		return nil, err
	}
	sess := evalui.NewSession(id, dir, r.root)
	sess.Strict, sess.Judge, sess.LLMJudge, sess.Count = r.o.strict, r.o.judge, r.o.llmJudge, r.o.count
	if prev != nil {
		sess.Seed(prev, rerun)
	}

	ctx, cancel := context.WithCancel(r.ctx)
	a := &evalActive{sess: sess, cancel: cancel, done: make(chan evalDone, 1)}
	go func() {
		defer cancel()
		var errs []error
		for _, target := range targets {
			if ctx.Err() != nil {
				break
			}
			errs = append(errs, runGoTest(ctx, sess, r.root, r.o, target, filepath.Join(dir, "journals")))
		}
		runErr := errors.Join(errs...)
		sess.Finish(runErr)
		saveErr := sess.Save()
		a.done <- evalDone{runErr: runErr, err: errors.Join(saveErr, evalui.Prune(store, id, r.o.keep))}
	}()
	r.mu.Lock()
	r.cur = a
	r.mu.Unlock()
	return a, nil
}

// rerun is the evalui.RerunFunc of the interface. It waits until the
// active run is saved, so a run is never pruned or read while it is
// written, then starts the rerun.
func (r *evalRunner) rerun(prev *evalui.Session, evals []evalui.Result) (*evalui.Session, error) {
	r.mu.Lock()
	cur := r.cur
	r.mu.Unlock()
	if cur != nil {
		d := <-cur.done
		cur.done <- d
		if d.err != nil {
			return nil, d.err
		}
	}
	a, err := r.start(rerunTargets(evals), prev, evals)
	if err != nil {
		return nil, err
	}
	return a.sess, nil
}

// finish shows sess in the interface on a terminal, waits until the last
// run is saved, prints it, and returns its result. With no run active, as
// in "bonnie eval show" when the user did not rerun, it returns nil.
func (r *evalRunner) finish(out io.Writer, sess *evalui.Session, selected string) error {
	stopped := false
	var exploreErr error
	if r.o.tui {
		var act evalui.Action
		act, exploreErr = evalui.Explore(r.ctx, sess, evalui.ExploreOptions{Selected: selected, Rerun: r.rerun})
		stopped = act.Stopped
	}
	r.mu.Lock()
	cur := r.cur
	r.mu.Unlock()
	if cur == nil {
		return exploreErr
	}
	if stopped || exploreErr != nil {
		cur.cancel()
	}
	d := <-cur.done
	cur.done <- d
	if err := errors.Join(exploreErr, d.err); err != nil {
		return err
	}
	snap := cur.sess.Snapshot()
	var err error
	if r.o.json {
		err = evalui.WriteJSON(out, snap)
	} else {
		err = evalui.WritePlain(out, snap)
	}
	if err != nil {
		return err
	}
	return evalResult(snap, d.runErr, stopped)
}

// runGoTest runs one go test and feeds its output to the session. Both
// streams go to the session: a terminal interface owns the screen, so go
// test must not write to it.
func runGoTest(ctx context.Context, sess *evalui.Session, root string, o evalOpts, target evalTarget, journals string) error {
	test := goTest(ctx, root, o, target, journals)
	stdout, err := test.StdoutPipe()
	if err != nil {
		return fmt.Errorf("bonnie: go test: %w", err)
	}
	stderr, err := test.StderrPipe()
	if err != nil {
		return fmt.Errorf("bonnie: go test: %w", err)
	}
	if err := test.Start(); err != nil {
		return fmt.Errorf("bonnie: go test: %w", err)
	}
	errOut := make(chan error, 1)
	go func() { errOut <- feed(sess, stderr) }()
	readErr := feed(sess, stdout)
	readErr = errors.Join(readErr, <-errOut)
	return errors.Join(readErr, test.Wait())
}

// rerunTargets returns the go test invocations that run exactly evals.
//
// go test -run splits its pattern on "/" before it reads "|", so one
// pattern cannot name two subtests of different parents without also
// matching their cross product. The evals are grouped by package and
// parent test instead, with one invocation for each group: the parent's
// levels are anchored, and the last level lists the names of the group.
func rerunTargets(evals []evalui.Result) []evalTarget {
	type group struct {
		pkg, parent string
		leaves      []string
	}
	var groups []*group
	index := map[string]*group{}
	for _, r := range evals {
		parent, leaf := "", r.Name
		if i := strings.LastIndex(r.Name, "/"); i >= 0 {
			parent, leaf = r.Name[:i], r.Name[i+1:]
		}
		key := r.Package + "\x00" + parent
		g := index[key]
		if g == nil {
			g = &group{pkg: r.Package, parent: parent}
			index[key] = g
			groups = append(groups, g)
		}
		if !slices.Contains(g.leaves, leaf) {
			g.leaves = append(g.leaves, leaf)
		}
	}
	targets := make([]evalTarget, 0, len(groups))
	for _, g := range groups {
		var levels []string
		if g.parent != "" {
			for elem := range strings.SplitSeq(g.parent, "/") {
				levels = append(levels, "^"+regexp.QuoteMeta(elem)+"$")
			}
		}
		quoted := make([]string, len(g.leaves))
		for i, leaf := range g.leaves {
			quoted[i] = regexp.QuoteMeta(leaf)
		}
		levels = append(levels, "^("+strings.Join(quoted, "|")+")$")
		pkg := g.pkg
		if pkg == "" {
			pkg = "./..."
		}
		targets = append(targets, evalTarget{pkg: pkg, run: strings.Join(levels, "/")})
	}
	return targets
}

// evalResult is the error of a finished run, or nil when it passed.
func evalResult(snap evalui.Snapshot, runErr error, stopped bool) error {
	c := snap.Counts()
	switch {
	case stopped:
		return errors.New("bonnie: evals stopped before they ended")
	case c.Failed > 0:
		return fmt.Errorf("bonnie: %d of %d evals failed", c.Failed, c.Evals)
	case runErr != nil:
		// No eval failed, so the package did: a build error or a panic.
		return fmt.Errorf("bonnie: go test: %w", runErr)
	case c.Evals == 0:
		return errors.New("bonnie: no evals ran: add a _eval_test.go file that starts with //go:build eval")
	}
	return nil
}

// goTest returns the go test command of one target. It runs in its own
// process group: when ctx ends, SIGINT reaches go test and the test
// binaries, so each eval's cleanup can close its journal.
func goTest(ctx context.Context, root string, o evalOpts, target evalTarget, journals string) *exec.Cmd {
	args := []string{"test", "-tags", evalBuildTag, "-json", "-count", strconv.Itoa(o.count), "-timeout", o.timeout.String()}
	if target.run != "" {
		args = append(args, "-run", target.run)
	}
	if o.parallel > 0 {
		args = append(args, "-parallel", strconv.Itoa(o.parallel))
	}
	args = append(args, target.pkg)
	test := exec.CommandContext(ctx, "go", args...)
	test.Dir = root
	test.Env = evalEnv(o, journals)
	test.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	test.Cancel = func() error { return syscall.Kill(-test.Process.Pid, syscall.SIGINT) }
	test.WaitDelay = 10 * time.Second
	return test
}

// feed copies each line of r into the session.
func feed(sess *evalui.Session, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		sess.Feed(sc.Bytes())
	}
	return sc.Err()
}

// evalEnv is the environment of go test: the caller's, with the judge and
// strict settings of the flags and the directory for the eval journals.
func evalEnv(o evalOpts, journals string) []string {
	env := o.env
	if env == nil {
		env = os.Environ()
	}
	env = append([]string(nil), env...)
	env = append(env, evalui.EnvDir+"="+journals)
	if o.judge != "" {
		env = append(env, evalui.EnvJudge+"="+o.judge)
	}
	if o.llmJudge != "" {
		env = append(env, evalui.EnvLLMJudge+"="+o.llmJudge)
	}
	if o.strict {
		env = append(env, evalui.EnvStrict+"=1")
	}
	return env
}

// newEvalShowCmd mounts `bonnie eval show`.
func newEvalShowCmd() *cobra.Command {
	var id string
	var tui, jsonOut bool
	cmd := &cobra.Command{
		Use:   "show [dir]",
		Short: "Show a saved eval run",
		Long: `Show a saved eval run of the agent tree at dir: the newest one, or the one
that --id names. On a terminal it opens the eval interface, where r and R
rerun evals of the saved run with its settings. With --tui=false or --json it
prints the run as "bonnie eval" does.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			sess, err := loadEvalRun(dir, id)
			if err != nil {
				return err
			}
			switch {
			case jsonOut:
				return evalui.WriteJSON(cmd.OutOrStdout(), sess.Snapshot())
			case tui && isTerminalCmd(cmd):
				// A rerun from a saved run uses that run's settings.
				snap := sess.Snapshot()
				o := evalOpts{strict: snap.Strict, judge: snap.Judge, llmJudge: snap.LLMJudge, count: max(1, sess.Count),
					timeout: defaultEvalTimeout, keep: defaultEvalKeep, tui: true}
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				r, err := newEvalRunner(ctx, dir, o)
				if err != nil {
					return err
				}
				return r.finish(cmd.OutOrStdout(), sess, "")
			}
			return evalui.WritePlain(cmd.OutOrStdout(), sess.Snapshot())
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "ID of the saved run (default: the newest)")
	cmd.Flags().BoolVar(&tui, "tui", true, "show the run in the terminal interface")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print each score as one JSON line")
	return cmd
}

// loadEvalRun reads a saved run of the tree at dir: id, or the newest.
func loadEvalRun(dir, id string) (*evalui.Session, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("bonnie: resolve eval dir: %w", err)
	}
	store := evalStore(root)
	if id == "" {
		ids, err := evalui.List(store)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("%w in %s: run bonnie eval first", evalui.ErrNoRuns, store)
		}
		id = ids[0]
	}
	if id != filepath.Base(id) {
		return nil, fmt.Errorf("bonnie: invalid eval run ID %q", id)
	}
	return evalui.Load(filepath.Join(store, id))
}

// newEvalListCmd mounts `bonnie eval list`.
func newEvalListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [dir]",
		Short: "List the saved eval runs",
		Long:  `List the saved eval runs of the agent tree at dir, newest first.`,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			root, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("bonnie: resolve eval dir: %w", err)
			}
			return listEvalRuns(cmd.OutOrStdout(), evalStore(root))
		},
	}
}

func listEvalRuns(w io.Writer, store string) error {
	ids, err := evalui.List(store)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		_, err := fmt.Fprintf(w, "no saved eval runs in %s\n", store)
		return err
	}
	if _, err := fmt.Fprintf(w, "%-18s  %-6s  %-6s  %-6s  %s\n", "ID", "EVALS", "PASSED", "FAILED", "STARTED"); err != nil {
		return err
	}
	for _, id := range ids {
		sess, err := evalui.Load(filepath.Join(store, id))
		if err != nil {
			return err
		}
		snap := sess.Snapshot()
		c := snap.Counts()
		if _, err := fmt.Fprintf(w, "%-18s  %-6d  %-6d  %-6d  %s\n", id, c.Evals, c.Passed, c.Failed, snap.Started.Format(time.DateTime)); err != nil {
			return err
		}
	}
	return nil
}
