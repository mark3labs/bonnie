package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newSeedDir writes a seed tree and returns its path. .gitkeep is included
// because the scaffold writes it, and it must never reach a workspace.
func newSeedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"hello.txt":          "hello from the seed\n",
		"notes/todo.md":      "- write the report\n",
		".gitkeep":           "",
		"notes/.gitkeep":     "",
		"data/blob-irony.md": "# headed\n\ncontent\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSeededMirrorsTheSeedTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seed := newSeedDir(t)
	root := t.TempDir()

	p := Seeded(Local(WithLocalRoot(root), WithLocalCleanup()), seed)
	sb, err := p.Open(ctx, "run-seed-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sb.Close() }()

	for _, c := range []struct{ path, want string }{
		{"/workspace/hello.txt", "hello from the seed\n"},
		{"/workspace/notes/todo.md", "- write the report\n"},
		{"/workspace/data/blob-irony.md", "# headed\n\ncontent\n"},
	} {
		got, err := sb.ReadFile(ctx, c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if string(got) != c.want {
			t.Fatalf("%s = %q, want %q", c.path, got, c.want)
		}
	}

	// .gitkeep is scaffold bookkeeping, never seed content.
	if _, err := sb.ReadFile(ctx, "/workspace/.gitkeep"); !errors.Is(err, ErrNotFound) {
		t.Fatalf(".gitkeep was seeded: %v", err)
	}
	if _, err := sb.ReadFile(ctx, "/workspace/notes/.gitkeep"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nested .gitkeep was seeded: %v", err)
	}
}

// A reattach is a resume, not a reset. Every edit the model made must
// survive the reopen; only a file the model deleted comes back.
func TestSeededReattachNeverRevertsEdits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seed := newSeedDir(t)
	root := t.TempDir()
	inner := Local(WithLocalRoot(root), WithLocalCleanup())
	p := Seeded(inner, seed)

	runID := "run-seed-resume"
	sb, err := p.Open(ctx, runID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sb.WriteFile(ctx, "/workspace/hello.txt", []byte("the model rewrote this\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A fresh provider instance is a second process in spirit: only the
	// directories are shared.
	p2 := Seeded(Local(WithLocalRoot(root), WithLocalCleanup()), seed)
	sb2, err := p2.Open(ctx, runID)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = sb2.Close() }()

	got, err := sb2.ReadFile(ctx, "/workspace/hello.txt")
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if string(got) != "the model rewrote this\n" {
		t.Fatalf("the seed reverted a model edit: %q", got)
	}

	// The other seed files still arrive; the skip is per file.
	if _, err := sb2.ReadFile(ctx, "/workspace/notes/todo.md"); err != nil {
		t.Fatalf("seed files other than the edited one vanished: %v", err)
	}
}

// A failed seed fails the open. The first tool call reports it and the model
// retries; nothing is silent.
func TestSeededFailsOpenWhenTheSeedFails(t *testing.T) {
	t.Parallel()
	p := Seeded(Local(), filepath.Join(t.TempDir(), "missing"))
	if _, err := p.Open(context.Background(), "run-seed-fail"); err == nil {
		t.Fatal("want an error when the seed directory does not exist")
	}
}

// The wrapper must not break the optional provider interfaces, or a seeded
// host would lose network enforcement and sandbox reclamation.
func TestSeededForwardsOptionalInterfaces(t *testing.T) {
	t.Parallel()

	t.Run("Networked is forwarded", func(t *testing.T) {
		t.Parallel()
		p := Seeded(Docker(), t.TempDir())
		n, ok := p.(Networked)
		if !ok {
			t.Fatal("the wrapper dropped the Networked interface")
		}
		if err := n.SetNetworkPolicy(NetworkPolicy{Mode: NetworkDenyAll}); err != nil {
			// Docker may be unavailable here; the assertion above is the
			// point of this subtest.
			t.Skipf("docker not usable: %v", err)
		}
	})

	t.Run("Networked refusal survives the wrapper", func(t *testing.T) {
		t.Parallel()
		p := Seeded(Local(), t.TempDir())
		n, ok := p.(Networked)
		if !ok {
			t.Fatal("the wrapper dropped the Networked interface")
		}
		// Local cannot control egress; the wrapper must refuse, not pretend.
		err := n.SetNetworkPolicy(NetworkPolicy{Mode: NetworkDenyAll})
		if !errors.Is(err, ErrPolicyUnsupported) {
			t.Fatalf("err = %v, want ErrPolicyUnsupported", err)
		}
	})

	t.Run("ExistenceChecker is forwarded", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		p := Seeded(Local(WithLocalRoot(t.TempDir())), t.TempDir())
		ec, ok := p.(ExistenceChecker)
		if !ok {
			t.Fatal("the wrapper dropped the ExistenceChecker interface")
		}
		if _, err := p.Open(ctx, "run-seed-fwd"); err != nil {
			t.Fatalf("Open: %v", err)
		}
		exists, err := ec.SandboxExists(ctx, "run-seed-fwd")
		if err != nil {
			t.Fatalf("SandboxExists: %v", err)
		}
		if !exists {
			t.Fatal("a local workspace exists once opened, but the check said no")
		}
	})

	t.Run("RunDeleter is forwarded", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		p := Seeded(Local(WithLocalRoot(root), WithLocalCleanup()), t.TempDir())
		rd, ok := p.(RunDeleter)
		if !ok {
			t.Fatal("the wrapper dropped the RunDeleter interface")
		}
		if _, err := p.Open(context.Background(), "run-seed-del"); err != nil {
			t.Fatalf("Open: %v", err)
		}
		deleted, err := rd.DeleteRun(context.Background(), "run-seed-del")
		if err != nil || !deleted {
			t.Fatalf("DeleteRun = %v, %v", deleted, err)
		}
		if _, err := os.Stat(filepath.Join(root, safeName("", "run-seed-del"))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the workspace survived the delete: %v", err)
		}
	})
}

// An empty seed directory is a no-op, and the scaffold's fresh workspace is
// exactly that.
func TestSeededEmptyDirectoryIsANoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seed := t.TempDir()
	if err := os.WriteFile(filepath.Join(seed, gitkeep), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := Seeded(Local(WithLocalRoot(t.TempDir())), seed)
	sb, err := p.Open(ctx, "run-seed-empty")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sb.Close() }()
	entries, err := os.ReadDir(filepath.Join(sb.(*localSandbox).dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an empty seed wrote files: %v", entries)
	}
}

// TestSeededForwardsTheWorkingDirectory pins the gap that let a fixed defect
// reach a live agent anyway.
//
// run.go wraps the DEFAULT provider in Seeded, so the wrapper sits between the
// agent and the only provider that can report a host working directory. The
// wrapper did not forward WorkingDir, the prompt fell back to /workspace —
// a directory that does not exist under a host-mapped backend — and a live
// model reported the disagreement: "my workspace is not actually /workspace".
//
// The unit guard passed throughout, because it tested the provider rather
// than the provider as it is actually assembled.
func TestSeededForwardsTheWorkingDirectory(t *testing.T) {
	t.Parallel()

	inner := Landlock(WithLandlockRoot(t.TempDir()))
	wrapped := Seeded(inner, t.TempDir())

	r, ok := wrapped.(WorkingDirReporter)
	if !ok {
		t.Fatal("Seeded drops WorkingDirReporter: the prompt would name a " +
			"directory the tools do not use")
	}
	want := inner.WorkingDir("run-abc")
	if got := r.WorkingDir("run-abc"); got != want {
		t.Fatalf("Seeded reports %q, the backend runs at %q", got, want)
	}

	// And the assembled value must be what the prompt receives.
	if got := promptWorkingDir(wrapped, "run-abc"); got != want {
		t.Fatalf("the prompt would say %q, the tools use %q", got, want)
	}
}

// TestSeededOverAGuestBackendStillReportsWorkspace: a wrapper that forwards
// must not invent a path for a backend that really does run at Workspace.
func TestSeededOverAGuestBackendStillReportsWorkspace(t *testing.T) {
	t.Parallel()
	wrapped := Seeded(&stubProvider{}, t.TempDir())
	if got := promptWorkingDir(wrapped, "run-abc"); got != Workspace {
		t.Fatalf("prompt working directory = %q, want %q", got, Workspace)
	}
}
