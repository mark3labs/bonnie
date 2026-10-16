package bonnie

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestWorkspaceSkillsManagedBoundaryAndRestore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	provider := sandbox.Local(sandbox.WithLocalRoot(root))
	journalDir := t.TempDir()
	source := "repo/.agents/skills/demo/SKILL.md"
	body := "---\nname: demo\ndescription: DEMO_METADATA\n---\nSAVED_BODY"
	name := ""
	model := fakemodel.New(fakemodel.Call("create", `{}`), fakemodel.Say("discovered"))
	cfg := resolve(WithSkills(""), WithWorkspaceSkills(WorkspaceSkills{}), WithToolFactory(func(ctx context.Context, scope ToolScope) ([]kit.Tool, error) {
		return []kit.Tool{kit.NewTool("create", "Create repository skill", func(ctx context.Context, _ struct{}) (kit.ToolOutput, error) {
			_, err := scope.Exec(ctx, sandbox.Command{Args: []string{"sh", "-c", `mkdir -p repo/.agents/skills/demo; printf '%s' "$1" > repo/.agents/skills/demo/SKILL.md`, "create", body}})
			return kit.ToolOutput{Content: "created"}, err
		})}, nil
	}))
	opts := []kit.Option{kit.Isolated(), kit.WithSkillActivation(), model.Option()}
	journal, err := runtime.OpenSQLiteJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = runtime.NewRunner(journal, cfg.managedFactory(provider, opts)).Start(ctx, "workspace", runtime.Input{Text: "create skill"})
	if err != nil {
		t.Fatal(err)
	}
	requests := model.Requests()
	if len(requests) != 2 || !strings.Contains(requests[1].Text(), "DEMO_METADATA") {
		t.Fatalf("metadata absent: %+v", requests)
	}
	if strings.Contains(requests[1].Text(), "SAVED_BODY") {
		t.Fatal("body before activation")
	}
	s, err := runtime.Restore(ctx, "workspace", journal)
	if err != nil {
		t.Fatal(err)
	}
	catalogue := s.WorkspaceSkills()
	if len(catalogue.Entries) != 1 {
		t.Fatalf("catalogue=%+v", catalogue)
	}
	name = catalogue.Entries[0].Name
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	host := provider.WorkingDir("workspace")
	if err := os.Remove(filepath.Join(host, source)); err != nil {
		t.Fatal(err)
	}
	second := fakemodel.New(fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Say("activated saved version"))
	reopened, err := runtime.OpenSQLiteJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	secondCfg := resolve(WithSkills(""), WithWorkspaceSkills(WorkspaceSkills{}))
	_, err = runtime.NewRunner(reopened, secondCfg.managedFactory(sandbox.Local(sandbox.WithLocalRoot(root)), []kit.Option{kit.Isolated(), kit.WithSkillActivation(), second.Option()})).Start(ctx, "workspace", runtime.Input{Text: "activate saved skill"})
	if err != nil {
		t.Fatal(err)
	}
	reqs := second.Requests()
	if !strings.Contains(reqs[0].ToolSchemas["activate_skill"], name) {
		t.Fatal("saved schema missing")
	}
	raw, _ := json.Marshal(reqs[1].Messages)
	if !strings.Contains(string(raw), "SAVED_BODY") {
		t.Fatalf("saved activation absent: %s", raw)
	}
}
func TestWorkspacePolicyCopyAndReplacement(t *testing.T) {
	t.Parallel()
	p := WorkspaceSkills{Exclude: []string{"first"}}
	option := WithWorkspaceSkills(p)
	p.Exclude[0] = "changed"
	c := resolve(option)
	if c.workspaceSkills.Exclude[0] != "first" {
		t.Fatal("option did not copy")
	}
	c2 := resolve(option, WithWorkspaceSkills(WorkspaceSkills{MaxSkills: 3}))
	if c2.workspaceSkills.MaxSkills != 3 || len(c2.workspaceSkills.Exclude) != 0 {
		t.Fatal("option did not replace")
	}
	if resolve(option, WithAgentFactory(func(context.Context, *runtime.Session) (runtime.Agent, error) { return nil, nil })).agentConflicts() != "WithWorkspaceSkills" {
		t.Fatal("missing factory conflict")
	}
}

func TestWorkspaceSkillsPublicOptionPath(t *testing.T) {
	t.Parallel()
	model := fakemodel.New(fakemodel.Say("public path"))
	agent := New(WithInstructions(""), WithSkills(""), WithContextFiles(""), WithJournal(t.TempDir()), WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))), WithWorkspaceSkills(WorkspaceSkills{}), WithKit(kit.Isolated(), model.Option()))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := agent.WithRuntime(ctx, func(ctx context.Context, rt *Runtime) error {
		_, err := rt.Runner().Start(ctx, "public", runtime.Input{Text: "test"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requests := model.Requests()
	if len(requests) != 1 || !requests[0].HasTool("activate_skill") {
		t.Fatal("public option did not enable empty activation")
	}
}
