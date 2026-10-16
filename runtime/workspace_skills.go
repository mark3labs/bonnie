package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/mark3labs/bonnie/internal/workspaceskills"
)

// RecordWorkspaceSkills stores a run-scoped selected catalogue, not a message.
const RecordWorkspaceSkills RecordKind = "workspace_skills"

type workspaceCatalogue struct {
	Schema   int                     `json:"schema"`
	Revision uint64                  `json:"revision"`
	Skills   []workspaceskills.Skill `json:"skills"`
}

// WorkspaceSkillVersion is one saved workspace instruction version. Assets
// are not included: sandbox tools always read the live workspace assets.
type WorkspaceSkillVersion = workspaceskills.Skill

// WorkspaceSkillCatalogue returns a detached copy of the selected versions.
func (s *Session) WorkspaceSkillCatalogue() ([]WorkspaceSkillVersion, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.workspaceSkills == nil {
		return nil, false
	}
	out := append([]WorkspaceSkillVersion(nil), s.workspaceSkills.Skills...)
	for i := range out {
		out[i].Original = append([]byte(nil), out[i].Original...)
	}
	return out, true
}

// SelectWorkspaceSkills appends a changed full snapshot before exposing it.
// A write failure leaves the previous selection unchanged.
func (s *Session) SelectWorkspaceSkills(ctx context.Context, skills []WorkspaceSkillVersion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspaceSkills != nil && reflect.DeepEqual(s.workspaceSkills.Skills, skills) {
		return nil
	}
	revision := uint64(1)
	if s.workspaceSkills != nil {
		revision = s.workspaceSkills.Revision + 1
	}
	snapshot := workspaceCatalogue{Schema: 1, Revision: revision, Skills: skills}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	validated, err := decodeWorkspaceCatalogue(data)
	if err != nil {
		return err
	}
	if _, err := s.journal.Append(ctx, Record{RunID: s.runID, Kind: RecordWorkspaceSkills, Timestamp: now(), Payload: data}); err != nil {
		return fmt.Errorf("bonnie: append workspace skill selection: %w", err)
	}
	s.workspaceSkills = validated
	return nil
}
func decodeWorkspaceCatalogue(data []byte) (*workspaceCatalogue, error) {
	// Full snapshots have a fixed defensive decode ceiling independent of policy.
	if len(data) > 64<<20 {
		return nil, fmt.Errorf("bonnie: workspace catalogue exceeds decode limit")
	}
	var c workspaceCatalogue
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("bonnie: corrupt workspace catalogue: %w", err)
	}
	if c.Schema != 1 || c.Revision == 0 || len(c.Skills) > 10000 {
		return nil, fmt.Errorf("bonnie: invalid workspace catalogue schema or limits")
	}
	names := map[string]bool{}
	for _, s := range c.Skills {
		if len(s.Original) > 16<<20 {
			return nil, fmt.Errorf("bonnie: workspace content exceeds decode limit")
		}
		parsed, err := workspaceskills.Parse(s.Source, s.Scope, s.Original)
		if err != nil || parsed == nil || !reflect.DeepEqual(*parsed, s) || names[s.Name] {
			return nil, fmt.Errorf("bonnie: corrupt workspace skill version")
		}
		names[s.Name] = true
	}
	return &c, nil
}
