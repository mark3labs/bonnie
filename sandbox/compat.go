package sandbox

// ErrOutsideWorkspace means a path leaves the sandbox work directory.
// Deprecated: use ErrOutsideWorkDir.
var ErrOutsideWorkspace = ErrOutsideWorkDir

// WithLocalSharedWorkspace enables shared local storage.
// Deprecated: use WithLocalSharedDirectory.
func WithLocalSharedWorkspace() LocalOption { return WithLocalSharedDirectory() }

// UseSharedWorkspace selects shared local storage.
// Deprecated: use UseSharedDirectory.
func (p *LocalProvider) UseSharedWorkspace(dir string) error { return p.UseSharedDirectory(dir) }

// UseSharedWorkspace selects shared confined host storage.
// Deprecated: use UseSharedDirectory.
func (p *LandlockProvider) UseSharedWorkspace(dir string) error { return p.UseSharedDirectory(dir) }
