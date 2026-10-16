package bonnie

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/workspaceskills"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// WorkspaceSkills controls opt-in recursive workspace discovery. Zero values
// select depth 12, 100 skills, 128 KiB per file and 32 KiB of metadata.
// Exclude adds workspace-relative directory globs (*, ?, classes, whole **
// segments). Negative limits and invalid patterns are rejected at startup.
// Discovery never follows symlinks or enables Kit's home discovery.
type WorkspaceSkills struct {
	// Exclude adds directory globs to the mandatory exclusions and Git rules.
	Exclude []string
	// MaxDepth bounds directory segments below the root (root depth is zero).
	MaxDepth int
	// MaxSkills bounds the selected workspace catalogue.
	MaxSkills int
	// MaxSkillBytes bounds original bytes, including frontmatter.
	MaxSkillBytes int64
	// MaxMetadataBytes bounds exact escaped catalogue text, not skill bodies.
	MaxMetadataBytes int64
}

// WithWorkspaceSkills enables untrusted repository metadata in model context.
// Only .agents/skills/<bundle>/SKILL.md and .kit/skills/<bundle>/SKILL.md
// qualify. Tree skills remain separate, including with WithSkills("").
// This option opens the sandbox at startup and cannot use WithAgentFactory.
// Repeated options replace the policy. Bundled assets remain live, not saved.
// Local, Landlock and custom SkillDiscovery sandboxes are supported. Built-in
// Docker and Microsandbox discovery is deferred and returns an explicit error.
func WithWorkspaceSkills(policy WorkspaceSkills) Option {
	policy.Exclude = slices.Clone(policy.Exclude)
	return func(c *config) {
		copy := policy
		copy.Exclude = slices.Clone(policy.Exclude)
		c.workspaceSkills = &copy
	}
}
func (c *config) workspacePolicy() (workspaceskills.Policy, error) {
	p := c.workspaceSkills
	return workspaceskills.Normalize(workspaceskills.Policy{Exclude: p.Exclude, MaxDepth: p.MaxDepth, MaxSkills: p.MaxSkills, MaxSkillBytes: p.MaxSkillBytes, MaxMetadataBytes: p.MaxMetadataBytes})
}
func (c *config) setupWorkspaceSkills(ctx context.Context, k *kit.Kit, s *runtime.Session, open sandbox.Opener) error {
	policy, err := c.workspacePolicy()
	if err != nil {
		return err
	}
	sb, err := open(ctx)
	if err != nil {
		return err
	}
	source, ok := sb.(sandbox.SkillDiscovery)
	if !ok {
		return fmt.Errorf("bonnie: sandbox lacks secure workspace skill discovery")
	}
	tree := k.GetSkills()
	protected := map[string]bool{}
	for _, skill := range tree {
		protected[skill.Name] = true
	}
	apply := func(skills []workspaceskills.Skill) error {
		combined := slices.Clone(tree)
		for _, v := range skills {
			if protected[v.Name] {
				return fmt.Errorf("bonnie: restored workspace skill conflicts with authored name")
			}
			combined = append(combined, workspaceskills.ActivationSkill(v))
		}
		return k.SetSkills(combined)
	}
	saved, restored := s.WorkspaceSkillCatalogue()
	if restored {
		if err := apply(saved); err != nil {
			return err
		}
	}
	scan := func(ctx context.Context) (workspaceskills.Result, error) {
		result, err := workspaceskills.Scan(ctx, source, policy, protected)
		if err != nil {
			return result, err
		}
		if c.activityLogger != nil {
			for _, note := range result.Diagnostics {
				payload, marshalErr := json.Marshal(map[string]string{"workspace_skills": note})
				if marshalErr != nil {
					return result, marshalErr
				}
				c.activityLogger.LogActivity(runtime.Event{RunID: s.RunID(), Type: string(kit.EventWarnings), Text: note, Data: payload})
			}
		}
		return result, nil
	}
	pending, err := scan(ctx)
	if err != nil {
		return err
	}
	prepareGate := make(chan struct{}, 1)
	first := true
	k.OnPrepareStep(kit.HookPriorityLow, func(h kit.PrepareStepHook) *kit.PrepareStepResult {
		select {
		case prepareGate <- struct{}{}:
			defer func() { <-prepareGate }()
		case <-h.Context.Done():
			return &kit.PrepareStepResult{Error: h.Context.Err()}
		}
		var result workspaceskills.Result
		var err error
		if first {
			first = false
			result = pending
			if restored {
				return nil
			}
		} else {
			result, err = scan(h.Context)
		}
		if err != nil {
			return &kit.PrepareStepResult{Error: err}
		}
		if !result.Complete {
			return nil
		}
		if err := s.SelectWorkspaceSkills(h.Context, result.Skills); err != nil {
			return &kit.PrepareStepResult{Error: err}
		}
		if err := apply(result.Skills); err != nil {
			return &kit.PrepareStepResult{Error: err}
		}
		return nil
	})
	return nil
}
