package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func openTempJournal(t *testing.T) *SQLiteJournal {
	t.Helper()
	j, err := OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// TestSQLiteJournalLayout pins where the store lives. An operator who knows
// the path can read a run with the sqlite3 CLI, which is the property the
// JSONL layout gave up when it was replaced.
func TestSQLiteJournalLayout(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)

	if _, err := j.Append(context.Background(), Record{
		RunID: "run-1", Kind: RecordMessage, Text: "hi", Timestamp: now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	want := filepath.Join(j.Root(), DefaultJournalFile)
	if j.Path() != want {
		t.Fatalf("Path() = %q, want %q", j.Path(), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("stat %s: %v", want, err)
	}
}

// TestSQLiteJournalLargePayload guards the round trip a tool call needs. Tool
// arguments routinely run past any convenient buffer size, and a silent
// truncation would look like journal corruption on resume.
func TestSQLiteJournalLargePayload(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	big := strings.Repeat("x", 4<<20)
	if _, err := j.Append(ctx, Record{
		RunID: "big", Kind: RecordMessage, Text: big,
		Payload: []byte(`{"blob":"` + big + `"}`), Timestamp: now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	next := reopenJournal(t, j)
	recs, err := next.Replay(ctx, "big")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 1 || len(recs[0].Text) != len(big) {
		t.Fatalf("round trip lost text: %d records, %d bytes", len(recs), len(recs[0].Text))
	}
	if len(recs[0].Payload) != len(big)+11 {
		t.Fatalf("round trip lost payload: %d bytes", len(recs[0].Payload))
	}
}

// TestSQLiteJournalRoundTripsEveryField is the lossless-replay guard at the
// storage layer. A column that is dropped or swapped here is a tool call that
// vanishes on resume — SPEC §4.1, one level down.
func TestSQLiteJournalRoundTripsEveryField(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	want := Record{
		RunID: "fields", Kind: RecordExtensionData, Timestamp: now(),
		EntryID: "e1", ParentID: "e0", Role: "assistant", ExtType: "memory",
		Text: "display only", State: RunWaiting, Payload: []byte(`{"a":1}`),
	}
	seq, err := j.Append(ctx, want)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	want.Seq = seq

	recs, err := j.Replay(ctx, "fields")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := recs[0]
	if !got.Timestamp.Equal(want.Timestamp) {
		t.Fatalf("timestamp = %v, want %v", got.Timestamp, want.Timestamp)
	}
	got.Timestamp = want.Timestamp
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", got, want)
	}
}

func TestSQLiteJournalRejectsUnsafeRunID(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	for _, id := range []string{"", "../escape", "a/b", ".hidden", "space id"} {
		if _, err := j.Append(ctx, Record{RunID: id, Kind: RecordMessage}); !errors.Is(err, ErrInvalidRunID) {
			t.Fatalf("Append(%q) err = %v, want ErrInvalidRunID", id, err)
		}
		if _, err := j.Replay(ctx, id); !errors.Is(err, ErrInvalidRunID) {
			t.Fatalf("Replay(%q) err = %v, want ErrInvalidRunID", id, err)
		}
	}
}

// TestSQLiteJournalRunsSeesForeignRuns proves Runs reads the store rather
// than process-local memory: a run written by one journal must be listed by
// another.
func TestSQLiteJournalRunsSeesForeignRuns(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	if err := j.Checkpoint(ctx, "alpha", RunWaiting); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	next := reopenJournal(t, j)
	waiting, err := next.Runs(ctx, RunWaiting)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if !slices.Equal(waiting, []string{"alpha"}) {
		t.Fatalf("Runs(waiting) = %v, want [alpha]", waiting)
	}
}

// TestSQLiteJournalCrashResumeIsProviderValid is the end-to-end durability
// claim: a process dies mid-step, a new process opens the same directory, and
// the conversation it gets is one a provider accepts.
func TestSQLiteJournalCrashResumeIsProviderValid(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	s := NewSession("crash", j)
	mustAppend(t, s, user("deploy the app"))
	mustAppend(t, s, toolCall("Deploying.", "c1", "deploy", `{"region":"eu"}`))
	// The tool result never reaches the journal: the process dies here.
	next := reopenJournal(t, j)

	restored, err := Restore(ctx, "crash", next)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	msgs := restored.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("restored %d messages, want the user turn only", len(msgs))
	}
	assertNoOrphan(t, msgs)
}

func TestSQLiteJournalFsyncPolicy(t *testing.T) {
	t.Parallel()
	j, err := OpenSQLiteJournal(t.TempDir(), WithFsync(FsyncRelaxed))
	if err != nil {
		t.Fatalf("OpenSQLiteJournal: %v", err)
	}
	defer func() { _ = j.Close() }()

	if j.policy != FsyncRelaxed {
		t.Fatalf("policy = %q, want %q", j.policy, FsyncRelaxed)
	}
	// The policy is a DSN pragma, so it has to survive reaching a real
	// connection, not just the struct field.
	var mode string
	if err := j.db.QueryRow(`PRAGMA synchronous`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA synchronous: %v", err)
	}
	if mode != "1" {
		t.Fatalf("synchronous = %q, want 1 (NORMAL)", mode)
	}
	if _, err := j.Append(context.Background(), Record{RunID: "r", Kind: RecordMessage, Text: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// TestSQLiteJournalUsesWAL pins the mode the concurrency claims rest on.
// Without WAL a reader blocks a writer, so `bonnie runs show` against a busy
// server would contend with the run it is inspecting.
func TestSQLiteJournalUsesWAL(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)

	var mode string
	if err := j.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

// TestSQLiteJournalAdmitsConcurrentWriters is what replaced the lock file.
//
// The JSONL journal refused a second writer with ErrRunOwnedElsewhere, because
// two writers meant interleaved records and reused sequence numbers. SQLite
// serialises write transactions and the (run_id, seq) primary key makes a
// reused number a constraint violation, so the second writer is now *safe*
// rather than forbidden. This test states that as the contract: two journals
// over one store, both writing one run, produce a dense unbroken sequence and
// lose nothing.
func TestSQLiteJournalAdmitsConcurrentWriters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	const (
		runID  = "shared"
		writes = 25
	)
	first, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer func() { _ = second.Close() }()

	var wg sync.WaitGroup
	for i, j := range []*SQLiteJournal{first, second} {
		wg.Go(func() {
			for n := range writes {
				if _, err := j.Append(ctx, Record{
					RunID: runID, Kind: RecordMessage,
					Text: fmt.Sprintf("w%d-%d", i, n), Timestamp: now(),
				}); err != nil {
					t.Errorf("writer %d Append: %v", i, err)
					return
				}
			}
		})
	}
	wg.Wait()

	recs, err := first.Replay(ctx, runID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 2*writes {
		t.Fatalf("replayed %d records, want %d: a concurrent writer lost records", len(recs), 2*writes)
	}
	for i, rec := range recs {
		if rec.Seq != i+1 {
			t.Fatalf("record %d has seq %d, want %d: sequence numbers collided or skipped", i, rec.Seq, i+1)
		}
	}
	// The second journal sees everything the first wrote. There is no
	// process-local sequence counter left to disagree.
	if pos, err := second.Position(ctx, runID); err != nil || pos != 2*writes {
		t.Fatalf("second journal Position = %d, %v, want %d", pos, err, 2*writes)
	}
}

// TestSQLiteJournalReadsOfUnknownRunsCostNothing keeps the property SPEC
// §4.12 had to fix in the JSONL journal, where a read created a per-run
// handle that was never released. There is no per-run state here, so the
// defect class is gone by construction; this pins the observable half.
func TestSQLiteJournalReadsOfUnknownRunsCostNothing(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	if _, err := j.Append(ctx, Record{
		RunID: "real-run", Kind: RecordMessage, Text: "hi", Timestamp: now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for i := range 500 {
		id := fmt.Sprintf("no-such-run-%d", i)
		if _, err := j.State(ctx, id); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("State(%s) = %v, want ErrRunNotFound", id, err)
		}
		if _, err := j.Replay(ctx, id); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("Replay(%s) = %v, want ErrRunNotFound", id, err)
		}
		if pos, err := j.Position(ctx, id); err != nil || pos != 0 {
			t.Fatalf("Position(%s) = %d, %v, want 0, nil", id, pos, err)
		}
	}

	runs, err := j.Runs(ctx, "")
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if !slices.Equal(runs, []string{"real-run"}) {
		t.Fatalf("Runs = %v: a read of an unknown run created one", runs)
	}
}

// TestSQLiteJournalRefusesANewerSchema is invariant 13 at the storage layer:
// a store this build does not understand is refused by name, not read with
// the wrong shape.
func TestSQLiteJournalRefusesANewerSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	j, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := j.db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		fmt.Sprint(journalSchemaVersion+1)); err != nil {
		t.Fatalf("bump schema version: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := OpenSQLiteJournal(dir); !errors.Is(err, ErrJournalSchema) {
		t.Fatalf("reopen err = %v, want ErrJournalSchema", err)
	}
}

// reopenJournal closes a journal and opens a fresh one over the same
// directory. It is the closest a test in one process gets to a restart.
func reopenJournal(t *testing.T, j *SQLiteJournal) *SQLiteJournal {
	t.Helper()
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	next, err := OpenSQLiteJournal(j.Root())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = next.Close() })
	return next
}
