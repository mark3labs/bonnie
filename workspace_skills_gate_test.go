package bonnie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// These gate probes test the public Kit contract required for workspace skills.
func TestWorkspaceSkillsKitGate(t *testing.T) {
	t.Parallel()
	const name = "workspace/root/demo--0123456789abcdef"
	for _, startup := range []bool{false, true} {
		label := "empty-startup"
		if startup {
			label = "content-backed-activation"
		}
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if startup {
				err := os.WriteFile(filepath.Join(dir, "seed.md"), []byte("---\nname: seed\ndescription: Seed metadata.\n---\nSeed body."), 0600)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(dir, "absent", "assets"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "absent", "assets", "HOST_ONLY_RESOURCE"), []byte("not workspace data"), 0600); err != nil {
				t.Fatal(err)
			}
			model := fakemodel.New(fakemodel.Call("advance", `{}`), fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Say("done"))
			advance := kit.NewTool("advance", "Advance one step.", func(context.Context, struct{}) (kit.ToolOutput, error) {
				return kit.ToolOutput{Content: "advanced"}, nil
			})
			k, err := kit.NewAgent(context.Background(), kit.WithSkillActivation(), model.Option(), func(o *kit.Options) {
				o.SkipConfig = true
				o.NoContextFiles = true
				o.NoExtensions = true
				o.NoAgents = true
				o.DisableCoreTools = true
				o.Quiet = true
				o.NoSkills = false
				o.SkillsDir = dir
				o.ExtraTools = []kit.Tool{advance}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = k.Close() }()
			k.OnPrepareStep(kit.HookPriority(0), func(h kit.PrepareStepHook) *kit.PrepareStepResult {
				if h.StepNumber == 1 {
					if err := k.SetSkills([]*kit.Skill{{Name: name, NoHostIO: true, Description: "WORKSPACE_METADATA", Content: "SAVED_BODY_MARKER", Path: filepath.Join(dir, "absent", "SKILL.md")}}); err != nil {
						t.Error(err)
					}
				}
				return nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := k.Prompt(ctx, "Advance, activate, then finish."); err != nil {
				t.Fatal(err)
			}
			reqs := model.Requests()
			if len(reqs) != 3 {
				t.Fatalf("requests = %d, want 3", len(reqs))
			}
			if !startup && !reqs[0].HasTool("activate_skill") {
				t.Error("initial empty catalogue request has no activate_skill; workspace contract requires the tool even before discovery")
			}
			if !reqs[1].HasTool("activate_skill") {
				t.Error("next same-loop request has no activate_skill after SetSkills")
			}
			if !strings.Contains(reqs[1].Text(), "WORKSPACE_METADATA") {
				t.Error("next same-loop request has no new metadata")
			}
			raw, err := json.Marshal(reqs[2].Messages)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "HOST_ONLY_RESOURCE") {
				t.Error("workspace activation enumerated host resources beside the selected Path")
			}
			if !strings.Contains(string(raw), "SAVED_BODY_MARKER") {
				t.Errorf("activation did not return saved content: %s", raw)
			}
		})
	}
}

func TestWorkspaceSkillsKitVersionsAndRemoval(t *testing.T) {
	t.Parallel()
	const name = "workspace/root/demo--0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	model := fakemodel.New(fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Say("done"))
	k, err := kit.NewIsolatedAgent(context.Background(), kit.WithSkillActivation(), model.Option())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	k.OnPrepareStep(kit.HookPriority(0), func(h kit.PrepareStepHook) *kit.PrepareStepResult {
		switch h.StepNumber {
		case 1, 2:
			body := "VERSION_ONE"
			if h.StepNumber == 2 {
				body = "VERSION_TWO"
			}
			if err := k.SetSkills([]*kit.Skill{{Name: name, Description: body + "_METADATA", Content: body + "_BODY", Path: "repo/.agents/skills/demo/SKILL.md", NoHostIO: true}}); err != nil {
				return &kit.PrepareStepResult{Error: err}
			}
		case 3:
			k.RemoveSkill(name)
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := k.Prompt(ctx, "test versions"); err != nil {
		t.Fatal(err)
	}
	reqs := model.Requests()
	if len(reqs) != 5 {
		t.Fatalf("requests = %d", len(reqs))
	}
	for i, r := range reqs {
		schema := r.ToolSchemas["activate_skill"]
		if schema == "" {
			t.Fatalf("step %d: missing activation", i)
		}
		if i == 1 || i == 2 {
			if !strings.Contains(schema, name) {
				t.Fatalf("step %d: missing qualified enum", i)
			}
		} else if strings.Contains(schema, `"enum"`) {
			t.Fatalf("step %d: empty/stale enum: %s", i, schema)
		}
	}
	if strings.Contains(reqs[1].Text(), "VERSION_ONE_BODY") {
		t.Fatal("body exposed before activation")
	}
	for i, marker := range map[int]string{2: "VERSION_ONE_BODY", 3: "VERSION_TWO_BODY", 4: "unknown skill"} {
		data, err := json.Marshal(reqs[i].Messages)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), marker) {
			t.Fatalf("step %d missing %s", i, marker)
		}
	}
}

func TestWorkspaceSkillsKitPreparationAbort(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			t.Parallel()
			model := fakemodel.New(fakemodel.Say("must not be requested"))
			k, err := kit.NewIsolatedAgent(context.Background(), kit.WithSkillActivation(), model.Option())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = k.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			expected := errors.New("catalogue append failed")
			k.OnPrepareStep(kit.HookPriority(0), func(h kit.PrepareStepHook) *kit.PrepareStepResult {
				if h.Context == nil {
					t.Error("missing context")
				}
				if cancelled {
					cancel()
					return nil
				}
				return &kit.PrepareStepResult{Error: expected}
			})
			_, err = k.Prompt(ctx, "test abort")
			if cancelled {
				expected = context.Canceled
			}
			if !errors.Is(err, expected) {
				t.Fatalf("error = %v, want %v", err, expected)
			}
			if len(model.Requests()) != 0 {
				t.Fatal("provider called after preparation failed")
			}
		})
	}
}

// Tree activation must keep its file-backed freshness behavior. NoHostIO is
// opt-in for workspace entries, not a policy change for authored tree skills.
func TestWorkspaceSkillsKitTreeCompatibility(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "tree.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(source, []byte("---\nname: tree\ndescription: Tree metadata.\n---\n"+body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("ORIGINAL_TREE_BODY")
	model := fakemodel.New(fakemodel.Call("activate_skill", `{"name":"tree"}`), fakemodel.Say("done"))
	k, err := kit.NewIsolatedAgent(context.Background(), model.Option(), kit.WithSkills(source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	write("UPDATED_TREE_BODY")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := k.Prompt(ctx, "activate the authored tree skill"); err != nil {
		t.Fatal(err)
	}
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	raw, err := json.Marshal(reqs[1].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "UPDATED_TREE_BODY") || strings.Contains(string(raw), "ORIGINAL_TREE_BODY") {
		t.Fatal("ordinary file-backed tree activation no longer reopens the authored skill: got registered old body instead of updated file body")
	}
}
