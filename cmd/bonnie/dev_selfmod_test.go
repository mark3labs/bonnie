package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/internal/treetest"
	"github.com/mark3labs/bonnie/runtime"
)

// selfModMain is a tree whose model is a script inside the tree. A tree is
// its own module, so it cannot import BONNIE's internal fakemodel; the
// script answers from the request it gets, which keeps it correct in every
// process. The tree passes selfmod.Local explicitly, which also proves that
// the explicit option replaces the default the dev overlay registers: Kit
// refuses two tools with one name.
const selfModMain = `package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/sandbox"
	"github.com/mark3labs/bonnie/selfmod"
)

const greetTool = "package greet\n\nimport (\n\t\"context\"\n\n\tkit \"github.com/mark3labs/kit/pkg/kit\"\n)\n\nfunc Tool() kit.Tool {\n\treturn kit.NewTool(\"greet\", \"Greet.\", func(ctx context.Context, _ struct{}) (kit.ToolOutput, error) {\n\t\treturn kit.TextResult(\"hello\"), nil\n\t})\n}\n"

type script struct{}

func lastUserText(msgs []fantasy.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != fantasy.MessageRoleUser {
			continue
		}
		for _, p := range msgs[i].Content {
			if t, ok := p.(fantasy.TextPart); ok {
				return t.Text
			}
		}
	}
	return ""
}

func endsWithToolResult(msgs []fantasy.Message) bool {
	return len(msgs) > 0 && msgs[len(msgs)-1].Role == fantasy.MessageRoleTool
}

type reply struct {
	text       string
	tool, args string
}

func (script) answer(call fantasy.Call) reply {
	system := ""
	if len(call.Prompt) > 0 && call.Prompt[0].Role == fantasy.MessageRoleSystem {
		for _, p := range call.Prompt[0].Content {
			if t, ok := p.(fantasy.TextPart); ok {
				system += t.Text
			}
		}
	}
	if strings.Contains(system, "You change the source of a BONNIE agent") {
		if endsWithToolResult(call.Prompt) {
			return reply{text: "Added the greet tool."}
		}
		args, _ := json.Marshal(map[string]string{"path": "tools/greet/tool.go", "content": greetTool})
		return reply{tool: "write_file", args: string(args)}
	}
	switch user := lastUserText(call.Prompt); {
	case strings.Contains(user, "add a greet tool"):
		if endsWithToolResult(call.Prompt) {
			return reply{text: "modify done"}
		}
		return reply{tool: "modify_agent", args: ` + "`" + `{"request":"add a greet tool"}` + "`" + `}
	default:
		var names []string
		for _, t := range call.Tools {
			names = append(names, t.GetName())
		}
		sort.Strings(names)
		return reply{text: "tools: " + strings.Join(names, ",")}
	}
}

func (s script) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	r := s.answer(call)
	finish := fantasy.FinishReasonStop
	var content fantasy.ResponseContent
	if r.text != "" {
		content = append(content, fantasy.TextContent{Text: r.text})
	}
	if r.tool != "" {
		finish = fantasy.FinishReasonToolCalls
		content = append(content, fantasy.ToolCallContent{ToolCallID: fmt.Sprintf("call_%d", time.Now().UnixNano()), ToolName: r.tool, Input: r.args})
	}
	return &fantasy.Response{Content: content, FinishReason: finish, Usage: fantasy.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, nil
}

func (s script) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	resp, err := s.Generate(ctx, call)
	if err != nil {
		return nil, err
	}
	var parts []fantasy.StreamPart
	for _, c := range resp.Content {
		switch c := c.(type) {
		case fantasy.TextContent:
			parts = append(parts,
				fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
				fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: c.Text},
				fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
		case fantasy.ToolCallContent:
			parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: c.ToolCallID, ToolCallName: c.ToolName, ToolCallInput: c.Input})
		}
	}
	parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: resp.FinishReason, Usage: resp.Usage})
	return iter.Seq[fantasy.StreamPart](func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}), nil
}

func (script) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not scripted")
}

func (script) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not scripted")
}

func (script) Provider() string { return "script" }
func (script) Model() string    { return "model" }

func main() {
	wd, _ := os.Getwd()
	bonnie.New(
		bonnie.WithAddr("127.0.0.1:0"),
		bonnie.WithSandbox(sandbox.Local(sandbox.WithLocalRoot(filepath.Join(wd, ".bonnie", "workspaces")))),
		bonnie.WithKit(
			kit.WithProvider("script", func(context.Context, *kit.ProviderConfig, string) (*kit.ProviderResult, error) {
				return &kit.ProviderResult{Model: script{}}, nil
			}),
			kit.WithModel("script/model"),
			func(o *kit.Options) {
				o.SkipConfig, o.NoContextFiles, o.NoSkills = true, true, true
				o.NoExtensions, o.NoAgents, o.Quiet = true, true, true
			},
		),
		selfmod.Local(selfmod.Config{Workspace: func(_ context.Context, dir string) (sandbox.Provider, error) {
			p := sandbox.Local()
			return p, p.UseSharedDirectory(dir)
		}}),
	).Serve()
}
`

// TestDevSelfModificationCrossesARestart is the durability claim of local
// self-modification. A run asks the agent to add a tool. The change lands in
// the tree while the first child serves, and that child still has the old
// tools. After the dev loop restarts, the next message on the same run — a
// new process, with only the journal and the tree shared — is answered by a
// build that has the new tool.
func TestDevSelfModificationCrossesARestart(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "agent")
	if _, err := agent.Scaffold(root, agent.InitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(selfModMain), 0o644); err != nil {
		t.Fatal(err)
	}
	treetest.LinkToCheckout(t, root)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), append(must(os.ReadFile(filepath.Join(root, "go.mod"))), []byte("\nrequire charm.land/fantasy v0.0.0\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	pinFantasy(t, root)

	var log syncBuffer
	// Port 0: the dev loop's walk from 8080 races the other tests that
	// start children, and the restart below reads the new address.
	d := newDevServer(root, devOpts{shutdown: 10 * time.Second, addr: "127.0.0.1:0"})
	d.env = treetest.BuildEnv()
	d.log = &log
	if err := d.restart(); err != nil {
		t.Fatalf("first build: %v\n%s", err, log.String())
	}
	defer d.stopChild()
	addr := waitForStart(t, &log, 1)

	started := postRun(t, addr, "/bonnie/v1/runs", map[string]any{"text": "add a greet tool"})
	if started.State != string(runtime.RunCompleted) || started.Response != "modify done" {
		t.Fatalf("modify turn: %+v\n%s", started, log.String())
	}
	if _, err := os.Stat(filepath.Join(root, "tools", "greet", "tool.go")); err != nil {
		t.Fatalf("the change did not reach the tree: %v", err)
	}
	before := postRun(t, addr, "/bonnie/v1/runs/"+started.RunID, map[string]any{"text": "which tools?"})
	if strings.Contains(before.Response, "greet") || strings.Count(before.Response, "modify_agent") != 1 {
		t.Fatalf("old build tools: %q", before.Response)
	}

	if err := d.restart(); err != nil {
		t.Fatalf("rebuild: %v\n%s", err, log.String())
	}
	addr = waitForStart(t, &log, 2)
	after := postRun(t, addr, "/bonnie/v1/runs/"+started.RunID, map[string]any{"text": "which tools now?"})
	if !strings.Contains(after.Response, "greet") || strings.Count(after.Response, "modify_agent") != 1 {
		t.Fatalf("new build tools: %q", after.Response)
	}
}

// A build with --no-self-modify has no overlay, and a default build has one
// that adds only the generated file to the tree's main package.
func TestDevSelfModifyOverlay(t *testing.T) {
	t.Parallel()
	root := buildHermeticTree(t)
	d := newDevServer(root, devOpts{noSelfModify: true})
	d.env = treetest.BuildEnv()
	if path, err := d.selfModifyOverlay(); err != nil || path != "" {
		t.Fatalf("--no-self-modify: overlay %q, %v", path, err)
	}
	d = newDevServer(root, devOpts{})
	d.env = treetest.BuildEnv()
	path, err := d.selfModifyOverlay()
	if err != nil || path == "" {
		t.Fatalf("default: overlay %q, %v", path, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, selfModifyFile); !strings.Contains(string(b), want) {
		t.Fatalf("overlay %s does not add %s", b, want)
	}
	if _, err := os.Stat(filepath.Join(root, selfModifyFile)); !os.IsNotExist(err) {
		t.Fatalf("the overlay wrote into the tree: %v", err)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// pinFantasy replaces the placeholder fantasy require with the version this
// checkout uses, so the tree resolves it from the module cache.
func pinFantasy(t *testing.T, root string) {
	t.Helper()
	mod := must(os.ReadFile(filepath.Join(treetest.Root(), "go.mod")))
	version := ""
	for line := range strings.SplitSeq(string(mod), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "charm.land/fantasy" {
			version = f[1]
		}
	}
	if version == "" {
		t.Fatal("no fantasy version in go.mod")
	}
	path := filepath.Join(root, "go.mod")
	body := strings.Replace(string(must(os.ReadFile(path))), "charm.land/fantasy v0.0.0", "charm.land/fantasy "+version, 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
