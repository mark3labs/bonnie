//go:build !linux

package skillfs

import (
	"context"
	"fmt"
	"os"
)

// Reader is unavailable outside Linux.
type Reader struct{}

// Open refuses platforms without supported rooted discovery operations.
func Open(string) (*Reader, error) {
	return nil, fmt.Errorf("workspace skill discovery requires Linux")
}

// FromDirectory refuses unsupported platforms.
func FromDirectory(*os.File) (*Reader, error) { return Open("") }

// Close releases no resources on unsupported platforms.
func (*Reader) Close() error { return nil }

// List refuses unsupported platforms.
func (*Reader) List(context.Context, string, int) ([]Entry, bool, error) {
	return nil, false, fmt.Errorf("workspace skill discovery requires Linux")
}

// Read refuses unsupported platforms.
func (*Reader) Read(context.Context, string, int64) (File, error) {
	return File{}, fmt.Errorf("workspace skill discovery requires Linux")
}
