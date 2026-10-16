//go:build linux

package sandbox

import (
	"context"

	"github.com/mark3labs/bonnie/internal/skillfs"
)

// DiscoveryEntry describes a direct workspace child without following links.
type DiscoveryEntry = skillfs.Entry

// DiscoveryFile holds bounded file bytes and a stable stat identity.
type DiscoveryFile = skillfs.File

// SkillDiscovery is an optional sandbox capability. Operations accept only
// workspace-relative paths (root is "."). Implementations must pin their root,
// reject every symlink component and nonregular file, and bound reads before
// allocation. They must not consult the host for a remote workspace.
type SkillDiscovery interface {
	SkillDirectory(context.Context, string, int) ([]DiscoveryEntry, bool, error)
	SkillFile(context.Context, string, int64) (DiscoveryFile, error)
}

func (s *localSandbox) discovery() (*skillfs.Reader, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	s.discoveryOnce.Do(func() { s.discoveryRoot, s.discoveryErr = skillfs.Open(s.dir) })
	if s.discoveryErr != nil {
		return nil, s.discoveryErr
	}
	if s.discoveryRoot == nil {
		return nil, ErrClosed
	}
	return s.discoveryRoot, nil
}

// SkillDirectory enumerates the local workspace without following links.
func (s *localSandbox) SkillDirectory(ctx context.Context, p string, n int) ([]DiscoveryEntry, bool, error) {
	r, err := s.discovery()
	if err != nil {
		return nil, false, err
	}
	return r.List(ctx, p, n)
}

// SkillFile reads a bounded regular file from the local workspace.
func (s *localSandbox) SkillFile(ctx context.Context, p string, n int64) (DiscoveryFile, error) {
	r, err := s.discovery()
	if err != nil {
		return DiscoveryFile{}, err
	}
	return r.Read(ctx, p, n)
}
func (s *landlockSandbox) discovery() (*skillfs.Reader, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return skillfs.FromDirectory(dir)
}

// SkillDirectory enumerates the pinned Landlock workspace without links.
func (s *landlockSandbox) SkillDirectory(ctx context.Context, p string, n int) ([]DiscoveryEntry, bool, error) {
	r, err := s.discovery()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = r.Close() }()
	return r.List(ctx, p, n)
}

// SkillFile reads a bounded regular file from the pinned Landlock workspace.
func (s *landlockSandbox) SkillFile(ctx context.Context, p string, n int64) (DiscoveryFile, error) {
	r, err := s.discovery()
	if err != nil {
		return DiscoveryFile{}, err
	}
	defer func() { _ = r.Close() }()
	return r.Read(ctx, p, n)
}
