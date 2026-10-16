package bonnie

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/internal/workspaceskills"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

const workspaceSource = "repo/.agents/skills/review/SKILL.md"
const workspaceBody = "---\nname: review\ndescription: Repository review metadata.\nwhen: always\n---\nSAVED_WORKSPACE_BODY\n"

func TestWorkspaceSkillsDiscoveryWithinToolLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	provider := sandbox.Local(sandbox.WithLocalRoot(root))
	name := workspaceskills.Identity(workspaceSource, "repo", "review")
	model := fakemodel.New(fakemodel.Call("clone", `{}`), fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Say("done"))
	clone := kit.NewTool("clone", "Create a repository fixture.", func(ctx context.Context, _ struct{}) (kit.ToolOutput, error) {
		sb, err := provider.Open(ctx, "discovery")
		if err != nil {
			return kit.ToolOutput{}, err
		}
		if err := sb.WriteFile(ctx, "repo/.git", []byte("gitdir: ignored")); err != nil {
			return kit.ToolOutput{}, err
		}
		err = sb.WriteFile(ctx, workspaceSource, []byte(workspaceBody))
		return kit.ToolOutput{Content: "cloned"}, err
	})
	cfg := resolve(WithSkills(""), WithWorkspaceSkills(WorkspaceSkills{}))
	factory := cfg.managedFactory(provider, append([]kit.Option{kit.Isolated(), kit.WithSkillActivation(), model.Option(), kit.WithExtraTools(clone)}, cfg.kitOptions("", "")...))
	run, err := runtime.NewRunner(runtime.NewMemoryJournal(), factory).Start(ctx, "discovery", runtime.Input{Text: "clone and review"})
	if err != nil || run.State != runtime.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	reqs := model.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests=%d", len(reqs))
	}
	if strings.Contains(reqs[0].Text(), "Repository review metadata") || strings.Contains(reqs[1].Text(), "SAVED_WORKSPACE_BODY") {
		t.Fatal("progressive disclosure failed")
	}
	if !strings.Contains(reqs[1].System(), name) || !strings.Contains(reqs[1].System(), "repo") || !strings.Contains(string(reqs[1].ToolSchemas["activate_skill"]), name) {
		t.Fatal("same-loop metadata/schema missing")
	}
	raw, err := json.Marshal(reqs[2].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "SAVED_WORKSPACE_BODY") || !strings.Contains(string(raw), "Selected version:") {
		t.Fatal("saved activation missing")
	}
}

func TestWorkspaceSkillsRestoreSavedVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	journalDir := t.TempDir()
	provider := sandbox.Local(sandbox.WithLocalRoot(root))
	sb, err := provider.Open(ctx, "restore")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(ctx, "repo/.git", []byte("gitdir: ignored")); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(ctx, workspaceSource, []byte(workspaceBody)); err != nil {
		t.Fatal(err)
	}
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := runtime.OpenSQLiteJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := resolve(WithSkills(""), WithWorkspaceSkills(WorkspaceSkills{}))
	model := fakemodel.New(fakemodel.Say("saved"))
	factory := cfg.managedFactory(provider, append([]kit.Option{kit.Isolated(), kit.WithSkillActivation(), model.Option()}, cfg.kitOptions("", "")...))
	if _, err := runtime.NewRunner(journal, factory).Start(ctx, "restore", runtime.Input{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "restore", workspaceSource)); err != nil {
		t.Fatal(err)
	}
	// The second process shares only durable storage and the workspace directory.
	journalB, err := runtime.OpenSQLiteJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journalB.Close() }()
	modelB := fakemodel.New(fakemodel.Call("activate_skill", `{"name":"`+workspaceskills.Identity(workspaceSource, "repo", "review")+`"}`), fakemodel.Say("restored"))
	cfgB := resolve(WithSkills(""), WithWorkspaceSkills(WorkspaceSkills{}))
	factoryB := cfgB.managedFactory(sandbox.Local(sandbox.WithLocalRoot(root)), append([]kit.Option{kit.Isolated(), kit.WithSkillActivation(), modelB.Option()}, cfgB.kitOptions("", "")...))
	if _, err := runtime.NewRunner(journalB, factoryB).Start(ctx, "restore", runtime.Input{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	reqs := modelB.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests=%d", len(reqs))
	}
	if !strings.Contains(reqs[0].System(), "Repository review metadata") {
		t.Fatal("recorded catalogue not restored")
	}
	raw, err := json.Marshal(reqs[1].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "SAVED_WORKSPACE_BODY") {
		t.Fatal("saved body lost after source removal")
	}
	if strings.Contains(reqs[1].System(), "Repository review metadata") {
		t.Fatal("deletion did not become active at following boundary")
	}
}

func TestWorkspaceSkillsOptionCopiesAndValidates(t *testing.T) {
	t.Parallel()
	exclude := []string{"archives/**"}
	option := WithWorkspaceSkills(WorkspaceSkills{Exclude: exclude})
	exclude[0] = "changed"
	c := resolve(option)
	if c.workspaceSkills.Exclude[0] != "archives/**" {
		t.Fatal("option retained caller slice")
	}
	c.workspaceSkills.Exclude[0] = "local"
	if resolve(option).workspaceSkills.Exclude[0] != "archives/**" {
		t.Fatal("config retained option slice")
	}
	if resolve(option, WithWorkspaceSkills(WorkspaceSkills{MaxDepth: 4})).workspaceSkills.MaxDepth != 4 {
		t.Fatal("option did not replace")
	}
	if conflict := resolve(option).agentConflicts(); conflict != "WithWorkspaceSkills" {
		t.Fatalf("conflict=%s", conflict)
	}
	for _, p := range []WorkspaceSkills{{MaxDepth: -1}, {MaxSkills: -1}, {MaxSkillBytes: -1}, {MaxMetadataBytes: -1}, {Exclude: []string{"../x"}}} {
		if _, err := resolve(WithWorkspaceSkills(p)).prepareRuntime(context.Background()); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}
