package workspaceskills

import (
	"bytes"
	"io"
	"io/fs"
	"time"
)

// candidateFS exposes exactly one bounded candidate to Kit without disk I/O.
type candidateFS struct{ data []byte }

func (f candidateFS) Open(name string) (fs.File, error) {
	switch name {
	case ".":
		return &candidateDir{}, nil
	case "candidate.md":
		return &candidateFile{Reader: bytes.NewReader(f.data), size: int64(len(f.data))}, nil
	default:
		return nil, fs.ErrNotExist
	}
}

type candidateInfo struct {
	directory bool
	size      int64
}

func (i candidateInfo) Name() string {
	if i.directory {
		return "."
	}
	return "candidate.md"
}
func (i candidateInfo) Size() int64 { return i.size }
func (i candidateInfo) Mode() fs.FileMode {
	if i.directory {
		return fs.ModeDir | 0500
	}
	return 0400
}
func (i candidateInfo) ModTime() time.Time         { return time.Time{} }
func (i candidateInfo) IsDir() bool                { return i.directory }
func (i candidateInfo) Sys() any                   { return nil }
func (i candidateInfo) Type() fs.FileMode          { return i.Mode().Type() }
func (i candidateInfo) Info() (fs.FileInfo, error) { return i, nil }

type candidateFile struct {
	*bytes.Reader
	size int64
}

func (f *candidateFile) Stat() (fs.FileInfo, error) { return candidateInfo{size: f.size}, nil }
func (f *candidateFile) Close() error               { return nil }

type candidateDir struct{ read bool }

func (d *candidateDir) Stat() (fs.FileInfo, error) { return candidateInfo{directory: true}, nil }
func (d *candidateDir) Read([]byte) (int, error)   { return 0, fs.ErrInvalid }
func (d *candidateDir) Close() error               { return nil }
func (d *candidateDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.read {
		if n > 0 {
			return nil, io.EOF
		}
		return nil, nil
	}
	d.read = true
	return []fs.DirEntry{candidateInfo{}}, nil
}
