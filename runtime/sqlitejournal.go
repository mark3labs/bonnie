package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// The driver is pure Go: BONNIE must keep building with CGO_ENABLED=0,
	// because `bonnie build` promises the user a single static binary and
	// `goreleaser` cross-compiles it for four targets from one machine.
	_ "modernc.org/sqlite"
)

// ErrInvalidRunID is returned when a run ID is not safe to carry.
var ErrInvalidRunID = errors.New("bonnie: invalid run ID")

// ErrJournalSchema is returned when a store was written by a newer BONNIE
// than the one opening it. A journal whose schema this binary does not
// understand is refused rather than read with the wrong shape.
var ErrJournalSchema = errors.New("bonnie: unsupported journal schema")

// FsyncPolicy decides how hard a [SQLiteJournal] works to get a record onto
// the platter before it reports success.
type FsyncPolicy string

const (
	// FsyncAlways flushes every commit to disk before the write returns. It
	// is the default, and it is what makes the durability claim true: a
	// record that Append acknowledged survives a power cut. It maps to
	// SQLite's synchronous=FULL.
	FsyncAlways FsyncPolicy = "always"
	// FsyncRelaxed lets the operating system decide when the write-ahead log
	// reaches the platter, and fsyncs only when the log is checkpointed into
	// the database file. It trades a bounded window of recent records for
	// throughput. It maps to SQLite's synchronous=NORMAL, which in WAL mode
	// cannot corrupt the database — only lose the most recent commits.
	FsyncRelaxed FsyncPolicy = "relaxed"
)

// journalSchemaVersion is the shape of the store this binary writes. Open
// refuses a store that reports a higher number.
const journalSchemaVersion = 1

// Connection pool limits. See [OpenSQLiteJournal] for why the pool is
// bounded rather than left to grow.
const (
	maxJournalConns        = 16
	maxJournalIdleConns    = 4
	idleJournalConnTimeout = time.Minute
)

// sqliteSchema is the whole store. Every table is STRICT, so a column that
// should hold text cannot quietly accept a number: the journal is the record
// a run is rebuilt from, and a type surprise there is a replay bug.
//
// The (run_id, seq) primary key is the integrity guarantee the old JSONL
// journal needed a lock file for. Two writers cannot be given the same
// sequence number, because the second INSERT fails the constraint.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS runs (
	run_id TEXT PRIMARY KEY,
	state  TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS records (
	run_id    TEXT    NOT NULL,
	seq       INTEGER NOT NULL,
	kind      TEXT    NOT NULL,
	timestamp TEXT    NOT NULL,
	entry_id  TEXT    NOT NULL,
	parent_id TEXT    NOT NULL,
	role      TEXT    NOT NULL,
	ext_type  TEXT    NOT NULL,
	text      TEXT    NOT NULL,
	state     TEXT    NOT NULL,
	payload   BLOB,
	PRIMARY KEY (run_id, seq)
) STRICT;

CREATE INDEX IF NOT EXISTS runs_by_state ON runs (state);
`

const insertRecordSQL = `
INSERT INTO records
	(run_id, seq, kind, timestamp, entry_id, parent_id, role, ext_type, text, state, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const selectRecordSQL = `
SELECT run_id, seq, kind, timestamp, entry_id, parent_id, role, ext_type, text, state, payload
FROM records`

// DefaultJournalFile is the name of the database inside a journal root.
// The write-ahead log and shared-memory files SQLite keeps beside it carry
// the same name with "-wal" and "-shm" appended.
const DefaultJournalFile = "journal.db"

// SQLiteJournal is a [Journal] backed by a single SQLite database, opened
// with a pure-Go driver so BONNIE keeps cross-compiling with CGO_ENABLED=0.
//
// The layout is <root>/journal.db. Records live in one append-only table
// keyed by (run_id, seq); run state lives in a second table that every write
// updates in the same transaction, so a state can never disagree with the
// record that documents it.
//
// # What the database buys over the old JSONL files
//
// Three properties that the file journal had to work for, or could not have
// at all:
//
//   - A step is atomic by construction. [SQLiteJournal.AppendStep] commits
//     every record of a tool-calling step in one transaction, so the
//     "torn single Write" window the JSONL journal documented is closed.
//   - Two writers cannot collide. SQLite serialises write transactions, in
//     this process and across processes, and the (run_id, seq) primary key
//     makes a reused sequence number a constraint violation rather than
//     silent corruption. The per-run lock file, and the refusal it produced,
//     are gone: concurrent writers are now safe instead of forbidden.
//   - A read of an unknown run costs nothing. There is no per-run handle to
//     leak, so scanning invented run IDs cannot grow the process.
//
// # What it does not buy
//
// SQLite's locking is still per host. A journal on a network filesystem
// without working POSIX locks is unsafe, exactly as the lock file was.
// And journal integrity is not turn coordination: two [Runner] instances
// that both execute the same run produce an interleaved conversation, and
// nothing here stops them. One owner per run is still a deployment decision.
type SQLiteJournal struct {
	root   string
	path   string
	policy FsyncPolicy
	db     *sql.DB

	// writeMu serialises write transactions started by this journal. SQLite
	// would serialise them anyway — it is the busy-timeout poll that the
	// lock avoids, not the corruption.
	writeMu sync.Mutex
}

var (
	_ Journal     = (*SQLiteJournal)(nil)
	_ Positioner  = (*SQLiteJournal)(nil)
	_ StepJournal = (*SQLiteJournal)(nil)
)

// SQLiteJournalOption configures a [SQLiteJournal].
type SQLiteJournalOption func(*SQLiteJournal)

// WithFsync sets the flush policy. The default is [FsyncAlways].
func WithFsync(policy FsyncPolicy) SQLiteJournalOption {
	return func(j *SQLiteJournal) { j.policy = policy }
}

// OpenSQLiteJournal opens, and creates when absent, a journal rooted at dir.
// Pass an empty dir to use ".bonnie".
//
// A root that still holds runs from BONNIE's original JSONL journal is
// imported once, on open. See [importJSONLRuns] for what that does and does
// not touch.
func OpenSQLiteJournal(dir string, opts ...SQLiteJournalOption) (*SQLiteJournal, error) {
	if dir == "" {
		dir = ".bonnie"
	}
	j := &SQLiteJournal{
		root:   dir,
		path:   filepath.Join(dir, DefaultJournalFile),
		policy: FsyncAlways,
	}
	for _, opt := range opts {
		opt(j)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("bonnie: open journal %s: %w", dir, err)
	}

	dsn, err := j.dsn()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("bonnie: open journal %s: %w", j.path, err)
	}
	// Bound the pool. Every connection this driver opens carries its own
	// emulated libc state, so an unbounded pool turns a burst of concurrent
	// readers — one event stream replay per connected client — into a burst
	// of memory. Writes serialise on writeMu anyway, so the limit only ever
	// queues readers, and briefly.
	db.SetMaxOpenConns(maxJournalConns)
	db.SetMaxIdleConns(maxJournalIdleConns)
	db.SetConnMaxIdleTime(idleJournalConnTimeout)
	j.db = db

	ctx := context.Background()
	if err := j.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := importJSONLRuns(ctx, j); err != nil {
		_ = db.Close()
		return nil, err
	}
	return j, nil
}

// dsn builds the connection string. Every pragma is a DSN parameter rather
// than a statement run after Open, so each connection the pool creates gets
// the same settings — a pragma executed on one connection does not reach the
// next one.
func (j *SQLiteJournal) dsn() (string, error) {
	abs, err := filepath.Abs(j.path)
	if err != nil {
		return "", fmt.Errorf("bonnie: resolve journal path %s: %w", j.path, err)
	}

	sync := "full"
	if j.policy == FsyncRelaxed {
		sync = "normal"
	}
	q := url.Values{}
	// Every transaction this journal opens is a write, so BEGIN IMMEDIATE
	// takes the write lock up front instead of failing to upgrade later.
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous("+sync+")")
	// A writer from another process holds the lock for the length of one
	// commit. Waiting is right; failing is not.
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(on)")

	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}
	return u.String(), nil
}

// migrate creates the schema and refuses a store from the future.
func (j *SQLiteJournal) migrate(ctx context.Context) error {
	if _, err := j.db.ExecContext(ctx, sqliteSchema); err != nil {
		return fmt.Errorf("bonnie: create journal schema in %s: %w", j.path, err)
	}

	var version int
	err := j.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := j.db.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES ('schema_version', ?)`,
			fmt.Sprint(journalSchemaVersion),
		); err != nil {
			return fmt.Errorf("bonnie: record journal schema version: %w", err)
		}
	case err != nil:
		return fmt.Errorf("bonnie: read journal schema version from %s: %w", j.path, err)
	case version > journalSchemaVersion:
		return fmt.Errorf("%w: %s is version %d, this build understands %d",
			ErrJournalSchema, j.path, version, journalSchemaVersion)
	}
	return nil
}

// Root returns the directory the journal is rooted at.
func (j *SQLiteJournal) Root() string { return j.root }

// Path returns the database file the journal writes to.
func (j *SQLiteJournal) Path() string { return j.path }

// validRunID rejects any ID that is not safe to carry in a file name, a URL
// path segment, or a log line. The database itself would take any string;
// the restriction is about everything downstream of it, and about keeping
// two IDs from looking like one after a round trip through a path.
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

// write runs fn inside one write transaction and commits it.
func (j *SQLiteJournal) write(ctx context.Context, fn func(*sql.Tx) error) error {
	j.writeMu.Lock()
	defer j.writeMu.Unlock()

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bonnie: begin journal write: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bonnie: commit journal write: %w", err)
	}
	return nil
}

// nextSeq reads the sequence number the next record of a run must take.
// It runs inside the write transaction, so no other writer can take it.
func nextSeq(ctx context.Context, tx *sql.Tx, runID string) (int, error) {
	var seq int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM records WHERE run_id = ?`, runID,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("bonnie: next sequence for run %s: %w", runID, err)
	}
	return seq, nil
}

// insertRecord writes one record at the sequence number it already carries.
//
// A payload that is not valid JSON is refused. [Record.Payload] is the only
// source [Restore] rebuilds a message from, so storing bytes no decoder can
// read would journal a record that is durable and unreplayable at once. The
// JSONL journal caught this for free, because encoding the record encoded the
// payload with it; a blob column would take anything, so the check is
// explicit here.
func insertRecord(ctx context.Context, tx *sql.Tx, rec Record) error {
	var payload any
	if len(rec.Payload) > 0 {
		if !json.Valid(rec.Payload) {
			return fmt.Errorf("bonnie: record %d of run %s carries a payload that is not valid JSON",
				rec.Seq, rec.RunID)
		}
		payload = []byte(rec.Payload)
	}
	_, err := tx.ExecContext(ctx, insertRecordSQL,
		rec.RunID, rec.Seq, string(rec.Kind), rec.Timestamp.UTC().Format(time.RFC3339Nano),
		rec.EntryID, rec.ParentID, rec.Role, rec.ExtType, rec.Text, string(rec.State),
		payload,
	)
	if err != nil {
		return fmt.Errorf("bonnie: write record %d of run %s: %w", rec.Seq, rec.RunID, err)
	}
	return nil
}

// noteRunState moves the run's state in the same transaction as the record
// that documents it. A run's first record makes it pending; a [RecordState]
// record moves it; anything else leaves a known state alone.
func noteRunState(ctx context.Context, tx *sql.Tx, rec Record) error {
	stmt := `INSERT INTO runs (run_id, state) VALUES (?, ?) ON CONFLICT(run_id) DO NOTHING`
	state := string(RunPending)
	if rec.Kind == RecordState {
		stmt = `INSERT INTO runs (run_id, state) VALUES (?, ?)
		        ON CONFLICT(run_id) DO UPDATE SET state = excluded.state`
		state = string(rec.State)
	}
	if _, err := tx.ExecContext(ctx, stmt, rec.RunID, state); err != nil {
		return fmt.Errorf("bonnie: record state of run %s: %w", rec.RunID, err)
	}
	return nil
}

// Append implements [Journal].
func (j *SQLiteJournal) Append(ctx context.Context, rec Record) (int, error) {
	if err := validRunID(rec.RunID); err != nil {
		return 0, err
	}
	var seq int
	err := j.write(ctx, func(tx *sql.Tx) error {
		var err error
		if seq, err = nextSeq(ctx, tx, rec.RunID); err != nil {
			return err
		}
		rec.Seq = seq
		if err := insertRecord(ctx, tx, rec); err != nil {
			return err
		}
		return noteRunState(ctx, tx, rec)
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// AppendStep implements [StepJournal]. Every record of the step is written in
// one transaction, so the step is either wholly durable or wholly absent.
//
// This is the property the JSONL journal could only approximate. It buffered
// the step into one Write and fsynced once, which left a short write able to
// land a prefix of the step on disk. A transaction has no prefix: a crash
// before the commit leaves the write-ahead log unclaimed and the step gone.
func (j *SQLiteJournal) AppendStep(ctx context.Context, recs []Record) ([]int, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	// One step belongs to one run; a mixed batch would no longer be a unit
	// that a single run's replay can see whole.
	for i := range recs {
		if recs[i].RunID != recs[0].RunID {
			return nil, fmt.Errorf("bonnie: append step: record %d is for run %q, not %q",
				i+1, recs[i].RunID, recs[0].RunID)
		}
	}
	if err := validRunID(recs[0].RunID); err != nil {
		return nil, err
	}

	seqs := make([]int, len(recs))
	err := j.write(ctx, func(tx *sql.Tx) error {
		base, err := nextSeq(ctx, tx, recs[0].RunID)
		if err != nil {
			return err
		}
		for i := range recs {
			recs[i].Seq = base + i
			seqs[i] = recs[i].Seq
			if err := insertRecord(ctx, tx, recs[i]); err != nil {
				return err
			}
			if err := noteRunState(ctx, tx, recs[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return seqs, nil
}

// Replay implements [Journal].
func (j *SQLiteJournal) Replay(ctx context.Context, runID string) ([]Record, error) {
	if err := validRunID(runID); err != nil {
		return nil, err
	}
	rows, err := j.db.QueryContext(ctx, selectRecordSQL+` WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("bonnie: replay run %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bonnie: replay run %s: %w", runID, err)
	}
	if len(out) == 0 {
		return nil, ErrRunNotFound
	}
	return out, nil
}

// scanRecord reads one row into a [Record].
func scanRecord(rows *sql.Rows) (Record, error) {
	var (
		rec     Record
		kind    string
		stamp   string
		state   string
		payload []byte
	)
	if err := rows.Scan(&rec.RunID, &rec.Seq, &kind, &stamp, &rec.EntryID, &rec.ParentID,
		&rec.Role, &rec.ExtType, &rec.Text, &state, &payload); err != nil {
		return rec, fmt.Errorf("bonnie: read journal record: %w", err)
	}
	rec.Kind = RecordKind(kind)
	rec.State = RunState(state)
	if len(payload) > 0 {
		rec.Payload = json.RawMessage(payload)
	}
	ts, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return rec, fmt.Errorf("bonnie: record %d of run %s has an unreadable timestamp %q: %w",
			rec.Seq, rec.RunID, stamp, err)
	}
	rec.Timestamp = ts
	return rec, nil
}

// Checkpoint implements [Journal]. The state move and the record that
// documents it happen in one transaction, so the two can never disagree.
func (j *SQLiteJournal) Checkpoint(ctx context.Context, runID string, state RunState) error {
	_, err := j.Append(ctx, Record{
		RunID: runID, Kind: RecordState, State: state, Timestamp: now(),
	})
	return err
}

// State implements [Journal].
func (j *SQLiteJournal) State(ctx context.Context, runID string) (RunState, error) {
	if err := validRunID(runID); err != nil {
		return "", err
	}
	var state string
	err := j.db.QueryRowContext(ctx, `SELECT state FROM runs WHERE run_id = ?`, runID).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrRunNotFound
	case err != nil:
		return "", fmt.Errorf("bonnie: read state of run %s: %w", runID, err)
	}
	return RunState(state), nil
}

// Runs implements [Journal]. It reads the runs table, so it sees runs written
// by an earlier process and by a concurrent one.
func (j *SQLiteJournal) Runs(ctx context.Context, state RunState) ([]string, error) {
	query := `SELECT run_id FROM runs ORDER BY run_id`
	var args []any
	if state != "" {
		query = `SELECT run_id FROM runs WHERE state = ? ORDER BY run_id`
		args = append(args, string(state))
	}
	rows, err := j.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("bonnie: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("bonnie: list runs: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bonnie: list runs: %w", err)
	}
	return out, nil
}

// Position implements [Positioner].
func (j *SQLiteJournal) Position(ctx context.Context, runID string) (int, error) {
	if err := validRunID(runID); err != nil {
		return 0, err
	}
	var seq int
	if err := j.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM records WHERE run_id = ?`, runID,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("bonnie: read position of run %s: %w", runID, err)
	}
	return seq, nil
}

// Persisted implements [Journal].
func (j *SQLiteJournal) Persisted() bool { return true }

// Close implements [Journal]. It closes the database; unlike the JSONL
// journal it replaced, a closed SQLite journal is not usable again — open a
// new one over the same directory, which is also what a restarted process
// does.
func (j *SQLiteJournal) Close() error {
	if err := j.db.Close(); err != nil {
		return fmt.Errorf("bonnie: close journal: %w", err)
	}
	return nil
}
