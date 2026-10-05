package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A later helper file must not erase a tool's runtime name.
func TestDuplicateToolAcrossFiles(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	writeTool(t, root, "other", "echo")
	if err := os.WriteFile(filepath.Join(root, "tools", "echo", "zzz.go"), []byte("package echo\n// NewTool(\"not-a-call\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("Discover = %v, want duplicate", err)
	}
}

// Hidden files and empty directories cannot make a Go embed pattern valid.
func TestEmbedEligibility(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{".hidden", "_hidden", "empty"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".only"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasRealContent(root) {
		t.Fatal("ineligible content selected for embed")
	}
	if err := os.WriteFile(filepath.Join(root, "empty", "data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasRealContent(root) {
		t.Fatal("nested regular file not selected")
	}
}

// Init must refuse a dangling symlink without changing its target.
func TestScaffoldRefusesDanglingSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "missing")
	if err := os.Symlink(target, filepath.Join(root, "main.go")); err != nil {
		t.Skip(err)
	}
	if _, err := Scaffold(root, InitOptions{}); err == nil {
		t.Fatal("accepted dangling symlink")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); !os.IsNotExist(err) {
		t.Fatal("preflight wrote go.mod")
	}
}

// Text that looks like a constructor is not a runtime declaration.
func TestDuplicateDiscoveryIgnoresComments(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	writeTool(t, root, "other", "different")
	path := filepath.Join(root, "tools", "other", "tool.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src = append([]byte("// NewTool(\"echo\")\n"), src...)
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); err != nil {
		t.Fatal(err)
	}
}

// Only the constructor returned by Tool establishes its runtime name.
func TestToolNameIgnoresUnrelatedConstructors(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"helper": `func helper() kit.Tool { return kit.NewTool("echo", "helper", nil) }
func Tool() kit.Tool { return kit.NewTool("other", "tool", nil) }`,
		"helper-built": `func helper() kit.Tool { return kit.NewTool("echo", "helper", nil) }
func Tool() kit.Tool { return helper() }`,
		"shadowed":    `func Tool() kit.Tool { kit := fake{}; return kit.NewTool("echo", "tool", nil) }`,
		"conditional": `func Tool() kit.Tool { if enabled { return kit.NewTool("echo", "tool", nil) }; return kit.NewTool("other", "tool", nil) }`,
		"generic":     "func Tool() kit.Tool { return kit.NewTool[input](`other`, \"tool\", nil) }",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := scaffoldTools(t)
			dir := filepath.Join(root, "tools", "other")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			src := "package other\nimport kit \"github.com/mark3labs/kit/pkg/kit\"\n" + body
			if err := os.WriteFile(filepath.Join(dir, "tool.go"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			plan, err := Discover(root)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if name == "helper" || name == "generic" {
				want = "other"
			}
			if got := plan.Tools[1].DeclaredName; got != want {
				t.Fatalf("declared name = %q, want %q", got, want)
			}
		})
	}
}

// Neither a receiver method nor a test-only function supplies the export.
func TestDiscoveryRequiresPackageToolFunction(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	dir := filepath.Join(root, "tools", "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, src := range map[string]string{
		"tool.go":      "package broken\ntype T struct{}\nfunc (T) Tool() int { return 0 }",
		"tool_test.go": "package broken\nfunc Tool() int { return 0 }",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Discover(root); !errors.Is(err, ErrToolMissingToolFunc) {
		t.Fatalf("Discover = %v, want missing export", err)
	}
}

// A symlinked slot or nested module must not trigger an embed directive.
func TestEmbedSkipsSymlinkSlotsAndModules(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if hasRealContent(link) || hasRealContent(root) {
		t.Fatal("symlink selected for embed")
	}
	if err := os.WriteFile(filepath.Join(target, "go.mod"), []byte("module nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasRealContent(target) {
		t.Fatal("nested module selected for embed")
	}
}

// A parent symlink must be refused before any scaffold file is written.
func TestScaffoldRefusesSymlinkParent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "skills")); err != nil {
		t.Skip(err)
	}
	if _, err := Scaffold(root, InitOptions{}); err == nil {
		t.Fatal("accepted symlink parent")
	}
	for _, path := range []string{filepath.Join(root, "instructions.md"), filepath.Join(target, ".gitkeep")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("scaffold wrote %s: %v", path, err)
		}
	}
}
