package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scaffoldTools builds a fresh --tools tree with a single sample tool, and
// returns its root.
func scaffoldTools(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "my-agent")
	if _, err := Scaffold(dir, InitOptions{Tools: true}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	return dir
}

// writeTool writes tools/<name>/tool.go exporting a tool whose runtime name is
// declared. The directory name is the tool's name in discovery.
func writeTool(t *testing.T, root, name, declared string) {
	t.Helper()
	dir := filepath.Join(root, "tools", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `package ` + name + `

import (
	"context"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the ` + declared + ` tool.
func Tool() kit.Tool {
	type in struct {
		Text string ` + "`json:\"text\"`" + `
	}
	return kit.NewTool("` + declared + `", "a test tool",
		func(ctx context.Context, i in) (kit.ToolOutput, error) {
			return kit.TextResult(i.Text), nil
		})
}
`
	if err := os.WriteFile(filepath.Join(dir, "tool.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Codegen twice over the same tree must be byte-identical (invariant 12).
func TestCodegenIsIdempotent(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	writeTool(t, root, "charge_card", "charge_card")

	first, _, err := Generate(root)
	if err != nil {
		t.Fatalf("Generate (1): %v", err)
	}
	second, _, err := Generate(root)
	if err != nil {
		t.Fatalf("Generate (2): %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("codegen is not idempotent: two runs over one tree differ\n--- first\n%s\n--- second\n%s", first, second)
	}
}

// Two tools that declare the same runtime name must fail with both paths
// named, before a provider silently keeps only one.
func TestCodegenRejectsDuplicateToolName(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	// The scaffold already has tools/echo declaring "echo". Add a second dir
	// that also declares "echo".
	writeTool(t, root, "clone", "echo")

	_, _, err := Generate(root)
	if err == nil {
		t.Fatal("want a duplicate-tool error")
	}
	if !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("err = %v, want ErrDuplicateTool", err)
	}
	for _, name := range []string{"echo", "clone"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the error does not name %q: %v", name, err)
		}
	}
}

// A tools/<name>/ directory without func Tool() is a generator error naming
// the directory, not a later compile failure.
func TestCodegenRejectsToolWithoutToolFunc(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	dir := filepath.Join(root, "tools", "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tool.go"),
		[]byte("package broken\n\nfunc Nothing() int { return 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := Generate(root)
	if err == nil || !errors.Is(err, ErrToolMissingToolFunc) {
		t.Fatalf("err = %v, want ErrToolMissingToolFunc", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("the error does not name the directory: %v", err)
	}
}

// The generated file's imports must match the allowlist: standard library,
// BONNIE public packages, kit/pkg/kit, and the tree's own tool packages.
// Anything else — especially kit/internal and fantasy — is a violation
// (invariant 11).
func TestGeneratedImportsMatchAllowlist(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	writeTool(t, root, "charge_card", "charge_card")

	out, _, err := Generate(root)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	text := string(out)

	for _, bad := range []string{"kit/internal", "charm.land/fantasy", "charm.land/"} {
		if strings.Contains(text, bad) {
			t.Fatalf("the generated file imports the forbidden %q", bad)
		}
	}

	// Every import line must be stdlib (no dot), a bonnie public package,
	// kit/pkg/kit, or the tree's own module path.
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`) && !strings.Contains(line, ` "`) {
			continue
		}
		imp := line
		if i := strings.Index(line, ` "`); i >= 0 {
			imp = line[i+2:]
		}
		imp = strings.Trim(imp, `"`)
		if !allowedImport(imp, "my-agent") {
			t.Fatalf("the generated file imports %q, outside the allowlist", imp)
		}
	}
}

// allowedImport reports whether an import path is in the codegen allowlist.
func allowedImport(imp, module string) bool {
	if imp == "" {
		return true
	}
	if !strings.Contains(imp, ".") {
		return true // standard library
	}
	if imp == "github.com/mark3labs/kit/pkg/kit" {
		return true
	}
	if strings.HasPrefix(imp, "github.com/mark3labs/bonnie/") {
		return true
	}
	if strings.HasPrefix(imp, module+"/tools/") {
		return true
	}
	return false
}

// Authored files are never rewritten: generating and writing bonnie_gen.go
// touches only that file, leaving instructions.md, tools/, skills/, and
// workspace/ untouched.
func TestGeneratorRewritesOnlyGeneratedFile(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	writeTool(t, root, "charge_card", "charge_card")

	sentinel, err := os.ReadFile(filepath.Join(root, "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := Generate(root)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	gen := filepath.Join(root, "bonnie_gen.go")
	if err := os.WriteFile(gen, out, 0o644); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(string(out), "// Code generated") {
		t.Fatal("the generated file lacks its DO-NOT-EDIT banner")
	}
	b, err := os.ReadFile(filepath.Join(root, "instructions.md"))
	if err != nil || string(b) != string(sentinel) {
		t.Fatalf("instructions.md was rewritten: %q, %v", b, err)
	}
	for _, p := range []string{
		filepath.Join("tools", "charge_card", "tool.go"),
		filepath.Join("tools", "echo", "tool.go"),
	} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatalf("authored file %s changed: %v", p, err)
		}
	}
}

// The --dry-run report names the files found, the tools generated, and the
// embed set.
func TestPlanStringNamesDiscovery(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	writeTool(t, root, "charge_card", "charge_card")
	if err := os.WriteFile(filepath.Join(root, "skills", "review.md"), []byte("# review\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	report := p.String()
	for _, want := range []string{"my-agent", "echo", "charge_card", "instructions.md", "skills/", "output: bonnie_gen.go"} {
		if !strings.Contains(report, want) {
			t.Fatalf("the dry-run report does not contain %q:\n%s", want, report)
		}
	}
}

// A custom workspace path must still bind to the canonical _workspace
// accessor, so a manifest that names a non-default directory is exposed the
// same way the default is.
func TestCodegenBindsCustomWorkspaceToCanonicalVar(t *testing.T) {
	t.Parallel()
	root := scaffoldTools(t)
	// The scaffold's manifest names workspace: workspace/. Overwrite it to a
	// custom directory and create one real file there.
	yamlW := strings.Replace(string(mustReadFile(t, filepath.Join(root, "agent.yaml"))), "workspace: workspace/", "workspace: seeds/", 1)
	if err := os.WriteFile(filepath.Join(root, "agent.yaml"), []byte(yamlW), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "seeds"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "seeds", "a.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := Generate(root)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "//go:embed seeds/") && !strings.Contains(text, "//go:embed seeds") {
		t.Fatalf("the generated file does not embed the custom workspace:\n%s", text)
	}
	if !strings.Contains(text, "var _workspace embed.FS") {
		t.Fatalf("the custom workspace is not bound to _workspace:\n%s", text)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The generated file must compile as the tree's wiring. This is the guard test
// that leans on the compiler: codegen emits a valid Go file. It is skipped
// when no kit checkout is beside the repo, like the scaffold build test.
func TestGeneratedFileCompiles(t *testing.T) {
	kitRoot, ok := findUpstream(t)
	if !ok {
		t.Skip("no kit checkout beside this repo; the workspace build cannot be simulated")
	}

	parent := t.TempDir()
	root := filepath.Join(parent, "my-agent")
	if _, err := Scaffold(root, InitOptions{Tools: true}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	writeTool(t, root, "charge_card", "charge_card")
	if err := os.WriteFile(filepath.Join(root, "workspace", "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := Generate(root)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bonnie_gen.go"), out, 0o644); err != nil {
		t.Fatal(err)
	}

	work := filepath.Join(parent, "go.work")
	if err := os.WriteFile(work, []byte("go 1.27.1\n\nuse (\n\t./my-agent\n\t"+bonnieRoot()+"\n\t"+kitRoot+"\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	build := exec.Command("go", "build", "./...")
	build.Dir = root
	build.Env = append(os.Environ(), "GOWORK="+work)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the generated file does not compile: %v\n%s", err, out)
	}
}
