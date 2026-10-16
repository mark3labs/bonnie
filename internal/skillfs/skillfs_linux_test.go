package skillfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNoFollowBoundedReads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "filelink")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "valid"), []byte("valid"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for _, p := range []string{"../secret", "/etc/passwd", "link/secret", "filelink", "pipe", "."} {
		if _, err := r.Read(context.Background(), p, 100); err == nil {
			t.Errorf("read accepted %q", p)
		}
	}
	if _, _, err := r.List(context.Background(), "link", 10); err == nil {
		t.Fatal("directory link followed")
	}
	if _, err := r.Read(context.Background(), "valid", 4); err == nil {
		t.Fatal("read exceeded bound")
	}
	f, err := r.Read(context.Background(), "valid", 5)
	if err != nil || string(f.Data) != "valid" || f.Identity == "" {
		t.Fatalf("read=%+v %v", f, err)
	}
	// Replacement of the root name cannot redirect this reader.
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(dir+"-old", dir) })
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) })
	if _, err := r.Read(context.Background(), "secret", 100); err == nil {
		t.Fatal("root replacement escaped")
	}
	if f, err := r.Read(context.Background(), "valid", 5); err != nil || string(f.Data) != "valid" {
		t.Fatalf("pinned read=%+v %v", f, err)
	}
}

func TestConcurrentSymlinkReplacement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for range 200 {
		target := filepath.Join(dir, "swap")
		if err := os.Symlink(outside, target); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Read(context.Background(), "swap/file", 100); err == nil {
			t.Fatal("symlink escaped")
		}
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
	}
}
