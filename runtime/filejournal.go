package runtime

import (
	"bufio"
	"context"
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

// ErrInvalidRunID is returned when a run ID cannot become a file name safely.
var ErrInvalidRunID = errors.New("bonnie: invalid run ID")

// FsyncPolicy decides how hard a [FileJournal] works to get a record onto the
// platter before it reports success.
type FsyncPolicy string

const (
	// FsyncAlways flushes every record to disk before Append returns. This is
	// the default, and it is what makes the durability claim true: a record
	// that Append acknowledged survives a power cut.
	FsyncAlways FsyncPolicy = "always"
	// FsyncInterval flushes at most once per interval. It trades a bounded
	// window of recent records for throughput.
	FsyncInterval FsyncPolicy = "interval"
)

// maxRecordLine caps one journal line. Tool arguments are the only records
// that grow, and they are bounded by the model's output limit long before
// this.
const maxRecordLine = 64 << 20

// FileJournal is a [Journal] backed by one append-only JSONL file per run.
//
// The layout is <root>/runs/<run-id>.jsonl, one JSON-encoded [Record] per
// line, which keeps a run inspectable with jq and cheap to append to. Run
// state is derived from the last [RecordState] record rather than kept in a
// sidecar, so there is only one file to keep consistent.
//
// One process owns a run at a time. FileJournal serialises writes within a
// process, but it takes no cross-process lock: two processes appending to the
// same run will interleave records. Cross-process ownership is out of scope
// for v0.1.0.
type FileJournal struct {
	root     string
	policy   FsyncPolicy
	interval time.Duration

	mu   sync.Mutex
	runs map[string]*runFile
}

var _ Journal = (*FileJournal)(nil)

// FileJournalOption configures a [FileJournal].
type FileJournalOption func(*FileJournal)

// WithFsync sets the flush policy. The default is [FsyncAlways].
func WithFsync(policy FsyncPolicy) FileJournalOption {
	return func(j *FileJournal) { j.policy = policy }
}

// WithFsyncInterval selects [FsyncInterval] with the given period.
func WithFsyncInterval(d time.Duration) FileJournalOption {
	return func(j *FileJournal) {
		j.policy = FsyncInterval
		j.interval = d
	}
}

// OpenFileJournal opens, and creates when absent, a journal rooted at dir.
// Pass an empty dir to use ".bonnie".
func OpenFileJournal(dir string, opts ...FileJournalOption) (*FileJournal, error) {
	if dir == "" {
		dir = ".bonnie"
	}
	j := &FileJournal{
		root:     dir,
		policy:   FsyncAlways,
		interval: time.Second,
		runs:     make(map[string]*runFile),
	}
	for _, opt := range opts {
		opt(j)
	}
	if err := os.MkdirAll(j.runsDir(), 0o755); err != nil {
		return nil, fmt.Errorf("bonnie: open journal %s: %w", dir, err)
	}
	return j, nil
}

// Root returns the directory the journal is rooted at.
func (j *FileJournal) Root() string { return j.root }

func (j *FileJournal) runsDir() string { return filepath.Join(j.root, "runs") }

// runFile owns one run's file. Its mutex serialises every read and write of
// that run, so a Checkpoint is one atomic unit even though it both appends a
// record and moves the cached state.
type runFile struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	seq    int
	state  RunState
	exists bool
	loaded bool
	synced time.Time
}

// run returns the handle for a run, creating the in-memory entry but not the
// file.
func (j *FileJournal) run(runID string) (*runFile, error) {
	if err := validRunID(runID); err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	rf, ok := j.runs[runID]
	if !ok {
		rf = &runFile{path: filepath.Join(j.runsDir(), runID+".jsonl")}
		j.runs[runID] = rf
	}
	return rf, nil
}

// validRunID rejects any ID that would escape the runs directory or collide
// with a different ID once written to a file name.
func validRunID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidRunID)
	}
	if len(id) > 200 {
		return fmt.Errorf("%w: longer than 200 bytes", ErrInvalidRunID)
	}
	if strings.HasPrefix(id, ".") {
		return fmt.Errorf("%w: %q starts with a dot", ErrInvalidRunID, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%w: %q contains %q", ErrInvalidRunID, id, string(r))
		}
	}
	return nil
}

// loadLocked reads the run file once to recover the sequence counter and the
// current state. The caller must hold rf.mu.
func (rf *runFile) loadLocked() error {
	if rf.loaded {
		return nil
	}
	recs, err := readRecords(rf.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		rf.loaded, rf.exists = true, false
		return nil
	case err != nil:
		return err
	}
	rf.exists = true
	rf.seq = len(recs)
	for _, rec := range recs {
		if rec.Kind == RecordState {
			rf.state = rec.State
		}
	}
	if rf.state == "" && len(recs) > 0 {
		rf.state = RunPending
	}
	rf.loaded = true
	return nil
}

// writerLocked returns the open append handle, opening it on first use. The
// caller must hold rf.mu.
func (rf *runFile) writerLocked() (*os.File, error) {
	if rf.file != nil {
		return rf.file, nil
	}
	f, err := os.OpenFile(rf.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("bonnie: open run file: %w", err)
	}
	rf.file = f
	rf.exists = true
	return f, nil
}

// appendLocked writes one record. The caller must hold rf.mu.
func (j *FileJournal) appendLocked(rf *runFile, rec Record) (int, error) {
	if err := rf.loadLocked(); err != nil {
		return 0, err
	}
	f, err := rf.writerLocked()
	if err != nil {
		return 0, err
	}

	rf.seq++
	rec.Seq = rf.seq
	line, err := json.Marshal(rec)
	if err != nil {
		rf.seq--
		return 0, fmt.Errorf("bonnie: encode record: %w", err)
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		rf.seq--
		return 0, fmt.Errorf("bonnie: write record: %w", err)
	}
	if err := j.syncLocked(rf, f); err != nil {
		return 0, err
	}

	switch {
	case rec.Kind == RecordState:
		rf.state = rec.State
	case rf.state == "":
		rf.state = RunPending
	}
	return rec.Seq, nil
}

func (j *FileJournal) syncLocked(rf *runFile, f *os.File) error {
	if j.policy == FsyncInterval && time.Since(rf.synced) < j.interval {
		return nil
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("bonnie: sync run file: %w", err)
	}
	rf.synced = time.Now()
	return nil
}

// Append implements [Journal].
func (j *FileJournal) Append(_ context.Context, rec Record) (int, error) {
	rf, err := j.run(rec.RunID)
	if err != nil {
		return 0, err
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return j.appendLocked(rf, rec)
}

// Replay implements [Journal].
func (j *FileJournal) Replay(_ context.Context, runID string) ([]Record, error) {
	rf, err := j.run(runID)
	if err != nil {
		return nil, err
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()

	recs, err := readRecords(rf.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrRunNotFound
	}
	return recs, err
}

// Checkpoint implements [Journal]. The state move and the record that
// documents it happen under one lock, so the two can never disagree.
func (j *FileJournal) Checkpoint(_ context.Context, runID string, state RunState) error {
	rf, err := j.run(runID)
	if err != nil {
		return err
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()

	_, err = j.appendLocked(rf, Record{
		RunID: runID, Kind: RecordState, State: state, Timestamp: now(),
	})
	return err
}

// State implements [Journal].
func (j *FileJournal) State(_ context.Context, runID string) (RunState, error) {
	rf, err := j.run(runID)
	if err != nil {
		return "", err
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if err := rf.loadLocked(); err != nil {
		return "", err
	}
	if !rf.exists {
		return "", ErrRunNotFound
	}
	return rf.state, nil
}

// Runs implements [Journal]. It lists the runs directory, so it sees runs
// written by an earlier process.
func (j *FileJournal) Runs(ctx context.Context, state RunState) ([]string, error) {
	entries, err := os.ReadDir(j.runsDir())
	if err != nil {
		return nil, fmt.Errorf("bonnie: list runs: %w", err)
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		s, err := j.State(ctx, id)
		if errors.Is(err, ErrRunNotFound) || errors.Is(err, ErrInvalidRunID) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if state == "" || s == state {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Persisted implements [Journal].
func (j *FileJournal) Persisted() bool { return true }

// Close implements [Journal]. It closes every open run file; the journal is
// usable again afterwards, reopening files on demand.
func (j *FileJournal) Close() error {
	j.mu.Lock()
	runs := make([]*runFile, 0, len(j.runs))
	for _, rf := range j.runs {
		runs = append(runs, rf)
	}
	j.mu.Unlock()

	var errs []error
	for _, rf := range runs {
		rf.mu.Lock()
		if rf.file != nil {
			if err := rf.file.Close(); err != nil {
				errs = append(errs, err)
			}
			rf.file = nil
		}
		rf.mu.Unlock()
	}
	if len(errs) > 0 {
		return fmt.Errorf("bonnie: close journal: %w", errors.Join(errs...))
	}
	return nil
}

// readRecords parses a run file.
//
// A truncated final line is a torn write, not a corrupt journal: the process
// died mid-append. It is dropped, and the conversation-level consequence is
// handled by the repair in [Restore]. A malformed line anywhere else is real
// corruption and is reported.
func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLine)

	var (
		out  []Record
		prev []byte
		line int
	)
	parse := func(raw []byte, at int) (Record, error) {
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return rec, fmt.Errorf("bonnie: journal %s line %d: %w", path, at, err)
		}
		return rec, nil
	}

	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		// A line is parsed only once a later line proves it was written
		// whole. That keeps the final line droppable without a second pass.
		if prev != nil {
			rec, err := parse(prev, line)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		}
		line++
		prev = append(prev[:0:0], raw...)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("bonnie: read journal %s: %w", path, err)
	}
	if prev != nil {
		// The last line is the only one a crash can truncate. Drop it.
		if rec, err := parse(prev, line); err == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}
