package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mark3labs/bonnie/internal/workspaceskills"
)

func workspaceEntry() WorkspaceSkill {
	raw := []byte("---\nname: demo\ndescription: Demo metadata\n---\nBODY")
	hash := sha256.Sum256(raw)
	source := "repo/.agents/skills/demo/SKILL.md"
	return WorkspaceSkill{Name: workspaceskills.Identity(source, "repo", "demo"), DeclaredName: "demo", Description: "Demo metadata", Source: source, Scope: "repo", Base: "repo/.agents/skills/demo", Hash: hex.EncodeToString(hash[:]), Original: raw, Body: "BODY"}
}
func TestWorkspaceCatalogueRestoresAndDeduplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	j, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession("skills", j)
	entries := []WorkspaceSkill{workspaceEntry()}
	for range 2 {
		if _, err := s.SelectWorkspaceSkills(ctx, entries); err != nil {
			t.Fatal(err)
		}
	}
	records, err := j.Replay(ctx, "skills")
	if err != nil || len(records) != 1 || records[0].EntryID != "" {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	restored, err := Restore(ctx, "skills", second)
	if err != nil {
		t.Fatal(err)
	}
	catalogue := restored.WorkspaceSkills()
	if len(catalogue.Entries) != 1 || catalogue.Entries[0].Body != "BODY" {
		t.Fatal(catalogue)
	}
	if err := restored.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := Restore(ctx, "skills", second)
	if err != nil || again.WorkspaceSkills().Revision != 1 {
		t.Fatalf("clear lost catalogue: %v", err)
	}
}
func TestWorkspaceCatalogueRejectsCorruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, corrupt := range []string{"hash", "body", "schema", "revision"} {
		t.Run(corrupt, func(t *testing.T) {
			t.Parallel()
			entry := workspaceEntry()
			catalogue := WorkspaceCatalogue{Schema: 1, Revision: 1, Entries: []WorkspaceSkill{entry}}
			switch corrupt {
			case "hash":
				catalogue.Entries[0].Hash = "wrong"
			case "body":
				catalogue.Entries[0].Body = "altered"
			case "schema":
				catalogue.Schema = 2
			case "revision":
				catalogue.Revision = 4
			}
			raw, _ := json.Marshal(catalogue)
			j := NewMemoryJournal()
			if _, err := j.Append(ctx, Record{RunID: "bad", Kind: RecordWorkspaceSkills, Payload: raw}); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(ctx, "bad", j); err == nil {
				t.Fatal("accepted corrupt catalogue")
			}
		})
	}
}

type catalogueFailure struct{ *MemoryJournal }

func (j catalogueFailure) Append(ctx context.Context, r Record) (int, error) {
	if r.Kind == RecordWorkspaceSkills {
		return 0, errors.New("injected append failure")
	}
	return j.MemoryJournal.Append(ctx, r)
}
func TestWorkspaceCatalogueAppendBeforePublication(t *testing.T) {
	t.Parallel()
	s := NewSession("fail", catalogueFailure{NewMemoryJournal()})
	if _, err := s.SelectWorkspaceSkills(context.Background(), []WorkspaceSkill{workspaceEntry()}); err == nil {
		t.Fatal("missing failure")
	}
	if s.WorkspaceSkills().Revision != 0 {
		t.Fatal("published unrecorded catalogue")
	}
}
