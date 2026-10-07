package bonnie

import (
	"os"
	"path/filepath"
	"testing"
)

// Embedded context files seed a run without legacy tree fields.
func TestTreeContextFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := seedFromEmbed((Tree{ContextFiles: testSeed}).ContextFiles, dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("context files not seeded: %v", err)
	}
}

// A legacy directory must not change the default context-file path.
func TestContextFilesDirectoryIgnoresLegacyLayout(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("workspace", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*config{defaults(), resolve(WithContextFiles(DefaultContextFiles))} {
		got, err := c.contextFilesDir()
		if err != nil || filepath.Base(got) != DefaultContextFiles {
			t.Fatalf("context directory: %q %v", got, err)
		}
	}
	got, err := resolve(WithContextFiles("")).contextFilesDir()
	if err != nil || got != "" {
		t.Fatalf("disabled context directory: %q %v", got, err)
	}
}
