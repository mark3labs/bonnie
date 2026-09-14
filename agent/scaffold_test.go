package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/treetest"
)

// The scaffolded tree is the default layout and nothing else: no manifest,
// no format flag, and --model lands in main.go as a bonnie option.
func TestScaffoldIsTheDefaultLayout(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"", "opencode/kimi-k2.5"} {
		t.Run("model="+model, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "my-agent")
			created, err := Scaffold(dir, InitOptions{Model: model})
			if err != nil {
				t.Fatalf("Scaffold: %v", err)
			}
			if len(created) == 0 {
				t.Fatal("no files created")
			}

			for _, f := range []string{"instructions.md", "go.mod", "main.go", "bonnie_gen.go"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
					t.Fatalf("the scaffold did not create %s: %v", f, err)
				}
			}
			// Directories exist and are tracked.
			for _, d := range []string{"skills", "workspace"} {
				if _, err := os.Stat(filepath.Join(dir, d, gitkeep)); err != nil {
					t.Fatalf("the %s directory was not scaffolded: %v", d, err)
				}
			}

			// No manifest, in any format. A tree configured in two places is
			// a tree whose settings can disagree.
			for _, name := range []string{"agent.yaml", "agent.yml", "agent.toml", "agent.json"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("the scaffold wrote a manifest (%s); configuration is code", name)
				}
			}

			main, err := os.ReadFile(filepath.Join(dir, "main.go"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(main), "bonnie.Main(") {
				t.Fatalf("main.go does not call bonnie.Main: %s", main)
			}
			wantModel := `bonnie.WithModel("` + model + `")`
			if model == "" {
				wantModel = `// bonnie.WithModel("anthropic/claude-sonnet-4-5")`
			}
			if !strings.Contains(string(main), wantModel) {
				t.Fatalf("main.go does not carry %q:\n%s", wantModel, main)
			}
		})
	}
}

// The minimum a user writes is one call. A scaffold that grows boilerplate
// back is a scaffold that has started to copy the framework into user space,
// where it drifts.
func TestScaffoldedMainIsOneCall(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "my-agent")
	if _, err := Scaffold(dir, InitOptions{}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	var code int
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		code++
	}
	// package, import block (3), func main, bonnie.Main(, ), }
	if code > 12 {
		t.Fatalf("the scaffolded main.go carries %d lines of code; it is meant to be one call:\n%s", code, b)
	}
	if strings.Contains(string(b), "net/http") || strings.Contains(string(b), "runtime.NewRunner") {
		t.Fatalf("main.go wires the server by hand; that belongs in the framework:\n%s", b)
	}
}

func TestScaffoldNeverOverwrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sentinel := "the author was here first\n"
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Scaffold(dir, InitOptions{})
	if err == nil {
		t.Fatal("want a refusal when a file exists")
	}
	if !strings.Contains(err.Error(), "instructions.md") || !strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("the refusal does not name the file and the rule: %v", err)
	}

	// The existing file is untouched.
	b, err := os.ReadFile(filepath.Join(dir, "instructions.md"))
	if err != nil || string(b) != sentinel {
		t.Fatalf("init touched the existing file: %q, %v", b, err)
	}

	// And init changed nothing else: no half-scaffold next to the refusal.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("init wrote files despite refusing: %v", entries)
	}
}

func TestScaffoldToolsModule(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "my-agent")
	if _, err := Scaffold(dir, InitOptions{Tools: true}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	for _, f := range []string{
		"go.mod", "main.go", "bonnie_gen.go", filepath.Join("tools", "echo", "tool.go"),
	} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("the --tools scaffold did not create %s: %v", f, err)
		}
	}

	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(mod), "module my-agent\n") || !strings.Contains(string(mod), "go 1.27") {
		t.Fatalf("go.mod is not the expected scaffold: %s", mod)
	}

	// The generated wiring stub is the disposable placeholder; the generator
	// rewrites it from the tree on the next build. It carries the banner but,
	// alone, registers no tools — the sample tool is wired by codegen.
	gen, err := os.ReadFile(filepath.Join(dir, "bonnie_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gen), "DO NOT EDIT") {
		t.Fatalf("bonnie_gen.go is not the expected stub: %s", gen)
	}
	if !strings.Contains(string(gen), "bonnie.Register(bonnie.Tree{") ||
		!strings.Contains(string(gen), "Tools:        []kit.Tool{}") {
		t.Fatalf("the stub should register an empty tree until codegen runs: %s", gen)
	}

	// The scaffold carries its own .gitignore? No — it does not. Assert the
	// journal directory is not pre-created either: the tree starts clean.
	if _, err := os.Stat(filepath.Join(dir, ".bonnie")); !os.IsNotExist(err) {
		t.Fatal("the scaffold pre-created a journal directory")
	}
}

// The scaffold pins the release it was generated by, so a tree's go mod tidy
// resolves a specific bonnie release instead of whatever @latest the proxy
// offers (which lags a fresh tag). A non-release build writes no requires.
func TestScaffoldPinsReleasedVersion(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"v0.2.0", "v0.2.0"},
		{"0.2.0", "v0.2.0"},
		{"dev", ""},
		{"0.1.0-SNAPSHOT-b1e45fc", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := pinVersion(c.in); got != c.want {
			t.Fatalf("pinVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// A released binary pins the bonnie require; a dev binary does not.
	dir := filepath.Join(t.TempDir(), "pinned")
	if _, err := Scaffold(dir, InitOptions{Version: "v0.2.0"}); err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), "require github.com/mark3labs/bonnie v0.2.0") {
		t.Fatalf("released scaffold does not pin bonnie: %s", mod)
	}

	dev := filepath.Join(t.TempDir(), "dev")
	if _, err := Scaffold(dev, InitOptions{Version: "dev"}); err != nil {
		t.Fatal(err)
	}
	mod, err = os.ReadFile(filepath.Join(dev, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "require") {
		t.Fatalf("dev scaffold pins a version: %s", mod)
	}
}

// The scaffold's main.go must compile against this checkout. The mark3labs
// modules are public, so a user can `go mod tidy` and build straight off the
// proxy; this test instead points the tree's go.mod at the local checkout, so
// it exercises the code under test and needs no network and no second
// repository.
func TestScaffoldToolsModuleBuilds(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "my-agent")
	if _, err := Scaffold(dir, InitOptions{Tools: true}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	treetest.LinkToCheckout(t, dir)

	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	build.Env = treetest.BuildEnv()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the fresh scaffold does not build: %v\n%s", err, out)
	}
}
