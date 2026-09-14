package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// writeTree writes a minimal agent tree and returns its root.
func writeTree(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("be terse"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestWorkspaceIsTheAgentRoot: a tree's files live in the workspace, so a
// model's write cannot land on the manifest, the instructions, or the
// journal. The default is workspace/ — the directory bonnie init scaffolds —
// and the manifest can name another.
func TestWorkspaceIsTheAgentRoot(t *testing.T) {
	t.Parallel()

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		dir := writeTree(t, "apiVersion: bonnie.dev/v0alpha\n")
		o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
		cfg, err := resolveServe(flags, o)
		if err != nil {
			t.Fatalf("resolveServe: %v", err)
		}
		want, err := filepath.Abs(filepath.Join(dir, "workspace"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.workspace != want {
			t.Fatalf("workspace = %q, want %q", cfg.workspace, want)
		}
		if !containsLine(cfg.banner, "workspace") {
			t.Errorf("the banner does not name the workspace:\n%s", strings.Join(cfg.banner, "\n"))
		}
	})

	t.Run("manifest names it", func(t *testing.T) {
		t.Parallel()
		dir := writeTree(t, "apiVersion: bonnie.dev/v0alpha\nworkspace: files/\n")
		o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
		cfg, err := resolveServe(flags, o)
		if err != nil {
			t.Fatalf("resolveServe: %v", err)
		}
		want, err := filepath.Abs(filepath.Join(dir, "files"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.workspace != want {
			t.Fatalf("workspace = %q, want %q", cfg.workspace, want)
		}
	})

	// No tree means nothing to anchor to, and the process's own directory
	// stays the root — the historical behaviour, kept deliberately.
	t.Run("no tree", func(t *testing.T) {
		t.Parallel()
		o, flags := parseServe(t, "--journal", filepath.Join(t.TempDir(), "j"))
		cfg, err := resolveServe(flags, o)
		if err != nil {
			t.Fatalf("resolveServe: %v", err)
		}
		if cfg.workspace != "" {
			t.Fatalf("workspace = %q, want empty without a tree", cfg.workspace)
		}
	})
}

// TestWorkspaceNeverEscapesTheTree pins what the resolution guarantees: the
// workspace is always inside the agent root, so the default configuration
// cannot point the model's files at the wider filesystem.
func TestWorkspaceNeverEscapesTheTree(t *testing.T) {
	t.Parallel()
	dir := writeTree(t, "apiVersion: bonnie.dev/v0alpha\n")
	o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
	cfg, err := resolveServe(flags, o)
	if err != nil {
		t.Fatalf("resolveServe: %v", err)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, cfg.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(rel, "..") {
		t.Fatalf("workspace %q is outside the agent root %q", cfg.workspace, root)
	}
	// The tree's own files must not be reachable by a relative path from the
	// workspace root: the manifest and the journal live one level up.
	if cfg.workspace == root {
		t.Fatal("the workspace is the agent root itself: a write would land on the manifest")
	}
}

// TestSandboxedAgentGetsNoHostTools is a security guard, not a style check.
//
// [hostWorkspaceOptions] rebuilds Kit's core tools with a working directory
// and passes them through [kit.WithTools], which sets Options.Tools. Kit
// honours Options.Tools even when DisableCoreTools is set — [sandbox.Agent]
// applies the caller's options after its own — so using those options on a
// sandboxed agent would hand the model host tools inside a sandbox: a real
// shell on this machine, exactly what the sandbox exists to prevent.
func TestSandboxedAgentGetsNoHostTools(t *testing.T) {
	t.Parallel()

	// With a workspace, the host options really do install host tools —
	// applied to a kit.Options, they populate the field Kit reads.
	var o kit.Options
	for _, opt := range hostWorkspaceOptions(t.TempDir()) {
		opt(&o)
	}
	if len(o.Tools) == 0 {
		t.Fatal("hostWorkspaceOptions installed no tools: the workdir would not be applied")
	}

	// Without one, they install nothing.
	var empty kit.Options
	for _, opt := range hostWorkspaceOptions("") {
		opt(&empty)
	}
	if len(empty.Tools) != 0 {
		t.Fatalf("no workspace must add no tools, got %d", len(empty.Tools))
	}

	// And the sandboxed branch must never call them. Reading the source is
	// the honest check here: the call has to be inside the `none` branch,
	// before the provider is built, and a future edit that moves it below
	// sandboxProvider would silently reintroduce host tools in a sandbox.
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	call := strings.Index(body, "hostWorkspaceOptions(workspace)")
	provider := strings.Index(body, "provider, err := sandboxProvider(")
	if call < 0 || provider < 0 {
		t.Fatal("serve.go no longer has the shapes this guard reads")
	}
	if call > provider {
		t.Fatal("hostWorkspaceOptions is called past the sandbox branch: " +
			"a sandboxed agent would receive host tools")
	}
	if strings.Count(body, "hostWorkspaceOptions(") != 2 { // the definition and the one call
		t.Fatalf("hostWorkspaceOptions is used %d times; it belongs on the no-sandbox branch only",
			strings.Count(body, "hostWorkspaceOptions(")-1)
	}
}

// TestSandboxedWorkspaceBecomesASeed: with a sandbox, the workspace is not a
// working directory but a seed mirrored into [sandbox.Workspace]. The
// manifest key was accepted and ignored before this — invariant 13 forbids
// exactly that.
func TestSandboxedWorkspaceBecomesASeed(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "sandbox.Seeded(provider, workspace)") {
		t.Fatal("the sandboxed branch does not seed the workspace: " +
			"the manifest key would be accepted and ignored")
	}
}
