package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTempJournal(t *testing.T) *FileJournal {
	t.Helper()
	j, err := OpenFileJournal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func TestFileJournalLayout(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)

	if _, err := j.Append(context.Background(), Record{
		RunID: "run-1", Kind: RecordMessage, Text: "hi", Timestamp: now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	path := filepath.Join(j.Root(), "runs", "run-1.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want 1", len(lines))
	}
	if !strings.HasPrefix(lines[0], "{") || !strings.Contains(lines[0], `"kind":"message"`) {
		t.Fatalf("line is not an inspectable JSON record: %s", lines[0])
	}
}

// TestFileJournalTruncatedLastLine simulates the torn write a power cut leaves:
// the final record is half-written. Replay must drop it, not fail, or one bad
// byte makes the whole run unreadable.
func TestFileJournalTruncatedLastLine(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	for _, text := range []string{"one", "two"} {
		if _, err := j.Append(ctx, Record{RunID: "torn", Kind: RecordMessage, Text: text, Timestamp: now()}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(j.Root(), "runs", "torn.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Chop the final record in half, newline and all.
	if err := os.WriteFile(path, append(data, []byte(`{"seq":3,"run_id":"torn","ki`)...), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	next, err := OpenFileJournal(j.Root())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = next.Close() }()

	recs, err := next.Replay(ctx, "torn")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("replayed %d records, want the 2 whole ones", len(recs))
	}

	// The next append must continue the sequence, not reuse seq 3's slot in a
	// way that hides the loss.
	seq, err := next.Append(ctx, Record{RunID: "torn", Kind: RecordMessage, Text: "three", Timestamp: now()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq != 3 {
		t.Fatalf("seq after a torn write = %d, want 3", seq)
	}
}

// TestFileJournalRejectsCorruptMiddleLine is the other half: damage that is not
// at the tail is real corruption and must be reported.
func TestFileJournalRejectsCorruptMiddleLine(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	for _, text := range []string{"one", "two"} {
		if _, err := j.Append(ctx, Record{RunID: "bad", Kind: RecordMessage, Text: text, Timestamp: now()}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(j.Root(), "runs", "bad.jsonl")
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines[0] = "{ this is not json"
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	next, err := OpenFileJournal(j.Root())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = next.Close() }()

	if _, err := next.Replay(ctx, "bad"); err == nil {
		t.Fatal("want an error for a corrupt line that is not the last")
	}
}

// TestFileJournalLargePayload guards the scanner buffer. Tool arguments routinely
// exceed the 64 KB bufio default, and a silent truncation there would look like
// journal corruption.
func TestFileJournalLargePayload(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	big := strings.Repeat("x", 200*1024)
	if _, err := j.Append(ctx, Record{
		RunID: "big", Kind: RecordMessage, Text: big, Timestamp: now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	next, err := OpenFileJournal(j.Root())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = next.Close() }()

	recs, err := next.Replay(ctx, "big")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 1 || len(recs[0].Text) != len(big) {
		t.Fatalf("round trip lost data: %d records, %d bytes", len(recs), len(recs[0].Text))
	}
}

func TestFileJournalRejectsUnsafeRunID(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	for _, id := range []string{"", "../escape", "a/b", ".hidden", "space id"} {
		if _, err := j.Append(ctx, Record{RunID: id, Kind: RecordMessage}); !errors.Is(err, ErrInvalidRunID) {
			t.Fatalf("Append(%q) err = %v, want ErrInvalidRunID", id, err)
		}
	}
}

// TestFileJournalRunsSeesForeignRuns proves Runs reads the directory rather
// than a process-local map: a run written by one journal must be listed by
// another.
func TestFileJournalRunsSeesForeignRuns(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	if err := j.Checkpoint(ctx, "alpha", RunWaiting); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	next, err := OpenFileJournal(j.Root())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = next.Close() }()

	waiting, err := next.Runs(ctx, RunWaiting)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(waiting) != 1 || waiting[0] != "alpha" {
		t.Fatalf("Runs(waiting) = %v, want [alpha]", waiting)
	}
}

// TestFileJournalCrashResumeIsProviderValid is the end-to-end durability claim
// for T-003 and T-004 together: a process dies mid-step, a new process opens
// the same directory, and the conversation it gets is one a provider accepts.
func TestFileJournalCrashResumeIsProviderValid(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	s := NewSession("crash", j)
	mustAppend(t, s, user("deploy the app"))
	mustAppend(t, s, toolCall("Deploying.", "c1", "deploy", `{"region":"eu"}`))
	// The tool result never reaches the journal: the process dies here.
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	next, err := OpenFileJournal(j.Root())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = next.Close() }()

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

func TestFileJournalFsyncIntervalPolicy(t *testing.T) {
	t.Parallel()
	j, err := OpenFileJournal(t.TempDir(), WithFsync(FsyncInterval))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	defer func() { _ = j.Close() }()

	if j.policy != FsyncInterval {
		t.Fatalf("policy = %q, want %q", j.policy, FsyncInterval)
	}
	if _, err := j.Append(context.Background(), Record{RunID: "r", Kind: RecordMessage, Text: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
}
