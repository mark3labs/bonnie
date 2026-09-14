package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// writeLegacyRun lays down a run in the JSONL format BONNIE shipped in
// v0.1.0, the way an older binary would have left it on disk. Records are
// written verbatim: they already carry the sequence numbers the old journal
// assigned, and the import has to keep them.
func writeLegacyRun(t *testing.T, root, runID string, recs []Record) string {
	t.Helper()
	dir := filepath.Join(root, legacyRunsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, runID+legacySuffix)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The old journal left a lock file beside every run it had written.
	if err := os.WriteFile(filepath.Join(dir, runID+legacyLockSuffix), nil, 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	return path
}

// legacyMessages returns the records a real [Session] produces for a run, so
// the import is tested against the shape the old journal actually held —
// entry IDs, parent links, and lossless message payloads included — rather
// than a hand-written approximation that [Restore] would ignore.
func legacyMessages(t *testing.T, runID string, msgs ...kit.LLMMessage) []Record {
	t.Helper()
	mem := NewMemoryJournal()
	s := NewSession(runID, mem)
	for _, msg := range msgs {
		mustAppend(t, s, msg)
	}
	recs, err := mem.Replay(context.Background(), runID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return recs
}

// TestLegacyRunsAreImportedLosslessly is the upgrade claim: a run written by
// the JSONL journal is resumable after the store becomes SQLite. It must keep
// its tool calls (SPEC §4.1) and its sequence numbers, because an event
// anchored to a journal position has to mean the same thing after the import.
func TestLegacyRunsAreImportedLosslessly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := context.Background()

	recs := legacyMessages(t, "old",
		user("deploy the app"),
		toolCall("Deploying.", "c1", "deploy", `{"region":"eu"}`),
		toolResult("c1", "ok"),
	)
	recs = append(recs, Record{
		RunID: "old", Seq: len(recs) + 1, Kind: RecordState,
		State: RunWaiting, Timestamp: now(),
	})
	path := writeLegacyRun(t, root, "old", recs)

	j, err := OpenSQLiteJournal(root)
	if err != nil {
		t.Fatalf("OpenSQLiteJournal: %v", err)
	}
	defer func() { _ = j.Close() }()

	if state, err := j.State(ctx, "old"); err != nil || state != RunWaiting {
		t.Fatalf("State = %q, %v, want waiting", state, err)
	}
	imported, err := j.Replay(ctx, "old")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(imported) != len(recs) {
		t.Fatalf("imported %d records, want %d", len(imported), len(recs))
	}
	for i, rec := range imported {
		if rec.Seq != recs[i].Seq || rec.EntryID != recs[i].EntryID {
			t.Fatalf("record %d changed across the import: got %d/%q, want %d/%q",
				i, rec.Seq, rec.EntryID, recs[i].Seq, recs[i].EntryID)
		}
	}

	// The run is resumable, tool call intact.
	restored, err := Restore(ctx, "old", j)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	msgs := restored.GetMessages()
	if len(msgs) != 3 {
		t.Fatalf("restored %d messages, want 3", len(msgs))
	}
	assertNoOrphan(t, msgs)
	part, ok := msgs[1].Content[1].(kit.LLMToolCallPart)
	if !ok {
		t.Fatalf("part is %T, want a tool call", msgs[1].Content[1])
	}
	if part.ToolCallID != "c1" || part.Input != `{"region":"eu"}` {
		t.Fatalf("the import lost tool-call detail: %+v", part)
	}

	// The original is retired, never deleted, and the lock file it needed
	// is gone.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat %s: the imported file must be renamed, err = %v", path, err)
	}
	if _, err := os.Stat(path + legacyImportedSuffix); err != nil {
		t.Fatalf("the original was not kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, legacyRunsDir, "old"+legacyLockSuffix)); !os.IsNotExist(err) {
		t.Fatal("the legacy lock file survived the import")
	}
}

// TestLegacyImportIsIdempotent covers the crash between the insert and the
// rename: a second open must not double the run.
func TestLegacyImportIsIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := context.Background()

	path := writeLegacyRun(t, root, "twice", legacyMessages(t, "twice",
		user("one"), user("two")))

	j, err := OpenSQLiteJournal(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Put the file back, as a crash before the rename would have left it.
	if err := os.Rename(path+legacyImportedSuffix, path); err != nil {
		t.Fatalf("restore file: %v", err)
	}

	next, err := OpenSQLiteJournal(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = next.Close() }()

	recs, err := next.Replay(ctx, "twice")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("replayed %d records after a second import, want 2", len(recs))
	}
}

// TestLegacyImportContinuesTheSequence pins that an imported run keeps
// growing where it stopped, rather than restarting and colliding.
func TestLegacyImportContinuesTheSequence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := context.Background()

	writeLegacyRun(t, root, "continued", legacyMessages(t, "continued",
		user("one"), user("two")))

	j, err := OpenSQLiteJournal(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = j.Close() }()

	seq, err := j.Append(ctx, Record{RunID: "continued", Kind: RecordMessage, Text: "three", Timestamp: now()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq != 3 {
		t.Fatalf("seq after import = %d, want 3", seq)
	}
}

// TestLegacyImportRefusesACorruptFile is the other half of the torn-write
// rule: damage that is not at the tail is real corruption, and the open fails
// with the path named rather than quietly losing the run.
func TestLegacyImportRefusesACorruptFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	path := writeLegacyRun(t, root, "bad", legacyMessages(t, "bad",
		user("one"), user("two")))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines[0] = "{ this is not json"
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := OpenSQLiteJournal(root); err == nil {
		t.Fatal("want an error for a corrupt legacy line that is not the last")
	} else if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not name the file: %v", err)
	}
}

// TestLegacyImportDropsATornLastLine keeps the JSONL journal's crash
// semantics: a half-written final record is a torn write, not corruption.
func TestLegacyImportDropsATornLastLine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := context.Background()

	path := writeLegacyRun(t, root, "torn", legacyMessages(t, "torn",
		user("one"), user("two")))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(path, append(data, []byte(`{"seq":3,"run_id":"torn","ki`)...), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	j, err := OpenSQLiteJournal(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = j.Close() }()

	recs, err := j.Replay(ctx, "torn")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("imported %d records, want the 2 whole ones", len(recs))
	}
}
