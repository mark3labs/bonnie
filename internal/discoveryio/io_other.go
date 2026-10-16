//go:build !linux

package discoveryio

import (
	"context"
	"fmt"
)

// Root is unavailable on unsupported platforms.
type Root struct{}

// Open fails explicitly; no insecure fallback is used.
func Open(string) (*Root, error) { return nil, fmt.Errorf("secure discovery requires Linux") }

// Close implements the root lifecycle.
func (*Root) Close() error { return nil }

// List fails explicitly on unsupported platforms.
func (*Root) List(context.Context, string, int) ([]Entry, bool, error) {
	return nil, false, fmt.Errorf("secure discovery requires Linux")
}

// Read fails explicitly on unsupported platforms.
func (*Root) Read(context.Context, string, int64) (File, error) {
	return File{}, fmt.Errorf("secure discovery requires Linux")
}
