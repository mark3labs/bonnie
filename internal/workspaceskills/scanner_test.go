package workspaceskills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/discoveryio"
)

type nativeReader struct{ *discoveryio.Root }

func (n nativeReader) ListDiscovery(ctx context.Context, p string, limit int) ([]discoveryio.Entry, bool, error) {
	return n.List(ctx, p, limit)
}
func (n nativeReader) ReadDiscovery(ctx context.Context, p string, limit int64) (discoveryio.File, error) {
	return n.Read(ctx, p, limit)
}
func fixture(t *testing.T, files map[string]string) (nativeReader, string) {
	t.Helper()
	dir := t.TempDir()
	for p, data := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := discoveryio.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return nativeReader{root}, dir
}

const validSkill = "---\nname: demo\ndescription: Demo skill\n---\n\nBODY_MARKER"

func TestScanLocationsAndScopes(t *testing.T) {
	t.Parallel()
	r, _ := fixture(t, map[string]string{
		".agents/skills/demo/SKILL.md":                validSkill,
		"repo/.git":                                   "gitdir: elsewhere",
		"repo/sub/.kit/skills/demo/SKILL.md":          validSkill,
		"outside/.agents/skills/demo/SKILL.md":        validSkill,
		"skills/demo/SKILL.md":                        validSkill,
		"arbitrary/SKILL.md":                          validSkill,
		"node_modules/x/.agents/skills/demo/SKILL.md": validSkill,
	})
	p, _ := Normalize(Policy{})
	result := Scan(context.Background(), r, p, nil)
	if !result.Complete || len(result.Entries) != 3 {
		t.Fatalf("result = %+v", result)
	}
	for _, e := range result.Entries {
		if strings.HasPrefix(e.Source, "repo/") && e.Scope != "repo" {
			t.Fatalf("scope=%s", e.Scope)
		}
		if !strings.HasSuffix(e.Name, e.Hash) && len(e.Name) < 64 {
			t.Fatal("missing path hash")
		}
	}
	if strings.Contains(Render(result.Entries), "BODY_MARKER") {
		t.Fatal("eager body")
	}
}
func TestScanGitIgnoredParentsAndRepositoryReset(t *testing.T) {
	t.Parallel()
	r, _ := fixture(t, map[string]string{
		".gitignore": "ignored/\n!ignored/child/\n*.tmp\n",
		"ignored/child/.agents/skills/demo/SKILL.md": validSkill,
		"repo/.git":       "gitdir: ignored pointer",
		"repo/.gitignore": "sub/\n!sub/child/\n",
		"repo/sub/child/.agents/skills/demo/SKILL.md": validSkill,
		"repo/ok/.agents/skills/demo/SKILL.md":        validSkill,
		"repo/nested/.git":                            "gitdir: ignored pointer",
		"repo/nested/.agents/skills/demo/SKILL.md":    validSkill,
	})
	p, _ := Normalize(Policy{})
	result := Scan(context.Background(), r, p, nil)
	if len(result.Entries) != 2 {
		t.Fatalf("result = %+v", result)
	}
}
func TestScanInvalidEditRemovesAndBudgetSkips(t *testing.T) {
	t.Parallel()
	r, dir := fixture(t, map[string]string{"a/.agents/skills/demo/SKILL.md": validSkill, "b/.agents/skills/demo/SKILL.md": validSkill})
	p, _ := Normalize(Policy{MaxSkills: 1})
	first := Scan(context.Background(), r, p, nil)
	if len(first.Entries) != 1 {
		t.Fatal(first)
	}
	if err := os.WriteFile(filepath.Join(dir, "a/.agents/skills/demo/SKILL.md"), []byte("---\nname: interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	second := Scan(context.Background(), r, p, nil)
	if len(second.Entries) != 1 || !strings.HasPrefix(second.Entries[0].Source, "b/") {
		t.Fatal(second)
	}
}
func TestPolicyValidation(t *testing.T) {
	t.Parallel()
	for _, g := range []string{"", "/abs", "a//b", "../x", "!x", "a/["} {
		if _, err := Normalize(Policy{Exclude: []string{g}}); err == nil {
			t.Errorf("accepted %q", g)
		}
	}
	for _, g := range []string{"a/**", "**/archives", "a/[xy]"} {
		if _, err := Normalize(Policy{Exclude: []string{g}}); err != nil {
			t.Error(err)
		}
	}
}

func TestGitRulesFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rule, path string
		ignored    bool
	}{
		{"# comment\n", "x", false}, {"\\#literal/\n", "#literal", true}, {"\\!literal/\n", "!literal", true}, {"space\\ /\n", "space ", true}, {"/anchored/\n", "nested/anchored", false}, {"**/drop/\n", "nested/drop", true}, {"drop/\n!drop/\n", "drop", false},
	} {
		t.Run(tc.path+tc.rule, func(t *testing.T) {
			t.Parallel()
			r, _ := fixture(t, map[string]string{".gitignore": tc.rule, tc.path + "/.agents/skills/demo/SKILL.md": validSkill})
			p, _ := Normalize(Policy{})
			got := Scan(context.Background(), r, p, nil)
			if (len(got.Entries) == 0) != tc.ignored {
				t.Fatalf("rule %q path %q: %+v", tc.rule, tc.path, got)
			}
		})
	}
}
