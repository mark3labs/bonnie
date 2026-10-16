package workspaceskills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/skillfs"
)

type fixture struct{ *skillfs.Reader }

func (f fixture) SkillDirectory(ctx context.Context, p string, n int) ([]skillfs.Entry, bool, error) {
	return f.List(ctx, p, n)
}
func (f fixture) SkillFile(ctx context.Context, p string, n int64) (skillfs.File, error) {
	return f.Read(ctx, p, n)
}
func fixtureSource(t *testing.T) (string, fixture) {
	t.Helper()
	dir := t.TempDir()
	r, err := skillfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return dir, fixture{r}
}
func put(t *testing.T, root, p, data string) {
	t.Helper()
	target := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

const valid = "---\nname: review\ndescription: Review this repository.\n---\nPRIVATE_BODY\n"

func defaultPolicy(t *testing.T) Policy {
	t.Helper()
	p, err := Normalize(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func scan(t *testing.T, s Source, p Policy) Result {
	t.Helper()
	r, err := Scan(context.Background(), s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestScannerLocationsAndStableNames(t *testing.T) {
	t.Parallel()
	dir, s := fixtureSource(t)
	paths := []string{"a/.agents/skills/review/SKILL.md", "b/.kit/skills/review/SKILL.md", ".agents/skills/root/SKILL.md", "nested/no-repo/.kit/skills/x/SKILL.md"}
	for _, p := range paths {
		put(t, dir, p, valid)
	}
	put(t, dir, "a/.git/HEAD", "")
	put(t, dir, "b/.git", "gitdir: /not-read")
	for _, p := range []string{"a/skills/x/SKILL.md", "arbitrary/SKILL.md", "a/.agents/skills/loose.md", "a/.agents/skills/x/assets/SKILL.md"} {
		put(t, dir, p, valid)
	}
	first := scan(t, s, defaultPolicy(t))
	if !first.Complete || len(first.Skills) != 4 {
		t.Fatalf("result=%+v", first)
	}
	names := map[string]bool{}
	for _, v := range first.Skills {
		if names[v.Name] || !strings.HasPrefix(v.Name, "workspace/") {
			t.Fatalf("unstable identity %s", v.Name)
		}
		names[v.Name] = true
		if strings.Contains(Metadata([]Skill{v}), "PRIVATE_BODY") {
			t.Fatal("body leaked")
		}
	}
	again := scan(t, s, defaultPolicy(t))
	for i := range first.Skills {
		if first.Skills[i].Name != again.Skills[i].Name {
			t.Fatal("name changed")
		}
	}
	if first.Skills[1].Scope != "a" {
		t.Fatalf("scope=%q", first.Skills[1].Scope)
	}
}

func TestGitIgnoreTraversal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string]string
		want  int
	}{
		{"ignored parent cannot be included", map[string]string{".gitignore": "repo/\n!repo/.agents/skills/x/SKILL.md\n"}, 0},
		{"nested negation", map[string]string{".gitignore": "*.md\n", "repo/.gitignore": "!.agents/skills/x/SKILL.md\n"}, 1},
		{"independent repository resets", map[string]string{".gitignore": "*.md\n", "repo/.git": "gitdir: outside"}, 1},
		{"nested repository resets", map[string]string{".git": "gitdir: x", ".gitignore": "*.md\n", "repo/.git": "gitdir: y"}, 1},
		{"anchored directory", map[string]string{".gitignore": "/repo/\n"}, 0},
		{"globstar", map[string]string{".gitignore": "**/skills/**\n"}, 0},
		{"file negation", map[string]string{".gitignore": "*.md\n!SKILL.md\n"}, 1},
		{"comments", map[string]string{".gitignore": "# ignored comment\n\n"}, 1},
		{"directory only", map[string]string{".gitignore": "SKILL.md/\n"}, 1},
		{"no info exclude", map[string]string{"repo/.git/info/exclude": "*\n"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, s := fixtureSource(t)
			put(t, dir, "repo/.agents/skills/x/SKILL.md", valid)
			for p, data := range tt.files {
				put(t, dir, p, data)
			}
			r := scan(t, s, defaultPolicy(t))
			if !r.Complete || len(r.Skills) != tt.want {
				t.Fatalf("got %+v, want %d", r, tt.want)
			}
		})
	}
}

func TestMandatoryAndExplicitExclusions(t *testing.T) {
	t.Parallel()
	dir, s := fixtureSource(t)
	for name := range mandatory {
		put(t, dir, name+"/.agents/skills/x/SKILL.md", valid)
	}
	put(t, dir, "archives/old/.kit/skills/x/SKILL.md", valid)
	put(t, dir, "repo/.kit/skills/x/SKILL.md", valid)
	put(t, dir, ".gitignore", "!**/SKILL.md\n")
	p := defaultPolicy(t)
	p.Exclude = []string{"archives/**"}
	r := scan(t, s, p)
	if len(r.Skills) != 1 || r.Skills[0].Source != "repo/.kit/skills/x/SKILL.md" {
		t.Fatalf("result=%+v", r)
	}
}
func TestScannerChangesAndLimits(t *testing.T) {
	t.Parallel()
	dir, s := fixtureSource(t)
	source := "repo/.agents/skills/x/SKILL.md"
	put(t, dir, source, valid)
	p := defaultPolicy(t)
	first := scan(t, s, p)
	if len(first.Skills) != 1 {
		t.Fatalf("first=%+v", first)
	}
	put(t, dir, source, strings.Replace(valid, "PRIVATE_BODY", "UPDATED", 1))
	edited := scan(t, s, p)
	if len(edited.Skills) != 1 || edited.Skills[0].Name != first.Skills[0].Name || edited.Skills[0].Hash == first.Skills[0].Hash {
		t.Fatal("edit not selected")
	}
	put(t, dir, source, "---\nname: unfinished")
	if r := scan(t, s, p); len(r.Skills) != 0 || len(r.Diagnostics) == 0 {
		t.Fatal("invalid edit kept")
	}
	put(t, dir, source, valid)
	p.MaxSkillBytes = 5
	if len(scan(t, s, p).Skills) != 0 {
		t.Fatal("oversized file kept")
	}
	p = defaultPolicy(t)
	p.MaxMetadataBytes = int64(len(Metadata(first.Skills)) - 1)
	if len(scan(t, s, p).Skills) != 0 {
		t.Fatal("metadata overflow kept")
	}
	p = defaultPolicy(t)
	p.MaxDepth = 2
	if len(scan(t, s, p).Skills) != 0 {
		t.Fatal("depth overflow kept")
	}
	p = defaultPolicy(t)
	put(t, dir, "z/.kit/skills/x/SKILL.md", valid)
	p.MaxSkills = 1
	if r := scan(t, s, p); len(r.Skills) != 1 || r.Skills[0].Source != source {
		t.Fatalf("selection=%+v", r)
	}
	if err := os.Remove(filepath.Join(dir, source)); err != nil {
		t.Fatal(err)
	}
	p = defaultPolicy(t)
	if len(scan(t, s, p).Skills) != 1 {
		t.Fatal("delete not reconciled")
	}
}
func TestPolicyValidation(t *testing.T) {
	t.Parallel()
	for _, g := range []string{"", "/root", "!root", "a//b", "a/../b", "a/./b", "[", "ab**"} {
		if _, err := Normalize(Policy{Exclude: []string{g}}); err == nil {
			t.Errorf("accepted %q", g)
		}
	}
	for _, p := range []Policy{{MaxDepth: -1}, {MaxSkills: -1}, {MaxSkillBytes: -1}, {MaxMetadataBytes: -1}} {
		if _, err := Normalize(p); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	for _, g := range []string{"archives/**", "**/fixtures", "x/[ab]?*"} {
		if _, err := Normalize(Policy{Exclude: []string{g}}); err != nil {
			t.Error(err)
		}
	}
	for _, source := range []string{"repo/.agents/skills/x/SKILL.md", "weird ☃/.kit/skills/x/SKILL.md"} {
		a := Identity(source, "weird ☃", "review")
		b := Identity(source, "weird ☃", "review")
		if a != b || len(strings.Split(a, "--")[1]) != 64 {
			t.Fatal("invalid identity")
		}
	}
}

type exhausted struct{ Source }

func (e exhausted) SkillDirectory(context.Context, string, int) ([]skillfs.Entry, bool, error) {
	return nil, true, nil
}
func TestIncompleteAndCancellation(t *testing.T) {
	t.Parallel()
	_, s := fixtureSource(t)
	r := scan(t, exhausted{s}, defaultPolicy(t))
	if r.Complete {
		t.Fatal("partial scan committed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, s, defaultPolicy(t), nil); err == nil {
		t.Fatal("cancellation lost")
	}
}

func TestIgnoreEscapesAndChanges(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"#repo", "!repo", "space repo", "repo "} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			t.Parallel()
			dir, s := fixtureSource(t)
			put(t, dir, name+"/.agents/skills/x/SKILL.md", valid)
			pattern := name
			if name[0] == '#' || name[0] == '!' {
				pattern = "\\" + name
			}
			if before, ok := strings.CutSuffix(name, " "); ok {
				pattern = before + "\\ "
			}
			put(t, dir, ".gitignore", pattern+"/\n")
			if r := scan(t, s, defaultPolicy(t)); len(r.Skills) != 0 {
				t.Fatalf("ignored escaped path discovered: %+v", r)
			}
			put(t, dir, ".gitignore", "")
			if len(scan(t, s, defaultPolicy(t)).Skills) != 1 {
				t.Fatal("ignore change not observed")
			}
		})
	}
}
