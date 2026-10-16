package bonnie

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// The gate uses actual same-loop requests, not only the live Kit catalogue.
func TestWorkspaceSkillsPublicActivationGate(t *testing.T) {
	t.Parallel()
	const name = "workspace/root/review--0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "HOST_ONLY_RESOURCE"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "SKILL.md") // Intentionally absent on the host.
	model := fakemodel.New(fakemodel.Call("advance", `{}`), fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Call("activate_skill", `{"name":"`+name+`"}`), fakemodel.Say("done"))
	advance := kit.NewTool("advance", "Advance.", func(context.Context, struct{}) (kit.ToolOutput, error) {
		return kit.ToolOutput{Content: "advanced"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	k, err := kit.NewIsolatedAgent(ctx, model.Option(), kit.WithSkillActivation(), kit.WithExtraTools(advance))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	k.OnPrepareStep(kit.HookPriorityNormal, func(h kit.PrepareStepHook) *kit.PrepareStepResult {
		if h.StepNumber == 1 || h.StepNumber == 2 {
			body := "BODY_VERSION_ONE"
			if h.StepNumber == 2 {
				body = "BODY_VERSION_TWO"
			}
			if err := k.SetSkills([]*kit.Skill{{Name: name, Description: "METADATA_VERSION_" + body, Content: body, Path: source, NoHostIO: true}}); err != nil {
				return &kit.PrepareStepResult{Error: err}
			}
		}
		if h.StepNumber == 3 {
			if !k.RemoveSkill(name) {
				t.Error("remove failed")
			}
		}
		return nil
	})
	if _, err := k.Prompt(ctx, "advance then activate"); err != nil {
		t.Fatal(err)
	}
	reqs := model.Requests()
	if len(reqs) != 5 {
		t.Fatalf("requests=%d", len(reqs))
	}
	for i, r := range reqs {
		if !r.HasTool("activate_skill") {
			t.Errorf("request %d missing activation", i)
		}
	}
	if strings.Contains(reqs[0].Text(), "BODY_VERSION") {
		t.Fatal("initial request leaked content")
	}
	if !strings.Contains(string(reqs[1].ToolSchemas["activate_skill"]), name) || !strings.Contains(reqs[1].System(), "METADATA_VERSION_") {
		t.Fatal("same-loop schema/metadata absent")
	}
	// Description markers contain BODY_VERSION; inspect the selected body separately.
	if strings.Contains(reqs[1].Text(), "<skill_content") {
		t.Fatal("body arrived before activation")
	}
	for i, want := range map[int]string{2: "BODY_VERSION_ONE", 3: "BODY_VERSION_TWO", 4: "unknown skill"} {
		raw, err := json.Marshal(reqs[i].Messages)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("request %d missing %s", i, want)
		}
		if strings.Contains(string(raw), "HOST_ONLY_RESOURCE") {
			t.Fatal("host resources leaked")
		}
	}
	if strings.Contains(reqs[3].System(), name) || strings.Contains(string(reqs[3].ToolSchemas["activate_skill"]), name) {
		t.Fatal("removed skill still advertised")
	}
}

// A failed durable selection must stop before the provider receives a request.
func TestWorkspaceSkillsPreparationAborts(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "cancel"}[cancelled], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			model := fakemodel.New(fakemodel.Say("must not run"))
			k, err := kit.NewIsolatedAgent(ctx, model.Option(), kit.WithSkillActivation())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = k.Close() }()
			sentinel := errors.New("catalogue append failed")
			k.OnPrepareStep(kit.HookPriorityNormal, func(h kit.PrepareStepHook) *kit.PrepareStepResult {
				if cancelled {
					cancel()
					return &kit.PrepareStepResult{Error: h.Context.Err()}
				}
				return &kit.PrepareStepResult{Error: sentinel}
			})
			_, err = k.Prompt(ctx, "test")
			want := sentinel
			if cancelled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v", err)
			}
			if len(model.Requests()) != 0 {
				t.Fatal("provider ran after failed preparation")
			}
		})
	}
}
