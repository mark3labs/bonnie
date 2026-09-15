package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// stubProvider exists so the lifecycle seams can be tested on every machine.
// The real backends are covered by the conformance suite; this one answers
// the questions the seams ask without a daemon, a CLI, or KVM.
type stubProvider struct {
	mu        sync.Mutex
	name      string
	opened    int
	exists    bool
	checkErr  error
	sandboxes map[string]*stubSandbox
}

func newStubProvider(name string) *stubProvider {
	return &stubProvider{name: name, exists: true, sandboxes: map[string]*stubSandbox{}}
}

var (
	_ Provider         = (*stubProvider)(nil)
	_ ExistenceChecker = (*stubProvider)(nil)
	_ RunDeleter       = (*stubProvider)(nil)
)

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) Available(context.Context) error { return nil }

func (p *stubProvider) Open(_ context.Context, runID string) (Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opened++
	sb := &stubSandbox{id: runID, provider: p}
	p.sandboxes[runID] = sb
	return sb, nil
}

func (p *stubProvider) SandboxExists(_ context.Context, runID string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.checkErr != nil {
		return false, p.checkErr
	}
	return p.exists, nil
}

func (p *stubProvider) DeleteRun(_ context.Context, runID string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exists {
		return false, nil
	}
	p.exists = false
	return true, nil
}

type stubSandbox struct {
	id       string
	provider *stubProvider
}

func (s *stubSandbox) ID() string { return s.id }

func (s *stubSandbox) Exec(context.Context, Command) (*Result, error) {
	return &Result{}, nil
}
func (s *stubSandbox) ReadFile(context.Context, string) ([]byte, error) {
	return nil, errors.New("stub")
}
func (s *stubSandbox) WriteFile(context.Context, string, []byte) error { return nil }
func (s *stubSandbox) Stop(context.Context) error                      { return nil }
func (s *stubSandbox) Close() error {
	return nil
}

var _ Sandbox = (*stubSandbox)(nil)

// TestLazyOpenerJournalsTheOpen covers the record that makes a run's
// workspace findable: the first open writes one sandbox record, and the
// once-semantics mean the second open does not write another.
func TestLazyOpenerJournalsTheOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := runtime.NewMemoryJournal()
	s := runtime.NewSession("opener-record", j)

	p := newStubProvider("stub")
	open := LazyOpener(p, s)

	sb, err := open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sb.ID() != "opener-record" {
		t.Fatalf("sandbox ID = %q, want the run ID", sb.ID())
	}

	recs, err := j.Replay(ctx, "opener-record")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 1 || recs[0].Kind != runtime.RecordSandbox {
		t.Fatalf("journal holds %d records, want exactly the one sandbox record", len(recs))
	}
	if !strings.Contains(recs[0].Text, "backend stub") || !strings.Contains(recs[0].Text, sb.ID()) {
		t.Fatalf("record text %q does not name the backend and the sandbox", recs[0].Text)
	}

	// A second open is the same sandbox: one record, no duplicates.
	if _, err := open(ctx); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if recs2, _ := j.Replay(ctx, "opener-record"); len(recs2) != 1 {
		t.Fatalf("%d records after a second open, want 1", len(recs2))
	}
}

// TestLazyOpenerRecordFailureFailsOneCall pins the failure shape: a journal
// that refuses the record fails the tool call that opened the sandbox — the
// model sees the error and retries — and the next call lands the record and
// works. A bookkeeping failure must be visible, and it must not be permanent.
func TestLazyOpenerRecordFailureFailsOneCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fail := &failingOnceJournal{MemoryJournal: runtime.NewMemoryJournal(), fail: 1}
	s := runtime.NewSession("opener-record-fail", fail)
	p := newStubProvider("stub")
	open := LazyOpener(p, s)

	if _, err := open(ctx); err == nil {
		t.Fatal("a failed record must fail the call that opened the sandbox")
	}
	if _, err := open(ctx); err != nil {
		t.Fatalf("the call after a failed record must retry and work: %v", err)
	}
	// Exactly one record landed, once the journal took it.
	recs, err := fail.Replay(ctx, "opener-record-fail")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 1 || recs[0].Kind != runtime.RecordSandbox {
		t.Fatalf("journal holds %d records, want the one sandbox record", len(recs))
	}
}

// failingOnceJournal refuses the first N writes, then works — what a
// transient journal outage looks like from the inside.
type failingOnceJournal struct {
	*runtime.MemoryJournal
	fail int
}

var _ runtime.Journal = (*failingOnceJournal)(nil)

func (f *failingOnceJournal) Append(ctx context.Context, rec runtime.Record) (int, error) {
	if f.fail > 0 {
		f.fail--
		return 0, errors.New("journal refuses writes")
	}
	return f.MemoryJournal.Append(ctx, rec)
}

// TestCheckRecordedSandboxNotesTheLoss covers the resume path this feature
// exists for: a run parks, its container is pruned, and the resumed run says
// so in the conversation instead of handing the model an empty workspace in
// silence.
func TestCheckRecordedSandboxNotesTheLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("verified loss", func(t *testing.T) {
		t.Parallel()
		j := runtime.NewMemoryJournal()
		s := runtime.NewSession("loss-verified", j)
		if err := s.RecordSandboxOpen(ctx, "stub", "loss-verified"); err != nil {
			t.Fatalf("RecordSandboxOpen: %v", err)
		}

		p := newStubProvider("stub")
		p.exists = false // the container was pruned while the run was parked

		if err := checkRecordedSandbox(ctx, p, s); err != nil {
			t.Fatalf("checkRecordedSandbox: %v", err)
		}
		msgs := s.GetMessages()
		if len(msgs) == 0 || !strings.Contains(messageTextOf(msgs[len(msgs)-1]), "no longer available") {
			t.Fatalf("the resumed conversation holds no loss note: %v", msgs)
		}
		// The note is the only thing appended: no sandbox opened, nothing
		// else ran.
		if p.opened != 0 {
			t.Fatalf("the check opened a sandbox %d time(s); it must only ask", p.opened)
		}
	})

	t.Run("live workspace is silent", func(t *testing.T) {
		t.Parallel()
		j := runtime.NewMemoryJournal()
		s := runtime.NewSession("loss-live", j)
		if err := s.RecordSandboxOpen(ctx, "stub", "loss-live"); err != nil {
			t.Fatalf("RecordSandboxOpen: %v", err)
		}

		p := newStubProvider("stub")
		if err := checkRecordedSandbox(ctx, p, s); err != nil {
			t.Fatalf("checkRecordedSandbox: %v", err)
		}
		if msgs := s.GetMessages(); len(msgs) != 0 {
			t.Fatalf("a live workspace must not be noted: %v", msgs)
		}
	})

	t.Run("backend mismatch is unverified", func(t *testing.T) {
		t.Parallel()
		j := runtime.NewMemoryJournal()
		s := runtime.NewSession("loss-mismatch", j)
		if err := s.RecordSandboxOpen(ctx, "docker", "loss-mismatch"); err != nil {
			t.Fatalf("RecordSandboxOpen: %v", err)
		}

		p := newStubProvider("stub") // this host now runs a different backend
		if err := checkRecordedSandbox(ctx, p, s); err != nil {
			t.Fatalf("checkRecordedSandbox: %v", err)
		}
		if msgs := s.GetMessages(); len(msgs) == 0 {
			t.Fatal("a backend switch must be noted: the model's files are unreachable")
		}
		// Not verified, so no gone record — only the note.
		_, _, gone, _ := s.LastSandbox()
		if gone {
			t.Fatal("an unverified loss must not write a gone record")
		}
	})

	t.Run("check failure is not fatal", func(t *testing.T) {
		t.Parallel()
		j := runtime.NewMemoryJournal()
		s := runtime.NewSession("loss-checkerr", j)
		if err := s.RecordSandboxOpen(ctx, "stub", "loss-checkerr"); err != nil {
			t.Fatalf("RecordSandboxOpen: %v", err)
		}

		p := newStubProvider("stub")
		p.checkErr = errors.New("backend busy")
		if err := checkRecordedSandbox(ctx, p, s); err != nil {
			t.Fatalf("a failed check must not fail the resume: %v", err)
		}
		if msgs := s.GetMessages(); len(msgs) != 0 {
			t.Fatalf("a failed check must not note a loss it did not see: %v", msgs)
		}
	})
}

func messageTextOf(msg kit.LLMMessage) string {
	var b []string
	for _, part := range msg.Content {
		if t, ok := part.(kit.LLMTextPart); ok {
			b = append(b, t.Text)
		}
	}
	return strings.Join(b, "\n")
}

// TestPromptWorkingDirectoryIsTheToolWorkingDirectory is one assertion over
// both halves of the defect in issue #1, run against every backend.
//
// Kit's system prompt carries an environment block whose working directory is
// Options.SessionDir. The tools run somewhere real. When those disagree the
// model is told the wrong root — and the prompt is the half it believes.
//
// The earlier version of this test compared the prompt against the CONSTANT
// sandbox.Workspace and passed while the defect was live: the landlock and
// local backends map the workspace onto a host directory, so commands run
// there and /workspace does not exist. A live model reported exactly that:
// "my workspace is not actually /workspace". The test now asks the sandbox
// where it really is, with `pwd`, which is the only source that cannot be
// wrong.
func TestPromptWorkingDirectoryIsTheToolWorkingDirectory(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		const runID = "prompt-cwd"
		sb := openSandbox(t, p, runID)
		ctx := testCtx(t)

		// What the prompt will say, from the real option set.
		var o kit.Options
		for _, opt := range sandboxedKitOptions(
			func(context.Context) (Sandbox, error) { return sb, nil },
			promptWorkingDir(p, runID),
		) {
			opt(&o)
		}

		// What the tools actually do.
		res, err := sb.Exec(ctx, Shell("pwd"))
		if err != nil {
			t.Fatalf("Exec(pwd): %v", err)
		}
		actual := strings.TrimSpace(res.Stdout)

		if o.SessionDir != actual {
			t.Fatalf("the prompt says the working directory is %q, the tools "+
				"report %q: the model believes the prompt",
				o.SessionDir, actual)
		}

		// And a relative path must resolve there, so the directory the prompt
		// names is the one a tool call writes into.
		if err := sb.WriteFile(ctx, "cwd-proof.txt", []byte("x")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		seen, err := sb.Exec(ctx, Shell("cat cwd-proof.txt"))
		if err != nil || !seen.OK() {
			t.Fatalf("a relative write did not land in the reported directory: %+v err=%v", seen, err)
		}
	})
}
