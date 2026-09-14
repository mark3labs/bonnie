package runtime

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxRecordLine caps one line of a legacy JSONL journal. Tool arguments are
// the only records that grow, and the model's own output limit bounds them
// long before this.
const maxRecordLine = 64 << 20

// legacyRunsDir is where BONNIE's original journal kept one JSONL file per
// run, and legacyImportedSuffix is what an imported file is renamed to.
const (
	legacyRunsDir        = "runs"
	legacySuffix         = ".jsonl"
	legacyLockSuffix     = ".lock"
	legacyImportedSuffix = ".imported"
)

// importJSONLRuns moves every run from BONNIE's original JSONL journal into
// the database, once, when [OpenSQLiteJournal] finds one beside it.
//
// The import exists because the JSONL layout shipped in v0.1.0 and every run
// created by it is on a disk somewhere. Dropping the format without a path
// forward would strand those runs silently, which is the one outcome a
// durability layer may not produce.
//
// Three properties make it safe to run on every open:
//
//   - It destroys nothing. An imported file is renamed to
//     "<run>.jsonl.imported" and left in place, so an operator can still read
//     it and, if the import were ever wrong, still has the original.
//   - It is idempotent. A run that already has records in the database is
//     skipped, so a crash between the insert and the rename costs a second
//     read and nothing else.
//   - It preserves sequence numbers. Records keep the Seq they were written
//     with, because an [Event] anchored to a journal position must mean the
//     same thing after the import as before it.
//
// A file this process cannot parse is a damaged journal, not a torn write,
// and it fails the open with the path named. The alternative — skipping it —
// would present an operator with a journal that silently lost a run.
func importJSONLRuns(ctx context.Context, j *SQLiteJournal) error {
	dir := filepath.Join(j.root, legacyRunsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bonnie: read legacy journal %s: %w", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), legacySuffix) {
			continue
		}
		runID := strings.TrimSuffix(e.Name(), legacySuffix)
		if err := validRunID(runID); err != nil {
			return fmt.Errorf("bonnie: import legacy run from %s: %w",
				filepath.Join(dir, e.Name()), err)
		}
		path := filepath.Join(dir, e.Name())
		recs, err := readJSONLRecords(path)
		if err != nil {
			return err
		}
		if err := j.importRun(ctx, runID, recs); err != nil {
			return err
		}
		if err := os.Rename(path, path+legacyImportedSuffix); err != nil {
			return fmt.Errorf("bonnie: retire legacy run file %s: %w", path, err)
		}
		// The lock file was the JSONL journal's ownership mechanism. SQLite
		// serialises writers itself, so the file is now noise.
		if err := os.Remove(filepath.Join(dir, runID+legacyLockSuffix)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("bonnie: remove legacy lock file for run %s: %w", runID, err)
		}
	}
	return nil
}

// importRun writes one legacy run into the database in a single transaction,
// keeping the sequence numbers the records already carry. A run that is
// already present is left alone.
func (j *SQLiteJournal) importRun(ctx context.Context, runID string, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	return j.write(ctx, func(tx *sql.Tx) error {
		var have int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM records WHERE run_id = ?`, runID,
		).Scan(&have); err != nil {
			return fmt.Errorf("bonnie: check run %s before import: %w", runID, err)
		}
		if have > 0 {
			return nil
		}

		state := RunPending
		for _, rec := range recs {
			rec.RunID = runID
			if err := insertRecord(ctx, tx, rec); err != nil {
				return fmt.Errorf("bonnie: import legacy run %s: %w", runID, err)
			}
			if rec.Kind == RecordState {
				state = rec.State
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runs (run_id, state) VALUES (?, ?)
			 ON CONFLICT(run_id) DO UPDATE SET state = excluded.state`,
			runID, string(state),
		); err != nil {
			return fmt.Errorf("bonnie: import state of legacy run %s: %w", runID, err)
		}
		return nil
	})
}

// readJSONLRecords parses a legacy run file.
//
// A truncated final line is a torn write, not a corrupt journal: the process
// died mid-append. It is dropped, and the conversation-level consequence is
// handled by the repair in [Restore]. A malformed line anywhere else is real
// corruption and is reported.
func readJSONLRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("bonnie: read legacy journal: %w", err)
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
