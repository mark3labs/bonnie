package main

import (
	"path/filepath"
	"testing"

	"github.com/mark3labs/bonnie"
)

// TestWorkspaceIsNotWatched is the guard on a feedback loop.
//
// The workspace is the agent's root for files, so a model writing a file is
// the ordinary case, not an edge case. If the dev loop watched it, that write
// would trigger a rebuild and SIGTERM the child still serving the turn: the
// agent would restart itself, mid-answer, for doing its job.
func TestWorkspaceIsNotWatched(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ws := filepath.Join(root, bonnie.DefaultWorkspace)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"a file the model wrote", filepath.Join(ws, "notes.md"), false},
		{"a nested file the model wrote", filepath.Join(ws, "sub", "deep.txt"), false},
		{"the workspace directory itself", ws, false},
		{"the generated file", filepath.Join(root, "bonnie_gen.go"), false},

		{"main.go", filepath.Join(root, "main.go"), true},
		{"the instructions", filepath.Join(root, bonnie.DefaultInstructions), true},
		{"a tool", filepath.Join(root, "tools", "echo", "tool.go"), true},
		{"go.mod", filepath.Join(root, "go.mod"), true},
		// A sibling whose name merely starts with the workspace path must
		// still be watched: prefix matching alone would swallow it.
		{"a sibling with a workspace-like name", filepath.Join(root, bonnie.DefaultWorkspace+"-notes.md"), true},
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

// TestWorkspaceDirIsTheRuntimeWorkspace: the directory the dev loop skips must
// be the directory the running agent writes to. They are resolved in different
// packages — the loop here, the runtime in [bonnie.Run] — so the two agree only
// because both read [bonnie.DefaultWorkspace].
//
// This is invariant 14 (one place per setting) pointed at the dev loop. When
// the manifest could rename the workspace, the loop had to re-derive the name
// the same way the runtime did, and a mismatch left the renamed directory
// watched. The constant removed the class of defect; this test is what keeps a
// second copy of the path from creeping back in.
func TestWorkspaceDirIsTheRuntimeWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	got := workspaceDir(root)
	want := filepath.Join(root, bonnie.DefaultWorkspace)
	if got != want {
		t.Fatalf("workspaceDir = %q, want %q", got, want)
	}
	if watched(filepath.Join(got, "out.txt"), got) {
		t.Error("the agent's own workspace is watched: a model's write would restart the child mid-turn")
	}
}
