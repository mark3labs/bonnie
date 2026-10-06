package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalSharedWorkspacePersistsAndRejectsOverlap(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "shared")
	p := Local(WithLocalRoot(root), WithLocalSharedWorkspace(), WithLocalCleanup())
	first, err := p.Open(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(context.Background(), "two"); err == nil {
		t.Fatal("overlapping shared run accepted")
	}
	if err := first.WriteFile(context.Background(), "note.txt", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := first.(Deleter).Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("shared data = %q, %v", got, err)
	}
	second, err := p.Open(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.ReadFile(context.Background(), "note.txt")
	if err != nil || string(got) != "keep" {
		t.Fatalf("reopened data = %q, %v", got, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(context.Background(), "three"); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
