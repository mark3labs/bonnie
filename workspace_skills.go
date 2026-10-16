package bonnie

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/bonnie/internal/workspaceskills"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// WorkspaceSkills controls opt-in discovery in each run's sandbox. Zero limits
// select defaults: depth 12, 100 skills, 128 KiB per file, 32 KiB metadata.
// Exclude contains workspace-relative directory globs; ** matches segments.
// Mandatory exclusions and Git ignored parents cannot be re-included.
type WorkspaceSkills struct {
	Exclude          []string
	MaxDepth         int
	MaxSkills        int
	MaxSkillBytes    int64
	MaxMetadataBytes int64
}

// WithWorkspaceSkills enables sandbox discovery before model requests. It
// opens compute at startup, even without a model tool call. Repeated options
// replace the policy. It cannot be combined with WithAgentFactory.
func WithWorkspaceSkills(policy WorkspaceSkills) Option {
	policy.Exclude = append([]string(nil), policy.Exclude...)
	return func(c *config) {
		copy := policy
		copy.Exclude = append([]string(nil), policy.Exclude...)
		c.workspaceSkills = &copy
	}
}
func (p WorkspaceSkills) policy() (workspaceskills.Policy, error) {
	return workspaceskills.Normalize(workspaceskills.Policy{Exclude: p.Exclude, MaxDepth: p.MaxDepth, MaxSkills: p.MaxSkills, MaxSkillBytes: p.MaxSkillBytes, MaxMetadataBytes: p.MaxMetadataBytes})
}
func (c *config) setupWorkspaceSkills(ctx context.Context, k *kit.Kit, s *runtime.Session, open sandbox.Opener) error {
	if c.workspaceSkills == nil {
		return nil
	}
	policy, err := c.workspaceSkills.policy()
	if err != nil {
		return err
	}
	sb, err := open(ctx)
	if err != nil {
		return err
	}
	reader, ok := sb.(sandbox.DiscoveryReader)
	if !ok {
		return fmt.Errorf("bonnie: enabled workspace skills require secure sandbox DiscoveryReader")
	}
	tree := k.GetSkills()
	protected := map[string]bool{}
	for _, skill := range tree {
		protected[skill.Name] = true
	}
	var mu sync.Mutex
	selected := s.WorkspaceSkills()
	restored := selected.Revision > 0
	publish := func(entries []workspaceskills.Entry) error {
		skills := append([]*kit.Skill(nil), tree...)
		for _, e := range entries {
			if protected[e.Name] {
				return fmt.Errorf("bonnie: saved workspace skill conflicts with protected authored name %q", e.Name)
			}
			skill := workspaceskills.MetadataSkill(e)
			skill.Content = fmt.Sprintf("Workspace identity: %s\nVersion SHA-256: %s\nSource: %s\nScope: %s\nBase directory: %s\n\n%s", e.Name, e.Hash, e.Source, e.Scope, e.Base, e.Body)
			skills = append(skills, skill)
		}
		return k.SetSkills(skills)
	}
	diagnostics := map[string]bool{}
	scan := func(ctx context.Context) workspaceskills.Result {
		r := workspaceskills.Scan(ctx, reader, policy, protected)
		for _, d := range r.Diagnostics {
			if !diagnostics[d] && len(diagnostics) < 128 {
				diagnostics[d] = true
				if c.activityLogger != nil {
					c.activityLogger.LogActivity(runtime.Event{RunID: s.RunID(), Type: "workspace_skills_diagnostic", Time: time.Now(), Text: d})
				}
			}
		}
		return r
	}
	pending := scan(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if restored {
		if err := publish(selected.Entries); err != nil {
			return err
		}
	}
	first := true
	// Run before caller replacement hooks so their first non-nil result cannot
	// bypass durability. Returning nil preserves their message/tool overrides.
	k.OnPrepareStep(kit.HookPriorityHigh-1, func(h kit.PrepareStepHook) *kit.PrepareStepResult {
		mu.Lock()
		defer mu.Unlock()
		requestCtx := h.Context
		if err := s.WorkspaceSkillsError(); err != nil {
			return &kit.PrepareStepResult{Error: err}
		}
		var candidate workspaceskills.Result
		if first || pending.Complete && restored {
			candidate = pending
			if !first {
				restored = false
			}
		} else {
			candidate = scan(requestCtx)
		}
		if err := requestCtx.Err(); err != nil {
			return &kit.PrepareStepResult{Error: err}
		}
		if selected.Revision == 0 && !candidate.Complete {
			next, err := s.SelectWorkspaceSkills(requestCtx, nil)
			if err != nil {
				return &kit.PrepareStepResult{Error: err}
			}
			selected = next
		}
		if (!first || !restored) && candidate.Complete {
			next, err := s.SelectWorkspaceSkills(requestCtx, candidate.Entries)
			if err != nil {
				return &kit.PrepareStepResult{Error: err}
			}
			selected = next
		}
		first = false
		if err := publish(selected.Entries); err != nil {
			return &kit.PrepareStepResult{Error: err}
		}
		return nil
	})
	return nil
}
