package main

import (
	"path/filepath"
	"testing"

	"github.com/mark3labs/bonnie"
)

// TestContextFilesAreNotWatched checks that authored context files do not
// trigger a rebuild when the dev loop watches the agent tree.
func TestContextFilesAreNotWatched(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ws := filepath.Join(root, bonnie.DefaultContextFiles)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"a context file", filepath.Join(ws, "notes.md"), false},
		{"a nested context file", filepath.Join(ws, "sub", "deep.txt"), false},
		{"the context files directory itself", ws, false},
		{"the generated file", filepath.Join(root, "bonnie_gen.go"), false},

		{"main.go", filepath.Join(root, "main.go"), true},
		{"the instructions", filepath.Join(root, bonnie.DefaultInstructions), true},
		{"a tool", filepath.Join(root, "tools", "echo", "tool.go"), true},
		{"go.mod", filepath.Join(root, "go.mod"), true},
		// A sibling whose name merely starts with the workspace path must
		// still be watched: prefix matching alone would swallow it.
		{"a sibling with a workspace-like name", filepath.Join(root, bonnie.DefaultContextFiles+"-notes.md"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := watched(c.path, ws); got != c.want {
				t.Errorf("watched(%q) = %t, want %t", c.path, got, c.want)
			}
		})
	}
}

// TestContextFilesDirIsTheTreeContextFiles checks that the dev loop skips the
// authored context files directory selected by bonnie.DefaultContextFiles.
func TestContextFilesDirIsTheTreeContextFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	got := contextFilesDir(root)
	want := filepath.Join(root, bonnie.DefaultContextFiles)
	if got != want {
		t.Fatalf("contextFilesDir = %q, want %q", got, want)
	}
	if watched(filepath.Join(got, "out.txt"), got) {
		t.Error("the context files directory is watched")
	}
}
