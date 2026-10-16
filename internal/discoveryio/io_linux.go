//go:build linux

// Package discoveryio supplies bounded, rooted no-follow discovery operations.
package discoveryio

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Root holds a descriptor, not a path subject to replacement.
type Root struct {
	file   *os.File
	mu     sync.RWMutex
	closed bool
}

// Open opens a root without following the final directory symlink.
func Open(path string) (*Root, error) {
	base := "."
	if strings.HasPrefix(path, "/") {
		base = "/"
		path = strings.TrimPrefix(path, "/")
	}
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "" || segment == "." {
			continue
		}
		if segment == ".." {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("invalid root component")
		}
		next, err := unix.Openat(fd, segment, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return &Root{file: os.NewFile(uintptr(fd), "discovery-root")}, nil
}

// Close releases the root descriptor.
func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.file.Close()
}
func valid(p string) bool {
	if p == "." {
		return true
	}
	if !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return false
	}
	for s := range strings.SplitSeq(p, "/") {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}
func (r *Root) open(p string, directory bool) (*os.File, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, os.ErrClosed
	}
	if !valid(p) {
		return nil, fmt.Errorf("invalid workspace-relative path")
	}
	fd, err := unix.Openat(int(r.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if p == "." {
		return os.NewFile(uintptr(fd), p), nil
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	next, err := unix.Openat2(fd, p, &unix.OpenHow{Flags: uint64(flags), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	_ = unix.Close(fd)
	if err != nil {
		return nil, fmt.Errorf("rooted no-follow open: %w", err)
	}
	return os.NewFile(uintptr(next), p), nil
}
func identity(f *os.File) (Identity, error) {
	var s unix.Stat_t
	err := unix.Fstat(int(f.Fd()), &s)
	return Identity{uint64(s.Dev), s.Ino, s.Size, s.Mtim.Nano(), s.Ctim.Nano()}, err
}

// Read reads at most limit+1 bytes and rejects oversized or nonregular files.
func (r *Root) Read(ctx context.Context, p string, limit int64) (File, error) {
	if limit < 1 || limit > 64<<20 {
		return File{}, fmt.Errorf("invalid file limit")
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	f, err := r.open(p, false)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("not a regular file")
	}
	before, err := identity(f)
	if err != nil {
		return File{}, err
	}
	if before.Size > limit {
		return File{}, fmt.Errorf("file exceeds byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return File{}, err
	}
	if int64(len(data)) > limit {
		return File{}, fmt.Errorf("file exceeds byte limit")
	}
	after, err := identity(f)
	if err != nil {
		return File{}, err
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	return File{data, before, after}, nil
}

// List enumerates no more than limit entries, returning incomplete on overflow.
func (r *Root) List(ctx context.Context, p string, limit int) ([]Entry, bool, error) {
	if limit < 1 || limit > 10000 {
		return nil, false, fmt.Errorf("invalid entry limit")
	}
	f, err := r.open(p, true)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	var entries []Entry
	for len(entries) <= limit {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		batch, err := f.ReadDir(1)
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		if len(batch) == 0 {
			break
		}
		e := batch[0]
		if !utf8.ValidString(e.Name()) {
			return nil, false, fmt.Errorf("invalid UTF-8 directory entry")
		}
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
		entries = append(entries, Entry{e.Name(), kind})
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}
