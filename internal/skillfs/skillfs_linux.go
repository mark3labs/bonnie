//go:build linux

// Package skillfs provides bounded no-follow discovery operations.
package skillfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Entry describes one direct child without following links.
type Entry struct {
	Name string
	Kind string
}

// File contains bounded bytes and an identity for stability checks.
type File struct {
	Data     []byte
	Identity string
}

// Reader pins a workspace directory for discovery.
type Reader struct{ root *os.File }

// Open pins a root without following a final symlink.
func Open(root string) (*Reader, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &Reader{os.NewFile(uintptr(fd), root)}, nil
}

// Close releases the pinned root.
func (r *Reader) Close() error { return r.root.Close() }

// FromDirectory duplicates an already pinned directory handle.
func FromDirectory(dir *os.File) (*Reader, error) {
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &Reader{os.NewFile(uintptr(fd), "workspace")}, nil
}
func (r *Reader) open(p string, directory bool) (*os.File, error) {
	if !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return nil, fmt.Errorf("invalid workspace path")
	}
	if p != "." {
		for s := range strings.SplitSeq(p, "/") {
			if s == "" || s == "." || s == ".." {
				return nil, fmt.Errorf("invalid workspace path")
			}
		}
	}
	flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if directory {
		flags |= unix.O_DIRECTORY
	}
	// Each descriptor pins its directory. No component can be a symlink,
	// including during a concurrent replacement; no lexical parent is accepted.
	// openat2 prevents rename-out races as well as symlink swaps. Require
	// kernel support instead of falling back to a weaker path walk.
	fd, err := unix.Openat2(int(r.root.Fd()), p, &unix.OpenHow{Flags: uint64(flags), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, err
	}
	// Inspect via O_PATH before opening for reading: opening a device can
	// itself have effects, and FIFOs must never block this operation.
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	kind := stat.Mode & unix.S_IFMT
	if (directory && kind != unix.S_IFDIR) || (!directory && kind != unix.S_IFREG) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("not a permitted discovery file type")
	}
	file, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	_ = unix.Close(fd)
	return file, err
}

// List returns at most limit entries. Partial directories must not be selected.
func (r *Reader) List(ctx context.Context, p string, limit int) ([]Entry, bool, error) {
	if limit < 1 || limit > 10000 {
		return nil, false, fmt.Errorf("invalid entry limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	f, err := r.open(p, true)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	var out []Entry
	for len(out) <= limit {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		es, err := f.ReadDir(1)
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		if len(es) == 0 {
			break
		}
		e := es[0]
		var stat unix.Stat_t
		if err := unix.Fstatat(int(f.Fd()), e.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, false, err
		}
		kind := "other"
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			kind = "symlink"
		case unix.S_IFDIR:
			kind = "directory"
		case unix.S_IFREG:
			kind = "file"
		}
		out = append(out, Entry{e.Name(), kind})
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// Read opens only regular files and bounds allocation before reading.
func (r *Reader) Read(ctx context.Context, p string, limit int64) (File, error) {
	if limit < 1 || limit > 16<<20 {
		return File{}, fmt.Errorf("invalid byte limit")
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	f, err := r.open(p, false)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = f.Close() }()
	var before, after unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &before); err != nil {
		return File{}, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return File{}, fmt.Errorf("not a regular file")
	}
	if before.Nlink > 1 {
		return File{}, fmt.Errorf("multiply linked file is not a discovery source")
	}
	if before.Size > limit {
		return File{}, fmt.Errorf("file exceeds byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return File{}, err
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	if len(data) > int(limit) {
		return File{}, fmt.Errorf("file exceeds byte limit")
	}
	if err := unix.Fstat(int(f.Fd()), &after); err != nil {
		return File{}, err
	}
	identity := func(s unix.Stat_t) string {
		return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d", s.Dev, s.Ino, s.Size, s.Mtim.Sec, s.Mtim.Nsec, s.Ctim.Sec, s.Ctim.Nsec)
	}
	if identity(before) != identity(after) {
		return File{}, fmt.Errorf("file changed during read")
	}
	return File{data, identity(after)}, nil
}
