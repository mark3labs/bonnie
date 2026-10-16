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
