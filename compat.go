package bonnie

import "github.com/mark3labs/bonnie/runtime"

// DefaultWorkspace is the legacy authored seed directory.
// Deprecated: use DefaultContextFiles.
const DefaultWorkspace = "workspace"

// WithWorkspace selects an authored seed directory.
// Deprecated: use WithContextFiles.
func WithWorkspace(dir string) Option { return WithContextFiles(dir) }

// WorkspaceCleanupPolicy sets terminal sandbox retention.
// Deprecated: use SandboxCleanupPolicy.
type WorkspaceCleanupPolicy = runtime.SandboxCleanupPolicy

// WithRunWorkspaceCleanup enables terminal sandbox cleanup.
// Deprecated: use WithRunSandboxCleanup.
func WithRunWorkspaceCleanup(policy WorkspaceCleanupPolicy) Option {
	return WithRunSandboxCleanup(policy)
}

// WithPersistentWorkspace selects shared host storage.
// Deprecated: use WithSharedDirectory.
func WithPersistentWorkspace(dir string) Option { return WithSharedDirectory(dir) }
