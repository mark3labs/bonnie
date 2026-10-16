package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mark3labs/bonnie/internal/workspaceskills"
)

func savedSkill(t *testing.T) WorkspaceSkillVersion {
	t.Helper()
	s, err := workspaceskills.Parse("repo/.agents/skills/review/SKILL.md", "repo", []byte("---\nname: review\ndescription: Review repository.\n---\nSaved body.\n"))
	if err != nil {
		t.Fatal(err)
	}
	return *s
}
func TestWorkspaceCatalogueAppendAndRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j, err := OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	s := NewSession("catalogue", j)
	skill := savedSkill(t)
	for range 2 {
		if err := s.SelectWorkspaceSkills(ctx, []WorkspaceSkillVersion{skill}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := j.Replay(ctx, "catalogue")
	if err != nil || len(recs) != 1 || recs[0].EntryID != "" {
		t.Fatalf("records=%+v err=%v", recs, err)
	}
	restored, err := Restore(ctx, "catalogue", j)
	if err != nil {
		t.Fatal(err)
	}
	versions, ok := restored.WorkspaceSkillCatalogue()
	if !ok || len(versions) != 1 || versions[0].Body != skill.Body {
		t.Fatal("version not restored")
	}
	versions[0].Original[0] = 'x'
	again, _ := restored.WorkspaceSkillCatalogue()
	if again[0].Original[0] == 'x' {
		t.Fatal("returned mutable durable content")
	}
	if err := restored.SelectWorkspaceSkills(ctx, nil); err != nil {
		t.Fatal(err)
	}
	restored, err = Restore(ctx, "catalogue", j)
	if err != nil {
		t.Fatal(err)
	}
	versions, ok = restored.WorkspaceSkillCatalogue()
	if !ok || len(versions) != 0 {
		t.Fatal("empty snapshot lost")
	}
}

type catalogueFailure struct {
	*MemoryJournal
	fail bool
}

func (j *catalogueFailure) Append(ctx context.Context, r Record) (int, error) {
	if j.fail && r.Kind == RecordWorkspaceSkills {
		return 0, errors.New("injected catalogue write failure")
	}
	return j.MemoryJournal.Append(ctx, r)
}
func TestWorkspaceCatalogueAppendFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := &catalogueFailure{MemoryJournal: NewMemoryJournal()}
	s := NewSession("failure", j)
	if err := s.SelectWorkspaceSkills(ctx, []WorkspaceSkillVersion{savedSkill(t)}); err != nil {
		t.Fatal(err)
	}
	j.fail = true
	if err := s.SelectWorkspaceSkills(ctx, nil); err == nil {
		t.Fatal("write failure ignored")
	}
	versions, _ := s.WorkspaceSkillCatalogue()
	if len(versions) != 1 {
		t.Fatal("failed selection published")
	}
	restored, err := Restore(ctx, "failure", j)
	if err != nil {
		t.Fatal(err)
	}
	versions, _ = restored.WorkspaceSkillCatalogue()
	if len(versions) != 1 {
		t.Fatal("failed selection restored")
	}
}
func TestWorkspaceCatalogueRejectsCorruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mutate := range []func(*workspaceCatalogue){func(c *workspaceCatalogue) { c.Schema = 2 }, func(c *workspaceCatalogue) { c.Revision = 2 }, func(c *workspaceCatalogue) { c.Skills[0].Body = "injected" }, func(c *workspaceCatalogue) { c.Skills[0].Hash = "invalid" }, func(c *workspaceCatalogue) { c.Skills[0].Source = "../escape" }} {
		c := workspaceCatalogue{Schema: 1, Revision: 1, Skills: []WorkspaceSkillVersion{savedSkill(t)}}
		mutate(&c)
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		j := NewMemoryJournal()
		if _, err := j.Append(ctx, Record{RunID: "corrupt", Kind: RecordWorkspaceSkills, Payload: data}); err != nil {
			t.Fatal(err)
		}
		if _, err := Restore(ctx, "corrupt", j); err == nil {
			t.Fatal("corrupt selection accepted")
		}
	}
}
