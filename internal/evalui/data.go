// Package evalui is the terminal interface of `bonnie eval`: it reads the
// output of go test -json, keeps each eval run under .bonnie/evals, and shows
// the evals, their scores and their conversations.
//
// It does not import package eval, because eval imports package testing and
// the CLI binary must not link it. It copies the names and the report shape
// of eval instead; TestNamesMatchEval keeps the copies equal.
package evalui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Names copied from package eval.
const (
	ReportPrefix = "bonnie-eval: "
	RunPrefix    = "bonnie-eval-run: "
	EnvJudge     = "BONNIE_EVAL_JUDGE"
	EnvLLMJudge  = "BONNIE_EVAL_LLM_JUDGE"
	EnvStrict    = "BONNIE_EVAL_STRICT"
	EnvDir       = "BONNIE_EVAL_DIR"
	// RunID is the run each eval drives in its journal.
	RunID = "eval"
)

// Dir is the directory under a tree's journal directory that keeps the eval
// runs, one subdirectory for each `bonnie eval`.
const Dir = "evals"

// resultsFile holds a [Session] in its run directory.
const resultsFile = "results.json"

// Report is eval.Report: one score.
type Report struct {
	Test   string   `json:"test"`
	Label  string   `json:"label"`
	Kind   string   `json:"kind"`
	Value  float64  `json:"value"`
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
	Gate   bool     `json:"gate,omitempty"`
	Status string   `json:"status"`
	Model  string   `json:"model,omitempty"`
	Reason string   `json:"reason,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// runInfo is eval.RunInfo.
type runInfo struct {
	Test    string `json:"test"`
	Journal string `json:"journal"`
	Turns   int    `json:"turns"`
}

// Statuses of a [Result].
const (
	StatusRunning = "running"
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusSkip    = "skip"
)

// Result is one run of one eval.
type Result struct {
	Package string    `json:"package"`
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Started time.Time `json:"started,omitzero"`
	Elapsed float64   `json:"elapsed"`
	Output  []string  `json:"output,omitempty"`
	Reports []Report  `json:"reports,omitempty"`
	// Journal is the eval's kept journal directory, or "".
	Journal string `json:"journal,omitempty"`
	Turns   int    `json:"turns,omitempty"`
	// Parent is true for a test whose subtests are the evals. It is not
	// counted or shown.
	Parent bool `json:"parent,omitempty"`
	// Run is the ID of the earlier run that made this result, for a result
	// that a rerun carried over. It is "" for a result of this run.
	Run string `json:"run,omitempty"`
}

// Carried reports whether the result came from an earlier run.
func (r *Result) Carried() bool { return r.Run != "" }

// Failure returns the lines of the output that explain a failure: the test
// log without the go test framing.
func (r *Result) Failure() []string {
	var out []string
	for _, o := range r.Output {
		t := strings.TrimSpace(o)
		if t == "" || strings.HasPrefix(t, "=== ") || strings.HasPrefix(t, "--- ") {
			continue
		}
		out = append(out, strings.TrimRight(o, "\n"))
	}
	return out
}

// Session is one `bonnie eval`: its settings and each eval result. Its
// methods are safe for concurrent use.
type Session struct {
	mu sync.Mutex
	// The fields below are the saved state.
	ID       string    `json:"id"`
	Tree     string    `json:"tree"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Strict   bool      `json:"strict,omitempty"`
	Judge    string    `json:"judge,omitempty"`
	LLMJudge string    `json:"llm_judge,omitempty"`
	Count    int       `json:"count,omitempty"`
	// From is the ID of the run that this one reruns evals of, or "".
	From    string    `json:"from,omitempty"`
	Results []*Result `json:"results"`
	// Build is build output and the output of a package that failed with
	// no test.
	Build []string `json:"build,omitempty"`
	// Err is why go test failed when no eval did, or "".
	Err string `json:"error,omitempty"`

	dir     string
	running map[string]*Result
	pending map[string][]string
	// carried holds the results that Seed carried over and no test of
	// this session has reported again, by package and name.
	carried map[string]*Result
	// queued holds the rerun evals that go test has not reported yet.
	queued  map[string]bool
	changed chan struct{}
}

// NewSession returns a session that keeps its data in dir.
func NewSession(id, dir, tree string) *Session {
	return &Session{ID: id, Tree: tree, Started: time.Now(), dir: dir, changed: make(chan struct{}, 1)}
}

// Dir returns the run directory of the session.
func (s *Session) Dir() string { return s.dir }

// Changed returns a channel that receives after each change. It does not
// block a writer: a reader that is behind sees one signal.
func (s *Session) Changed() <-chan struct{} { return s.changed }

func (s *Session) notify() {
	if s.changed == nil {
		return
	}
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Seed starts the session with each result of prev, for a rerun of rerun,
// and sets From. The rerun evals show as running at once, before go test
// has built the tests, and their new result replaces the old one in its
// place. The other results keep the result of the run that made it, so the
// session shows the whole suite, and its counts are those of the whole
// suite.
func (s *Session) Seed(prev *Session, rerun []Result) {
	again := map[string]bool{}
	for _, r := range rerun {
		again[r.Package+"\x00"+r.Name] = true
	}
	now := time.Now()
	prev.mu.Lock()
	results := make([]*Result, len(prev.Results))
	for i, r := range prev.Results {
		c := *r
		c.Output, c.Reports = slices.Clone(r.Output), slices.Clone(r.Reports)
		if again[r.Package+"\x00"+r.Name] {
			c = Result{Package: r.Package, Name: r.Name, Status: StatusRunning, Started: now, Parent: r.Parent}
		} else if c.Run == "" {
			c.Run = prev.ID
		}
		results[i] = &c
	}
	id := prev.ID
	prev.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notify()
	s.From = id
	s.carried, s.queued = map[string]*Result{}, map[string]bool{}
	for _, r := range results {
		key := r.Package + "\x00" + r.Name
		s.Results = append(s.Results, r)
		s.carried[key] = r
		if again[key] {
			s.queued[key] = true
		}
	}
}

// testEvent is one line of go test -json (see go doc test2json).
type testEvent struct {
	// ImportPath is set on a build event in place of Package.
	ImportPath string  `json:"ImportPath"`
	Action     string  `json:"Action"`
	Package    string  `json:"Package"`
	Test       string  `json:"Test"`
	Elapsed    float64 `json:"Elapsed"`
	Output     string  `json:"Output"`
}

// Feed adds one line of go test -json to the session. A line that is not an
// event is build output.
func (s *Session) Feed(line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notify()
	var ev testEvent
	if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &ev) != nil {
		s.Build = append(s.Build, string(line))
		return
	}
	if ev.Action == "build-output" {
		s.Build = append(s.Build, strings.TrimRight(ev.Output, "\n"))
		return
	}
	if s.running == nil {
		s.running = map[string]*Result{}
	}
	if ev.Test == "" {
		switch ev.Action {
		case "output":
			s.pkgOutput(ev.Package, ev.Output)
		case "fail":
			s.pkgFail(ev.Package)
		}
		return
	}
	key := ev.Package + "\x00" + ev.Test
	switch ev.Action {
	case "run":
		res := &Result{Package: ev.Package, Name: ev.Test, Status: StatusRunning, Started: time.Now()}
		if old := s.carried[key]; old != nil {
			// A rerun: the new result takes the place of the old one.
			delete(s.carried, key)
			delete(s.queued, key)
			res.Parent = old.Parent
			*old = *res
			res = old
		} else {
			s.Results = append(s.Results, res)
		}
		s.running[key] = res
		if i := strings.LastIndex(ev.Test, "/"); i > 0 {
			if parent := s.running[ev.Package+"\x00"+ev.Test[:i]]; parent != nil {
				parent.Parent = true
			}
		}
	case "output":
		res := s.running[key]
		if res == nil {
			return
		}
		if rep, ok := cut[Report](ev.Output, ReportPrefix); ok {
			res.Reports = append(res.Reports, rep)
			return
		}
		if info, ok := cut[runInfo](ev.Output, RunPrefix); ok {
			res.Journal, res.Turns = info.Journal, info.Turns
			return
		}
		res.Output = append(res.Output, ev.Output)
	case "pass", "fail", "skip":
		if res := s.running[key]; res != nil {
			res.Status, res.Elapsed = ev.Action, ev.Elapsed
		}
	}
}

// pkgOutput holds the output of a package until the package fails.
func (s *Session) pkgOutput(pkg, out string) {
	if s.pending == nil {
		s.pending = map[string][]string{}
	}
	s.pending[pkg] = append(s.pending[pkg], out)
}

// pkgFail keeps the output of a package that failed with no eval of its own,
// for example a build failure or a panic in init.
func (s *Session) pkgFail(pkg string) {
	for _, r := range s.Results {
		if r.Package == pkg && !r.Carried() {
			return
		}
	}
	s.Build = append(s.Build, "FAIL "+pkg)
	for _, o := range s.pending[pkg] {
		s.Build = append(s.Build, strings.TrimRight(o, "\n"))
	}
}

// cut decodes the JSON after prefix in a log line.
func cut[T any](line, prefix string) (T, bool) {
	var v T
	_, rest, ok := strings.Cut(line, prefix)
	if !ok {
		return v, false
	}
	return v, json.Unmarshal([]byte(strings.TrimSpace(rest)), &v) == nil
}

// Finish marks the session done. err is the go test error, if any.
func (s *Session) Finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notify()
	s.Finished = time.Now()
	// A test that never ended was stopped.
	failed := false
	for _, r := range s.Results {
		if r.Carried() {
			continue
		}
		if r.Status == StatusRunning {
			r.Status = StatusFail
			if s.queued[r.Package+"\x00"+r.Name] {
				r.Output = append(r.Output, "go test did not run this eval; see the build output\n")
			} else {
				r.Output = append(r.Output, "stopped before the eval ended\n")
			}
		}
		failed = failed || (r.Status == StatusFail && !r.Parent)
	}
	// go test fails when an eval fails. Its error explains something only
	// when no eval did.
	if err != nil && !failed {
		s.Err = err.Error()
	}
}

// Snapshot is a copy of a session, safe to read without a lock.
type Snapshot struct {
	ID, Tree, Dir, Err string
	// From is the ID of the run that this one reruns evals of, or "".
	From              string
	Started, Finished time.Time
	Strict            bool
	Judge, LLMJudge   string
	// Evals are the results that are evals: each test that is not a
	// parent of subtests, in the order they started.
	Evals []Result
	Build []string
}

// Done reports whether the session has finished.
func (s Snapshot) Done() bool { return !s.Finished.IsZero() }

// Snapshot returns a copy of the session.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{ID: s.ID, Tree: s.Tree, Dir: s.dir, Err: s.Err, From: s.From, Started: s.Started, Finished: s.Finished,
		Strict: s.Strict, Judge: s.Judge, LLMJudge: s.LLMJudge, Build: slices.Clone(s.Build)}
	for _, r := range s.Results {
		if r.Parent {
			continue
		}
		c := *r
		c.Output, c.Reports = slices.Clone(r.Output), slices.Clone(r.Reports)
		snap.Evals = append(snap.Evals, c)
	}
	return snap
}

// Counts are the totals of a snapshot.
type Counts struct {
	Evals, Running, Passed, Failed, Skipped int
	// Scores counts the scores by report status.
	Scores map[string]int
}

// Counts returns the totals of the snapshot.
func (s Snapshot) Counts() Counts {
	c := Counts{Scores: map[string]int{}}
	for _, r := range s.Evals {
		c.Evals++
		switch r.Status {
		case StatusRunning:
			c.Running++
		case StatusPass:
			c.Passed++
		case StatusSkip:
			c.Skipped++
		default:
			c.Failed++
		}
		for _, rep := range r.Reports {
			c.Scores[rep.Status]++
		}
	}
	return c
}

// Save writes the session to results.json in its run directory.
func (s *Session) Save() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("bonnie: encode eval results: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, resultsFile), append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("bonnie: write eval results: %w", err)
	}
	return nil
}

// ErrNoRuns means that a tree has no saved eval run.
var ErrNoRuns = errors.New("bonnie: no saved eval runs")

// Load reads the session saved in dir.
func Load(dir string) (*Session, error) {
	b, err := os.ReadFile(filepath.Join(dir, resultsFile))
	if err != nil {
		return nil, fmt.Errorf("bonnie: read eval results: %w", err)
	}
	s := &Session{dir: dir}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("bonnie: decode eval results %s: %w", dir, err)
	}
	return s, nil
}

// NewRun makes a new run directory under root and returns its ID and path.
// The ID is the local start time, so the IDs sort in time order.
func NewRun(root string, now time.Time) (id, dir string, err error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", fmt.Errorf("bonnie: create %s: %w", root, err)
	}
	base := now.Format("20060102-150405")
	for i := 1; i < 1000; i++ {
		id = base
		if i > 1 {
			id = fmt.Sprintf("%s-%d", base, i)
		}
		dir = filepath.Join(root, id)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return id, dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", fmt.Errorf("bonnie: create %s: %w", dir, err)
		}
	}
	return "", "", fmt.Errorf("bonnie: no free eval run directory in %s", root)
}

// List returns the IDs of the saved runs under root, newest first.
func List(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bonnie: read %s: %w", root, err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), resultsFile)); err == nil {
			ids = append(ids, e.Name())
		}
	}
	slices.Sort(ids)
	slices.Reverse(ids)
	return ids, nil
}

// unsavedGrace is how long Prune keeps a run directory with no results. A
// run writes its results only when it ends, so a recent directory without
// them can be the active run of another `bonnie eval` in the same tree.
// One older than this is what a crash left.
const unsavedGrace = 24 * time.Hour

// Prune removes the saved runs under root except the newest keep, and the
// runs they carry results from. A run directory without results is removed
// when it is older than a day, which is what a crash leaves; a newer one
// can belong to a run of another process, so it stays. current always
// stays. A keep of 0 or less keeps each run.
func Prune(root, current string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bonnie: read %s: %w", root, err)
	}
	ids, err := List(root)
	if err != nil {
		return err
	}
	saved := map[string]bool{}
	for i, id := range ids {
		saved[id] = i < keep || id == current
	}
	// A kept run can hold results, and point to journals, of the runs it
	// reruns. Those runs stay while it does.
	for id, keepIt := range saved {
		if !keepIt {
			continue
		}
		s, err := Load(filepath.Join(root, id))
		if err != nil {
			continue
		}
		for _, r := range s.Results {
			if r.Run != "" {
				saved[r.Run] = true
			}
		}
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || e.Name() == current || saved[e.Name()] {
			continue
		}
		if _, listed := saved[e.Name()]; !listed {
			// No results: a run in progress, or one that a crash left.
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < unsavedGrace {
				continue
			}
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
