package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/mark3labs/bonnie/internal/workspaceskills"
)

// RecordWorkspaceSkills is run-scoped instruction content, not a conversation entry.
const RecordWorkspaceSkills RecordKind = "workspace_skills"

// WorkspaceSkill is one selected durable workspace version.
type WorkspaceSkill = workspaceskills.Entry

// WorkspaceCatalogue is a full append-only selected catalogue snapshot.
type WorkspaceCatalogue struct {
	Schema   int              `json:"schema"`
	Revision uint64           `json:"revision"`
	Entries  []WorkspaceSkill `json:"entries"`
}

func validateCatalogue(c WorkspaceCatalogue) error {
	if c.Schema != 1 || c.Revision == 0 {
		return fmt.Errorf("bonnie: invalid workspace catalogue schema/revision")
	}
	if len(c.Entries) > 10000 {
		return fmt.Errorf("bonnie: excessive workspace catalogue entries")
	}
	names := map[string]bool{}
	sources := map[string]bool{}
	for _, e := range c.Entries {
		if err := workspaceskills.ValidateEntry(e); err != nil {
			return fmt.Errorf("bonnie: corrupt workspace skill: %w", err)
		}
		hash := sha256.Sum256(e.Original)
		if len(e.Original) > 64<<20 || e.Hash != hex.EncodeToString(hash[:]) || e.Name != workspaceskills.Identity(e.Source, e.Scope, e.DeclaredName) || e.Body == "" || names[e.Name] || sources[e.Source] {
			return fmt.Errorf("bonnie: corrupt workspace skill version")
		}
		names[e.Name] = true
		sources[e.Source] = true
	}
	return nil
}

// WorkspaceSkillsError returns a fatal workspace persistence failure. Enabled
// managers check it before the next request; ordinary runs are unchanged.
func (s *Session) WorkspaceSkillsError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workspaceDurabilityError
}

// WorkspaceSkills returns a detached copy of the last selected catalogue.
func (s *Session) WorkspaceSkills() WorkspaceCatalogue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneCatalogue(s.workspaceCatalogue)
}
func cloneCatalogue(c WorkspaceCatalogue) WorkspaceCatalogue {
	c.Entries = append([]WorkspaceSkill(nil), c.Entries...)
	for i := range c.Entries {
		c.Entries[i].Original = append([]byte(nil), c.Entries[i].Original...)
	}
	return c
}

// SelectWorkspaceSkills appends changed selections before publication.
func (s *Session) SelectWorkspaceSkills(ctx context.Context, entries []WorkspaceSkill) (WorkspaceCatalogue, error) {
	s.workspaceSelect.Lock()
	defer s.workspaceSelect.Unlock()
	previous := s.WorkspaceSkills()
	if previous.Revision > 0 && reflect.DeepEqual(previous.Entries, entries) {
		return previous, nil
	}
	c := WorkspaceCatalogue{Schema: 1, Revision: previous.Revision + 1, Entries: entries}
	if err := validateCatalogue(c); err != nil {
		return WorkspaceCatalogue{}, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return WorkspaceCatalogue{}, err
	}
	if len(raw) > 64<<20 {
		return WorkspaceCatalogue{}, fmt.Errorf("bonnie: workspace catalogue exceeds durable record limit")
	}
	if _, err := s.journal.Append(ctx, Record{RunID: s.runID, Kind: RecordWorkspaceSkills, Timestamp: now(), Payload: raw}); err != nil {
		return WorkspaceCatalogue{}, fmt.Errorf("bonnie: append workspace catalogue: %w", err)
	}
	s.mu.Lock()
	s.workspaceCatalogue = cloneCatalogue(c)
	s.mu.Unlock()
	return cloneCatalogue(c), nil
}
func (s *Session) restoreWorkspaceCatalogue(raw []byte) error {
	if len(raw) > 64<<20 {
		return fmt.Errorf("bonnie: oversized workspace catalogue")
	}
	var c WorkspaceCatalogue
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("bonnie: decode workspace catalogue: %w", err)
	}
	if err := validateCatalogue(c); err != nil {
		return err
	}
	if c.Revision != s.workspaceCatalogue.Revision+1 {
		return fmt.Errorf("bonnie: nonmonotonic workspace catalogue revision")
	}
	s.workspaceCatalogue = c
	return nil
}
