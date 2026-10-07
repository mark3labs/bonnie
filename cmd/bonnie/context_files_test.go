package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/bonnie"
)

// The watcher uses context/ even when only the legacy directory exists.
func TestContextFilesDirectoryIgnoresLegacyLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, bonnie.DefaultContextFiles)
	if got := contextFilesDir(root); got != want {
		t.Fatalf("context directory = %q, want %q", got, want)
	}
}
