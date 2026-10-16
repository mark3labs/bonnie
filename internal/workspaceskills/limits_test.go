package workspaceskills

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/discoveryio"
)

type incompleteReader struct{ Reader }

func (r incompleteReader) ListDiscovery(context.Context, string, int) ([]discoveryio.Entry, bool, error) {
	return nil, true, nil
}

type failingReader struct{}

func (failingReader) ListDiscovery(context.Context, string, int) ([]discoveryio.Entry, bool, error) {
	return nil, false, errors.New("root unavailable")
}
func (failingReader) ReadDiscovery(context.Context, string, int64) (discoveryio.File, error) {
	return discoveryio.File{}, errors.New("unavailable")
}
func TestIncompleteAndCancelledScans(t *testing.T) {
	t.Parallel()
	p, _ := Normalize(Policy{})
	for _, r := range []Reader{incompleteReader{}, failingReader{}} {
		if result := Scan(context.Background(), r, p, nil); result.Complete {
			t.Fatal("selected incomplete root")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := Scan(ctx, failingReader{}, p, nil); result.Complete {
		t.Fatal("selected cancelled scan")
	}
}
func TestMetadataBudgetExactEscaping(t *testing.T) {
	t.Parallel()
	r, _ := fixture(t, map[string]string{"a/.agents/skills/demo/SKILL.md": strings.Replace(validSkill, "Demo skill", "<escaped> & metadata", 1), "b/.agents/skills/demo/SKILL.md": validSkill})
	p, _ := Normalize(Policy{})
	all := Scan(context.Background(), r, p, nil)
	if len(all.Entries) != 2 {
		t.Fatal(all)
	}
	p.MaxMetadataBytes = int64(len(Render(all.Entries[:1])) - 1)
	limited := Scan(context.Background(), r, p, nil)
	if len(limited.Entries) != 1 || !strings.HasPrefix(limited.Entries[0].Source, "b/") {
		t.Fatalf("over-budget first entry should not exclude later candidate: %+v", limited)
	}
	if int64(len(Render(limited.Entries))) > p.MaxMetadataBytes {
		t.Fatal("escaped metadata exceeded budget")
	}
}
func TestMissingNameAndAlwaysBody(t *testing.T) {
	t.Parallel()
	r, _ := fixture(t, map[string]string{"a/.agents/skills/demo/SKILL.md": "---\ndescription: Missing name\n---\nBody", "b/.agents/skills/demo/SKILL.md": "---\nname: demo\ndescription: On demand\nwhen: always\n---\nALWAYS_BODY"})
	p, _ := Normalize(Policy{})
	got := Scan(context.Background(), r, p, nil)
	if len(got.Entries) != 1 {
		t.Fatal(got)
	}
	if strings.Contains(Render(got.Entries), "ALWAYS_BODY") {
		t.Fatal("body in metadata")
	}
	if MetadataSkill(got.Entries[0]).When != "on-demand" {
		t.Fatal("always policy retained")
	}
}

type changingReader struct {
	Reader
	reads int
}

func (r *changingReader) ReadDiscovery(ctx context.Context, p string, n int64) (discoveryio.File, error) {
	f, err := r.Reader.ReadDiscovery(ctx, p, n)
	if strings.HasSuffix(p, "SKILL.md") {
		r.reads++
		if r.reads%2 == 0 {
			f.After.Inode++
		}
	}
	return f, err
}
func TestUnstableReadSkipped(t *testing.T) {
	t.Parallel()
	r, _ := fixture(t, map[string]string{".agents/skills/demo/SKILL.md": validSkill})
	p, _ := Normalize(Policy{})
	result := Scan(context.Background(), &changingReader{Reader: r}, p, nil)
	if !result.Complete || len(result.Entries) != 0 || len(result.Diagnostics) == 0 {
		t.Fatal(result)
	}
}
func TestExplicitAndMandatoryExclusionsCannotBeUndone(t *testing.T) {
	t.Parallel()
	r, _ := fixture(t, map[string]string{".gitignore": "!archives/\n!vendor/\n", "archives/.agents/skills/demo/SKILL.md": validSkill, "vendor/.agents/skills/demo/SKILL.md": validSkill, "ok/.agents/skills/demo/SKILL.md": validSkill})
	p, _ := Normalize(Policy{Exclude: []string{"archives"}})
	result := Scan(context.Background(), r, p, nil)
	if len(result.Entries) != 1 || !strings.HasPrefix(result.Entries[0].Source, "ok/") {
		t.Fatal(result)
	}
}
