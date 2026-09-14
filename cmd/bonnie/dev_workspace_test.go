package main

import (
	"os"
	"path/filepath"
	"testing"
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
	ws := filepath.Join(root, "workspace")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"a file the model wrote", filepath.Join(ws, "notes.md"), false},
		{"a nested file the model wrote", filepath.Join(ws, "sub", "deep.txt"), false},
		{"the workspace directory itself", ws, false},
		{"the generated file", filepath.Join(root, "bonnie_gen.go"), false},

		{"the manifest", filepath.Join(root, "agent.yaml"), true},
		{"the instructions", filepath.Join(root, "instructions.md"), true},
		{"a tool", filepath.Join(root, "tools", "echo", "tool.go"), true},
		{"go.mod", filepath.Join(root, "go.mod"), true},
		// A sibling whose name merely starts with the workspace path must
		// still be watched: prefix matching alone would swallow it.
		{"a sibling with a workspace-like name", filepath.Join(root, "workspace-notes.md"), true},
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

// TestWorkspaceDirFollowsTheManifest: the dev loop must skip the directory the
// agent actually writes to, which the manifest can rename. Resolving it any
// other way would leave a renamed workspace watched.
func TestWorkspaceDirFollowsTheManifest(t *testing.T) {
	t.Parallel()

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeManifest(t, root, "apiVersion: bonnie.dev/v0alpha\n")
		if got, want := workspaceDir(root), filepath.Join(root, "workspace"); got != want {
			t.Fatalf("workspaceDir = %q, want %q", got, want)
		}
	})

	t.Run("renamed by the manifest", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeManifest(t, root, "apiVersion: bonnie.dev/v0alpha\nworkspace: files/\n")
		if got, want := workspaceDir(root), filepath.Join(root, "files"); got != want {
			t.Fatalf("workspaceDir = %q, want %q", got, want)
		}
		// The renamed one is skipped, and the default name is not special.
		if watched(filepath.Join(root, "files", "out.txt"), workspaceDir(root)) {
			t.Error("the renamed workspace is still watched")
		}
	})

	// No manifest at all still yields the scaffold's default, so a tree that
	// cannot be loaded does not silently start watching the workspace.
	t.Run("no manifest", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if got, want := workspaceDir(root), filepath.Join(root, "workspace"); got != want {
			t.Fatalf("workspaceDir = %q, want %q", got, want)
		}
	})
}

func writeManifest(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "agent.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "instructions.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
