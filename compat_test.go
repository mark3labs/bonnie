package bonnie

import (
	"embed"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// Legacy API names must remain available to v0.16.0 callers.
func TestLegacyOptions(t *testing.T) {
	t.Parallel()
	c := defaults()
	WithWorkspace("legacy")(c)
	if c.contextFiles != "legacy" || !c.contextFilesSet {
		t.Fatal("legacy seed option lost")
	}
	WithRunWorkspaceCleanup(WorkspaceCleanupPolicy{CompletedAfter: 1})(c)
	if c.sandboxCleanup == nil {
		t.Fatal("legacy cleanup option lost")
	}
	WithPersistentWorkspace("shared")(c)
	if !c.sharedDirectorySet || c.sharedDirectory != "shared" {
		t.Fatal("legacy shared option lost")
	}
	if DefaultWorkspace != "workspace" || sandbox.Workspace != sandbox.WorkDir || sandbox.ErrOutsideWorkspace != sandbox.ErrOutsideWorkDir || runtime.RecordWorkspaceDeleted != runtime.RecordSandboxDeleted {
		t.Fatal("legacy constant or sentinel changed")
	}
	legacyPolicy := runtime.WorkspaceCleanupPolicy{CompletedAfter: 1}
	WithRunWorkspaceCleanup(legacyPolicy)(c)
}

// Old embedded wiring must seed files; a nonempty new field wins.
func TestLegacyTreeEmbed(t *testing.T) {
	t.Parallel()
	for _, tree := range []Tree{{Workspace: testSeed}, {ContextFiles: testSeed}, {Workspace: testSeed, ContextFiles: embed.FS{}}} {
		dir := t.TempDir()
		if err := seedFromEmbed(contextEmbed(tree), dir); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			t.Fatalf("legacy embed not seeded: %v", err)
		}
	}
	tree := Tree{ContextFiles: testSeed}
	if embedIsEmpty(contextEmbed(tree)) {
		t.Fatal("new field not selected")
	}
}

// Existing source trees keep workspace seeds, but explicit options do not fall back.
func TestLegacySeedDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir(DefaultWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	c := defaults()
	got, err := c.contextFilesDir()
	if err != nil || filepath.Base(got) != DefaultWorkspace {
		t.Fatalf("fallback: %q %v", got, err)
	}
	WithContextFiles(DefaultContextFiles)(c)
	got, err = c.contextFilesDir()
	if err != nil || filepath.Base(got) != DefaultContextFiles {
		t.Fatalf("explicit: %q %v", got, err)
	}
	if err := os.Mkdir(DefaultContextFiles, 0o755); err != nil {
		t.Fatal(err)
	}
	c = defaults()
	got, err = c.contextFilesDir()
	if err != nil || filepath.Base(got) != DefaultContextFiles {
		t.Fatalf("precedence: %q %v", got, err)
	}
}
