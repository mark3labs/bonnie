// Package examples holds no Go code of its own. Each directory under
// examples/ is an agent TREE — its own Go module, made with `bonnie init`,
// run with `bonnie dev`, and shipped with `bonnie build` — exactly the shape
// a user gets. This test is what keeps it that way.
//
// Why it exists: the examples were once "library examples", packages inside
// BONNIE's own module run with `go run ./examples/...`. That kept them
// compiled by `go build ./...`, but it showed every reader a way to run
// BONNIE that no user ever uses, and each README then had to explain how to
// rebuild the example as a tree by hand. The examples are now trees, and a
// tree is a nested module that `go build ./...` skips. So this test takes on
// both jobs: it refuses a drift back to the library shape, and it compiles
// every tree against this checkout, so an example cannot go stale either.
//
// Do not weaken a check here to make an example easier to write. Change the
// example.
package examples_test

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/internal/treetest"
)

// trees returns every example directory. Each directory under examples/ is
// an example; there is no opt-out list, so a new directory is checked the
// moment it exists.
func trees(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read examples/: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatal("examples/ holds no example; the guard would pass over nothing")
	}
	return out
}

// TestNoGoCodeOutsideATree refuses a Go file directly in examples/ other
// than this test. A package here is compiled by BONNIE's own module, which
// is the library-example shape this directory left behind.
func TestNoGoCodeOutsideATree(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".go" && e.Name() != "examples_test.go" {
			t.Errorf("examples/%s: Go code outside an agent tree; put it in a tree made with `bonnie init`", e.Name())
		}
	}
}

// TestExamplesHaveTheScaffoldShape scaffolds a fresh tree with the same code
// `bonnie init` runs, and requires every file it writes to exist in every
// example. When the scaffold grows a file, the examples must grow it too.
func TestExamplesHaveTheScaffoldShape(t *testing.T) {
	t.Parallel()
	fresh := filepath.Join(t.TempDir(), "fresh")
	created, err := agent.Scaffold(fresh, agent.InitOptions{Version: "v0.0.1"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	// Two files beyond the scaffold. go.sum comes from `go mod tidy`, the
	// documented next step, and an example must run for a user straight
	// after a clone. README.md is authored: an example that does not say
	// how to run it with the CLI is not an example.
	want := append(slices.Clone(created), "go.sum", "README.md")

	for _, name := range trees(t) {
		for _, f := range want {
			if _, err := os.Stat(filepath.Join(name, f)); err != nil {
				t.Errorf("examples/%s: missing %s, which every example tree carries: %v", name, f, err)
			}
		}
	}
}

// TestExamplesGoModIsAUserGoMod requires the go.mod a user gets: the module
// is named after the directory, as `bonnie init` names it; bonnie is pinned
// to a published release; and nothing is redirected. A replace directive or
// a pseudo-version would make the example build here and nowhere else.
func TestExamplesGoModIsAUserGoMod(t *testing.T) {
	t.Parallel()
	// The pin must be one of the two newest releases. Two, not one: the
	// release commit adds the CHANGELOG heading before the tag exists, so
	// the examples can only move to the new tag in the commit after. A pin
	// that falls further behind was forgotten — see docs/RELEASE.md.
	releases := releasedVersions(t)
	if len(releases) > 2 {
		releases = releases[:2]
	}
	pin := regexp.MustCompile(`(?m)^\s*(?:require\s+)?github\.com/mark3labs/bonnie\s+(\S+)`)

	for _, name := range trees(t) {
		raw, err := os.ReadFile(filepath.Join(name, "go.mod"))
		if err != nil {
			t.Errorf("examples/%s: %v", name, err)
			continue
		}
		body := string(raw)
		if !strings.Contains(body, "module "+name+"\n") {
			t.Errorf("examples/%s: go.mod module is not %q; `bonnie init %s` names it after the directory", name, name, name)
		}
		if regexp.MustCompile(`(?m)^\s*replace\b|^\s*replace\s*\(`).MatchString(body) {
			t.Errorf("examples/%s: go.mod carries a replace directive; a user's tree resolves bonnie from the proxy", name)
		}
		m := pin.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("examples/%s: go.mod does not require github.com/mark3labs/bonnie", name)
			continue
		}
		v := strings.TrimPrefix(m[1], "v")
		if !slices.Contains(releases, v) {
			t.Errorf("examples/%s: pins bonnie %s; want one of the newest releases %v (run `task examples-pin TAG=v%s`)",
				name, m[1], releases, releases[0])
		}
	}
}

// TestExamplesMainUsesTheTree refuses the options that replace a tree's own
// files. They are what the library examples used because they had no tree;
// in a tree the prompt is instructions.md and the rest are default paths.
func TestExamplesMainUsesTheTree(t *testing.T) {
	t.Parallel()
	banned := []string{
		"bonnie.WithSystemPrompt(", // the prompt is instructions.md
		"bonnie.WithInstructions(", // ... at its default path
		"bonnie.WithSkills(",       // skills/ is the skill set
		"bonnie.WithWorkspace(",    // workspace/ is the agent's root
		"bonnie.Register(",         // bonnie_gen.go owns registration
	}
	for _, name := range trees(t) {
		src, err := os.ReadFile(filepath.Join(name, "main.go"))
		if err != nil {
			t.Errorf("examples/%s: %v", name, err)
			continue
		}
		for _, b := range banned {
			if bytes.Contains(src, []byte(b)) {
				t.Errorf("examples/%s/main.go calls %s — that bypasses the tree; use the file at its default path", name, strings.TrimSuffix(b, "("))
			}
		}
	}
}

// TestExamplesGeneratedWiringIsCurrent requires each committed bonnie_gen.go
// to be byte-identical to what `bonnie build` generates from the tree now.
// A stale file would make `bonnie dev` leave a diff on the first run.
func TestExamplesGeneratedWiringIsCurrent(t *testing.T) {
	t.Parallel()
	for _, name := range trees(t) {
		root, _ := filepath.Abs(name)
		want, _, err := agent.Generate(root)
		if err != nil {
			t.Errorf("examples/%s: generate: %v", name, err)
			continue
		}
		got, err := os.ReadFile(filepath.Join(name, "bonnie_gen.go"))
		if err != nil {
			t.Errorf("examples/%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("examples/%s/bonnie_gen.go is stale; regenerate with `task examples-gen`", name)
		}
	}
}

// TestExamplesDocumentTheCLI refuses a README that tells a reader to run an
// example with `go run`, and requires one that runs it with `bonnie dev`.
// It reads examples/README.md, every example README, and the repository
// README's pointer to them.
func TestExamplesDocumentTheCLI(t *testing.T) {
	t.Parallel()
	goRun := regexp.MustCompile(`go\s+run\s+\S*examples`)
	docs := []string{"README.md", "../README.md"}
	for _, name := range trees(t) {
		docs = append(docs, filepath.Join(name, "README.md"))
	}
	for _, doc := range docs {
		raw, err := os.ReadFile(doc)
		if err != nil {
			t.Errorf("%s: %v", doc, err)
			continue
		}
		if m := goRun.Find(raw); m != nil {
			t.Errorf("%s says %q; an example is a tree, run it with `bonnie dev`", doc, m)
		}
		if doc != "../README.md" && !bytes.Contains(raw, []byte("bonnie dev")) {
			t.Errorf("%s never says `bonnie dev`", doc)
		}
	}
}

// TestExamplesBuildAgainstThisCheckout compiles and vets every tree against
// the BONNIE under test. The tree's own go.mod pins a release, so it is not
// used here: the tree is copied, given a go.mod that points at this checkout,
// and built the way `bonnie build` builds it.
//
// This is what stops an example from going stale now that it is a nested
// module `go build ./...` cannot see. Only bonnie is redirected; kit and
// the rest resolve through BONNIE's own go.mod, from the module cache.
func TestExamplesBuildAgainstThisCheckout(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds a subprocess per example")
	}
	for _, name := range trees(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dst := filepath.Join(t.TempDir(), name)
			copyTree(t, name, dst)
			gomod := "module " + name + "\n\ngo 1.27.0\n"
			if err := os.WriteFile(filepath.Join(dst, "go.mod"), []byte(gomod), 0o644); err != nil {
				t.Fatal(err)
			}
			treetest.LinkToCheckout(t, dst)

			env := append(treetest.BuildEnv(), "GOWORK=off")
			for _, args := range [][]string{
				{"build", "-o", os.DevNull, "."},
				{"vet", "./..."},
			} {
				cmd := exec.Command("go", args...)
				cmd.Dir = dst
				cmd.Env = env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("examples/%s: go %s against this checkout: %v\n%s", name, strings.Join(args, " "), err, out)
				}
			}
		})
	}
}

// copyTree copies an example's authored files, without go.mod and go.sum
// (the test writes its own) and without the output a local `bonnie dev` or
// `bonnie build` may have left beside them: the journal and the binary.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			if rel == ".bonnie" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if rel == "go.mod" || rel == "go.sum" || rel == filepath.Base(src) || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy examples/%s: %v", src, err)
	}
}

// releasedVersions returns the released versions in CHANGELOG.md, newest
// first, read from its "## [x.y.z]" headings.
func releasedVersions(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindAllSubmatch(raw, -1) {
		out = append(out, string(m[1]))
	}
	if len(out) == 0 {
		t.Fatal("CHANGELOG.md names no released version")
	}
	return out
}
