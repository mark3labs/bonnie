package sandbox

import (
	"context"
	"sync"

	"github.com/mark3labs/bonnie/internal/discoveryio"
)

type discoveryRoot struct {
	mu   sync.Mutex
	root *discoveryio.Root
}

func (d *discoveryRoot) get(path string) (*discoveryio.Root, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.root == nil {
		r, err := discoveryio.Open(path)
		if err != nil {
			return nil, err
		}
		d.root = r
	}
	return d.root, nil
}
func (d *discoveryRoot) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.root != nil {
		return d.root.Close()
	}
	return nil
}

// ListDiscovery implements DiscoveryReader with descriptor-relative access.
func (s *localSandbox) ListDiscovery(ctx context.Context, p string, n int) ([]DiscoveryEntry, bool, error) {
	if s.isClosed() {
		return nil, false, ErrClosed
	}
	r, err := s.discovery.get(s.dir)
	if err != nil {
		return nil, false, err
	}
	return r.List(ctx, p, n)
}

// ReadDiscovery implements DiscoveryReader without following any symlink.
func (s *localSandbox) ReadDiscovery(ctx context.Context, p string, n int64) (DiscoveryFile, error) {
	if s.isClosed() {
		return DiscoveryFile{}, ErrClosed
	}
	r, err := s.discovery.get(s.dir)
	if err != nil {
		return DiscoveryFile{}, err
	}
	return r.Read(ctx, p, n)
}

// ListDiscovery implements DiscoveryReader with descriptor-relative access.
func (s *landlockSandbox) ListDiscovery(ctx context.Context, p string, n int) ([]DiscoveryEntry, bool, error) {
	r, err := s.discovery.get(s.dir)
	if err != nil {
		return nil, false, err
	}
	return r.List(ctx, p, n)
}

// ReadDiscovery implements DiscoveryReader without following any symlink.
func (s *landlockSandbox) ReadDiscovery(ctx context.Context, p string, n int64) (DiscoveryFile, error) {
	r, err := s.discovery.get(s.dir)
	if err != nil {
		return DiscoveryFile{}, err
	}
	return r.Read(ctx, p, n)
}
