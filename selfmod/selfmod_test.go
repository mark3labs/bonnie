package selfmod

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/internal/treetest"
	"github.com/mark3labs/bonnie/sandbox"
)

func TestAuthored(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"instructions.md":                true,
		"main.go":                        true,
		"helpers.go":                     true,
		"go.mod":                         true,
		"go.sum":                         true,
		"tools/greet/tool.go":            true,
		"skills/review/SKILL.md":         true,
		"context/notes.txt":              true,
		"bonnie_gen.go":                  false,
		"tools/greet/bonnie_gen.go":      false,
		".env":                           false,
		".bonnie/journal.db":             false,
		"context/.secret":                false,
		"tools/.hidden/tool.go":          false,
		"README.md":                      false,
		"Dockerfile":                     false,
		"other/x.go":                     false,
		"../outside.go":                  false,
		"/etc/passwd":                    false,
		"tools/../.env":                  false,
		"":                               false,
		"tools/greet/testdata/input.txt": true,
	} {
		if got := Authored(path); got != want {
			t.Errorf("Authored(%q) = %v, want %v", path, got, want)
		}
	}
}

// newTree scaffolds an agent tree that builds against this checkout.
func newTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := agent.Scaffold(root, agent.InitOptions{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	treetest.LinkToCheckout(t, root)
	gen, plan, err := agent.Generate(root)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, plan.OutFile), gen, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// scripted is a fake subagent. Each attempt runs the next step with the
// subagent's real sandbox tools, so the steps edit the staged copy the way a
// model does: through write_file, confined to the workspace.
type scripted struct {
	mu      sync.Mutex
	steps   []func(t *testing.T, cfg kit.SubagentConfig) string
	prompts []string
	t       *testing.T
}

func (s *scripted) run(ctx context.Context, _ *kit.Kit, cfg kit.SubagentConfig) (*kit.SubagentResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, cfg.Prompt)
	if len(s.steps) == 0 {
		s.t.Errorf("unexpected subagent call %d", len(s.prompts))
		return &kit.SubagentResult{Response: "nothing to do"}, nil
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return &kit.SubagentResult{Response: step(s.t, cfg)}, nil
}

func (s *scripted) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

// writeFile calls the subagent's write_file tool.
func writeFile(t *testing.T, cfg kit.SubagentConfig, path, content string) {
	t.Helper()
	call(t, cfg.Tools, "write_file", map[string]string{"path": path, "content": content})
}

func call(t *testing.T, tools []kit.Tool, name string, args any) string {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Info().Name != name {
			continue
		}
		res, err := tool.Run(context.Background(), kit.LLMToolCall{ID: "sub-" + name, Name: name, Input: string(input)})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %s", name, err, res.Content)
		}
		return res.Content
	}
	t.Fatalf("no %s tool", name)
	return ""
}

func testConfig(s *scripted) Config {
	cfg := Config{
		Workspace: func(_ context.Context, dir string) (sandbox.Provider, error) {
			p := sandbox.Local()
			return p, p.UseSharedDirectory(dir)
		},
		subagent: s.run,
		buildEnv: treetest.BuildEnv(),
	}
	return cfg.normalize()
}

// localTools runs the Local factory as `bonnie dev` would, with root as the
// tree.
func localTools(t *testing.T, cfg Config, root string) []kit.Tool {
	t.Helper()
	getenv := func(k string) string {
		if k == bonnie.DevRootEnv {
			return root
		}
		return ""
	}
	tools, err := localFactory(cfg, getenv)(context.Background(), bonnie.ToolScope{RunID: "run-1"})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return tools
}

func runTool(t *testing.T, tool kit.Tool, id string, args any) kit.LLMToolResponse {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Run(context.Background(), kit.LLMToolCall{ID: id, Name: tool.Info().Name, Input: string(input)})
	if err != nil {
		t.Fatalf("run %s: %v", tool.Info().Name, err)
	}
	return res
}

const greetTool = `package greet

import (
	"context"

	kit "github.com/mark3labs/kit/pkg/kit"
)

func Tool() kit.Tool {
	type input struct {
		Name string ` + "`json:\"name\"`" + `
	}
	return kit.NewTool("greet", "Greet someone.", func(ctx context.Context, in input) (kit.ToolOutput, error) {
		return kit.TextResult("hello " + in.Name), nil
	})
}
`

// The default local tool exists only under `bonnie dev`, and an explicit
// Disabled removes it there too.
func TestLocalOnlyUnderDev(t *testing.T) {
	t.Parallel()
	cfg := testConfig(&scripted{t: t})
	none := func(string) string { return "" }
	if tools, err := localFactory(cfg, none)(context.Background(), bonnie.ToolScope{}); err != nil || len(tools) != 0 {
		t.Fatalf("outside dev: tools %d, err %v; want none", len(tools), err)
	}
	if tools := localTools(t, cfg, t.TempDir()); len(tools) != 1 || tools[0].Info().Name != LocalTool {
		t.Fatalf("under dev: want %s, got %d tools", LocalTool, len(tools))
	}
	cfg.Disabled = true
	if tools := localTools(t, cfg, t.TempDir()); len(tools) != 0 {
		t.Fatalf("disabled: want no tool, got %d", len(tools))
	}
	cfg.Disabled = false
	cfg.Authorize = func(context.Context, Caller) (bool, error) { return false, nil }
	if tools := localTools(t, cfg, t.TempDir()); len(tools) != 0 {
		t.Fatalf("unauthorized: want no tool, got %d", len(tools))
	}
}

// A change lands in the live tree only after it builds, and a repeated call
// — what recovery does after a dev restart cut the first one off — returns
// the first result without a second subagent run.
func TestLocalAppliesABuiltChangeOnce(t *testing.T) {
	t.Parallel()
	root := newTree(t)
	s := &scripted{t: t, steps: []func(*testing.T, kit.SubagentConfig) string{
		func(t *testing.T, cfg kit.SubagentConfig) string {
			if cfg.SystemPrompt != systemPrompt || !cfg.NoSession {
				t.Errorf("subagent config: system prompt or session not set")
			}
			writeFile(t, cfg, "tools/greet/tool.go", greetTool)
			writeFile(t, cfg, "notes.txt", "not authored")
			return "Added the greet tool."
		},
	}}
	tool := localTools(t, testConfig(s), root)[0]

	res := runTool(t, tool, "call-1", map[string]string{"request": "add a greet tool"})
	if res.IsError {
		t.Fatalf("modify_agent failed: %s", res.Content)
	}
	for _, want := range []string{"Applied 1 file change", "Added the greet tool.", "Not applied, because they are outside the agent tree's layout: notes.txt", "+++ b/tools/greet/tool.go", "+\t\treturn kit.TextResult"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("result does not contain %q:\n%s", want, res.Content)
		}
	}
	got, err := os.ReadFile(filepath.Join(root, "tools", "greet", "tool.go"))
	if err != nil || string(got) != greetTool {
		t.Fatalf("live tool file: %v %q", err, got)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("a file outside the layout reached the tree: %v", err)
	}

	again := runTool(t, tool, "call-1", map[string]string{"request": "add a greet tool"})
	if again.Content != res.Content {
		t.Errorf("repeated call returned a different result:\n%s", again.Content)
	}
	if n := s.calls(); n != 1 {
		t.Errorf("subagent ran %d times, want 1", n)
	}
}

// A change that does not build goes back to the subagent with the compiler
// output, and the live tree is not touched until it builds.
func TestLocalRetriesABrokenBuild(t *testing.T) {
	t.Parallel()
	root := newTree(t)
	s := &scripted{t: t}
	s.steps = []func(*testing.T, kit.SubagentConfig) string{
		func(t *testing.T, cfg kit.SubagentConfig) string {
			writeFile(t, cfg, "tools/greet/tool.go", strings.Replace(greetTool, "kit.TextResult(", "kit.TextResultt(", 1))
			if _, err := os.Stat(filepath.Join(root, "tools", "greet")); !os.IsNotExist(err) {
				t.Errorf("the live tree changed before the build: %v", err)
			}
			return "first try"
		},
		func(t *testing.T, cfg kit.SubagentConfig) string {
			if !strings.Contains(cfg.Prompt, "does not build") || !strings.Contains(cfg.Prompt, "TextResultt") {
				t.Errorf("second attempt has no build output:\n%s", cfg.Prompt)
			}
			writeFile(t, cfg, "tools/greet/tool.go", greetTool)
			return "fixed"
		},
	}
	res := runTool(t, localTools(t, testConfig(s), root)[0], "call-2", map[string]string{"request": "add a greet tool"})
	if res.IsError {
		t.Fatalf("modify_agent failed: %s", res.Content)
	}
	if n := s.calls(); n != 2 {
		t.Fatalf("subagent ran %d times, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(root, "tools", "greet", "tool.go")); err != nil {
		t.Fatalf("fixed change did not land: %v", err)
	}
}

// When every attempt fails to build, the live tree stays as it was.
func TestLocalGivesUpWithoutWriting(t *testing.T) {
	t.Parallel()
	root := newTree(t)
	broken := func(t *testing.T, cfg kit.SubagentConfig) string {
		writeFile(t, cfg, "main.go", "package main\n\nfunc main() { undefined() }\n")
		return "tried"
	}
	s := &scripted{t: t, steps: []func(*testing.T, kit.SubagentConfig) string{broken, broken}}
	cfg := testConfig(s)
	cfg.MaxAttempts = 2
	before, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	res := runTool(t, localTools(t, cfg, root)[0], "call-3", map[string]string{"request": "break it"})
	if !res.IsError || !strings.Contains(res.Content, "does not build after 2 attempts") {
		t.Fatalf("want a build failure, got %v: %s", res.IsError, res.Content)
	}
	after, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("main.go changed: %v", err)
	}
}

// A file the user changed while the subagent worked is a conflict, and
// nothing is written.
func TestLocalRefusesAConflict(t *testing.T) {
	t.Parallel()
	root := newTree(t)
	s := &scripted{t: t, steps: []func(*testing.T, kit.SubagentConfig) string{
		func(t *testing.T, cfg kit.SubagentConfig) string {
			writeFile(t, cfg, "instructions.md", "the subagent's version\n")
			if err := os.WriteFile(filepath.Join(root, "instructions.md"), []byte("the user's version\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return "changed the instructions"
		},
	}}
	res := runTool(t, localTools(t, testConfig(s), root)[0], "call-4", map[string]string{"request": "be terse"})
	if !res.IsError || !strings.Contains(res.Content, "the tree changed") {
		t.Fatalf("want a conflict, got %v: %s", res.IsError, res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "instructions.md"))
	if string(got) != "the user's version\n" {
		t.Fatalf("instructions.md = %q, want the user's version", got)
	}
}

// A call that stopped after the staged change was built, and before all
// files were written, writes the rest and does not run the subagent again.
func TestLocalResumesAnInterruptedApply(t *testing.T) {
	t.Parallel()
	root := newTree(t)
	s := &scripted{t: t}
	cfg := testConfig(s)
	j := &job{cfg: cfg, live: root, work: workDir(filepath.Join(root, bonnie.DefaultJournal, "selfmod"), "run-1", "call-5"), requireBuild: true}
	st, err := j.prepare("change both")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"instructions.md": "new instructions\n", "context/a.txt": "a\n"} {
		if err := writeFileAtomic(filepath.Join(j.treeDir(), name), []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	staged, err := takeSnapshot(j.treeDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Changes, st.Status, st.Summary = diffSnapshots(st.Base, staged), statusApplying, "both"
	if err := j.save(st); err != nil {
		t.Fatal(err)
	}
	// The first file landed before the interruption.
	if err := writeFileAtomic(filepath.Join(root, "context", "a.txt"), []byte("a\n")); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, localTools(t, cfg, root)[0], "call-5", map[string]string{"request": "change both"})
	if res.IsError {
		t.Fatalf("resume failed: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "instructions.md"))
	if string(got) != "new instructions\n" {
		t.Fatalf("instructions.md = %q", got)
	}
	if s.calls() != 0 {
		t.Fatalf("subagent ran on resume")
	}
}

func TestDiffShowsChangedLines(t *testing.T) {
	t.Parallel()
	before, after := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(before, "f.md"), []byte("a\nb\nc\nd\ne\nf\ng\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(after, "f.md"), []byte("a\nb\nc\nD\ne\nf\ng\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := renderDiff(before, after, []change{{Path: "f.md", Op: opModify}})
	want := "--- a/f.md\n+++ b/f.md\n@@\n b\n c\n-d\n+D\n e\n f\n"
	if got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
}

// The default workspace confines the subagent's tools to the staged copy:
// they write there, and they cannot read a file beside it.
func TestLandlockWorkspaceConfinesTheSubagent(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	staged := filepath.Join(parent, "tree")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := landlockWorkspace(context.Background(), staged)
	if err != nil {
		t.Skipf("landlock is not available: %v", err)
	}
	sb, err := p.Open(context.Background(), "self-modification")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	tools := sandbox.Tools(func(context.Context) (sandbox.Sandbox, error) { return sb, nil })
	call(t, tools, "write_file", map[string]string{"path": "instructions.md", "content": "hi\n"})
	if b, err := os.ReadFile(filepath.Join(staged, "instructions.md")); err != nil || string(b) != "hi\n" {
		t.Fatalf("write did not reach the staged copy: %v %q", err, b)
	}
	res, err := sb.Exec(context.Background(), sandbox.Command{Args: []string{"cat", filepath.Join(parent, "secret.txt")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() || strings.Contains(res.Stdout, "secret") {
		t.Fatalf("the subagent read a file outside the staged copy: %+v", res)
	}
}

// A file over the size limit stays out of the snapshot instead of failing
// it: a large data file in context/ must not stop self-modification.
func TestSnapshotSkipsOversizedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := writeFileAtomic(filepath.Join(root, "context", "big.bin"), make([]byte, maxFileBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(root, "instructions.md"), []byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	snap, err := takeSnapshot(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, ok := snap["context/big.bin"]; ok || len(snap) != 1 {
		t.Fatalf("snapshot = %v, want only instructions.md", snap)
	}
}

// A change whose path passes through a symbolic link is refused before
// anything is read, written, or deleted: a linked folder in a cloned
// repository must not take a write outside the tree.
func TestApplyRefusesSymlinkedPaths(t *testing.T) {
	t.Parallel()
	root, outside, staged := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "tools")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := writeFileAtomic(filepath.Join(staged, "tools", "x", "tool.go"), []byte("package x\n")); err != nil {
		t.Fatal(err)
	}
	hash, _ := currentHash(filepath.Join(staged, "tools", "x", "tool.go"))
	err := applyChanges(root, staged, []change{{Path: "tools/x/tool.go", Op: opAdd, Hash: hash}})
	if !errors.Is(err, errSymlink) {
		t.Fatalf("apply = %v, want errSymlink", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("a file was written outside the tree: %v", entries)
	}
}
