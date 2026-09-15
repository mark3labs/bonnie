package bonnie

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// generatedSkills is the shape codegen produces. The generated file binds the
// tree's skills directory with `//go:embed skills`, so every path begins with
// that one element, and a skill is either a file in it or a subdirectory with
// a SKILL.md. An fs.FS fixture states that shape in the test rather than
// leaving it implied by an embed directive somewhere else.
var generatedSkills = fstest.MapFS{
	"skills/refunds/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: refunds\ndescription: How a refund is decided.\n---\n\nBody.\n")},
	"skills/tone.md":          &fstest.MapFile{Data: []byte("---\nname: tone\ndescription: How the agent speaks.\n---\n\nBody.\n")},
}

// The tree's skills directory is a slot like instructions.md and workspace/:
// the default layout names it, and an option replaces it.
func TestSkillsAreASlotInTheTree(t *testing.T) {
	t.Parallel()

	t.Run("the default is the tree's skills directory", func(t *testing.T) {
		t.Parallel()
		if got := resolve().skillsPath; got != DefaultSkills {
			t.Fatalf("default skills path = %q, want %q", got, DefaultSkills)
		}
	})

	t.Run("a host with no tree loads none", func(t *testing.T) {
		t.Parallel()
		dir, err := resolve(WithSkills("")).skillsDir()
		if err != nil {
			t.Fatalf("skillsDir: %v", err)
		}
		if dir != "" {
			t.Fatalf("skills = %q, want empty without a tree", dir)
		}
	})

	t.Run("a directory on disk is used as it stands", func(t *testing.T) {
		t.Parallel()
		tree := t.TempDir()
		writeSkill(t, filepath.Join(tree, "tone.md"))

		dir, err := resolve(WithSkills(tree)).skillsDir()
		if err != nil {
			t.Fatalf("skillsDir: %v", err)
		}
		if dir != tree {
			t.Fatalf("skills = %q, want the directory on disk %q", dir, tree)
		}
		if !filepath.IsAbs(dir) {
			t.Fatalf("skills %q is not absolute: Kit scans the path as given", dir)
		}
	})
}

// The skill set reaches Kit as Options.SkillsDir, which is also what turns
// Kit's auto-discovery off. Without it Kit scans .agents/skills and
// .kit/skills under Options.SessionDir — and sandbox.Agent points SessionDir
// at the sandbox root, so a confined agent would inherit instructions from a
// directory it merely sits beside.
func TestSkillsReachKitAsTheWholeSkillSet(t *testing.T) {
	t.Parallel()

	got := applyKitOptions(resolve().kitOptions("", "/tmp/tree/skills"))
	if got.SkillsDir != "/tmp/tree/skills" {
		t.Fatalf("SkillsDir = %q, want the resolved skills directory", got.SkillsDir)
	}
	if got.NoSkills {
		t.Fatal("skills are switched off: the tree's skills would be embedded and ignored")
	}
	if len(got.Skills) > 0 {
		t.Fatalf("Skills = %v, want the directory form: Kit consults the explicit list "+
			"BEFORE SkillsDir, so a leftover list would shadow the tree's own directory", got.Skills)
	}

	// The tree's directory is the skill set, so it also wins over a list a
	// host passed through WithKit — the rule the system prompt follows.
	over := applyKitOptions(resolve(WithKit(func(o *kit.Options) {
		o.Skills = []string{"/opt/host/one.md"}
		o.NoSkills = true
	})).kitOptions("", "/tmp/tree/skills"))
	if over.SkillsDir != "/tmp/tree/skills" || len(over.Skills) > 0 || over.NoSkills {
		t.Fatalf("the tree's skills did not win over WithKit: SkillsDir = %q, Skills = %v, NoSkills = %v",
			over.SkillsDir, over.Skills, over.NoSkills)
	}

	// A tree with no skills says so, instead of leaving Kit to auto-discover
	// ~/.agents/skills — the operator's own editor skills — and the
	// .agents/skills under the sandbox root the run happens to use.
	bare := applyKitOptions(resolve().kitOptions("", ""))
	if bare.SkillsDir != "" {
		t.Fatalf("SkillsDir = %q for a tree with no skills, want empty", bare.SkillsDir)
	}
	if !bare.NoSkills {
		t.Fatal("a tree with no skills leaves discovery on: the agent would inherit " +
			"the operator's own skills, and a sandboxed run would read a directory it merely sits beside")
	}

	// What the host asked for through WithKit is not discovery, and an empty
	// slot in the tree must not withdraw it.
	host := applyKitOptions(resolve(WithKit(func(o *kit.Options) { o.SkillsDir = "/opt/host/skills" })).kitOptions("", ""))
	if host.NoSkills || host.SkillsDir != "/opt/host/skills" {
		t.Fatalf("the host's WithKit skills were dropped: SkillsDir = %q, NoSkills = %v",
			host.SkillsDir, host.NoSkills)
	}
}

// A built binary has no tree beside it, so the skills codegen embedded are
// what it has. Kit takes a path, so the embedded copy has to reach disk
// first — beside the journal, never in the tree.
func TestSkillsFallBackToTheEmbeddedCopy(t *testing.T) {
	// Register is process-wide, so this test is not parallel and puts the
	// registry back when it is done.
	before := Registered()
	t.Cleanup(func() { Register(before) })

	t.Run("disk wins over the embed", func(t *testing.T) {
		tree := t.TempDir()
		writeSkill(t, filepath.Join(tree, "tone.md"))
		journal := t.TempDir()

		dir, err := resolve(WithSkills(tree), WithJournal(journal)).skillsDir()
		if err != nil {
			t.Fatalf("skillsDir: %v", err)
		}
		if dir != tree {
			t.Fatalf("skills = %q, want the tree on disk %q", dir, tree)
		}
	})

	t.Run("an absent directory falls back", func(t *testing.T) {
		journal := t.TempDir()
		dest := filepath.Join(journal, DefaultSkills)
		if _, err := unpackSkills(generatedSkills, dest); err != nil {
			t.Fatalf("unpackSkills: %v", err)
		}

		// The embed's own root element is stripped, so a skill bundled as
		// skills/<name>/SKILL.md keeps its directory — which is the only
		// layout Kit recognises for a multi-file skill.
		for _, want := range []string{
			filepath.Join(dest, "refunds", "SKILL.md"),
			filepath.Join(dest, "tone.md"),
		} {
			if _, err := os.Stat(want); err != nil {
				t.Fatalf("the embedded skills did not materialise at %s: %v", want, err)
			}
		}

		// The layout is not merely plausible: Kit is the reader, so Kit's own
		// loader is what says whether the unpack produced a skill set. This is
		// the seam where a change to either side would otherwise pass every
		// test and reach the model as a prompt with no skills in it.
		loaded, err := kit.LoadSkillsFromDir(dest)
		if err != nil {
			t.Fatalf("kit.LoadSkillsFromDir: %v", err)
		}
		names := make([]string, 0, len(loaded))
		for _, s := range loaded {
			names = append(names, s.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"refunds", "tone"}) {
			t.Fatalf("Kit loaded %v from the unpacked directory, want both skills", names)
		}
	})

	t.Run("a directory holding only .gitkeep is not a skill set", func(t *testing.T) {
		tree := t.TempDir()
		if err := os.WriteFile(filepath.Join(tree, ".gitkeep"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if hasSkillFiles(tree) {
			t.Fatal("the scaffold's empty slot counted as a skill set: " +
				"it would win over the copy a built binary carries")
		}
	})

	t.Run("a tree with no skills names no directory", func(t *testing.T) {
		Register(Tree{})
		dir, err := resolve(WithSkills(filepath.Join(t.TempDir(), "absent")), WithJournal(t.TempDir())).skillsDir()
		if err != nil {
			t.Fatalf("skillsDir: %v", err)
		}
		if dir != "" {
			t.Fatalf("skills = %q, want empty: neither the tree nor the binary has any", dir)
		}
	})
}

// Unlike the workspace seed, unpacking replaces what is there. A skill is
// authored data the model never writes, so a copy an older binary left is
// stale — and keeping it would leave a deleted skill in the system prompt for
// as long as the journal directory survives.
func TestUnpackedSkillsAreReplacedNotKept(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), DefaultSkills)
	stale := filepath.Join(dest, "withdrawn.md")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, stale)

	if _, err := unpackSkills(generatedSkills, dest); err != nil {
		t.Fatalf("unpackSkills: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a skill the tree no longer carries survived the unpack: " +
			"the model would still be offered it")
	}
	if _, err := os.Stat(filepath.Join(dest, "tone.md")); err != nil {
		t.Fatalf("the current skills did not replace the stale copy: %v", err)
	}
}

// applyKitOptions resolves an option slice the way Kit does, so a test can
// read the configuration the agent would really be built with.
func applyKitOptions(opts []kit.Option) *kit.Options {
	o := &kit.Options{}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

// writeSkill writes a minimal valid skill file: YAML frontmatter with a name,
// then a body.
func writeSkill(t *testing.T, path string) {
	t.Helper()
	name := filepath.Base(path)
	body := "---\nname: " + name + "\ndescription: A test skill.\n---\n\nBody.\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
