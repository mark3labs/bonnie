package discoveryio

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNoFollowAndBoundedReads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large"), make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, p := range []string{"link/secret", "leaf", "fifo", "../secret", "/secret", "a/../large", "large"} {
		if _, err := root.Read(context.Background(), p, 8); err == nil {
			t.Errorf("accepted %s", p)
		}
	}
	for range 2 {
		entries, _, err := root.List(context.Background(), ".", 100)
		if err != nil || len(entries) != 4 {
			t.Fatalf("enumeration = %+v, %v", entries, err)
		}
	}
}
func TestRootReplacementDoesNotRedirect(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := root.Read(context.Background(), "file", 100)
	if err != nil || string(file.Data) != "original" {
		t.Fatalf("read=%s %v", file.Data, err)
	}
}

func TestSymlinkSwapNeverReadsOutside(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("OUTSIDE"), 0600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "inside")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, "file"), []byte("INSIDE"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = os.Rename(inside, inside+"-parked")
			_ = os.Symlink(outside, inside)
			_ = os.Remove(inside)
			_ = os.Rename(inside+"-parked", inside)
		}
	}()
	defer func() { cancel(); <-done }()
	for range 200 {
		file, err := root.Read(context.Background(), "inside/file", 32)
		if err == nil && string(file.Data) != "INSIDE" {
			t.Fatalf("escaped root: %s", file.Data)
		}
	}
}
