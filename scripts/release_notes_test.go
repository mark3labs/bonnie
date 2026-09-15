// Package scripts holds no Go code. This test guards the release tooling
// that lives beside it: scripts/release-notes.sh slices a release's section
// out of CHANGELOG.md so the published body carries the claims and the
// limits instead of a commit list (T-027).
//
// The script runs in the release workflow, where a mistake is only visible
// after a tag is pushed and a tag is never re-cut. So the behaviour is
// pinned here, where `go test ./...` reaches it.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run executes release-notes.sh against a changelog and returns stdout,
// stderr, and the exit code.
func run(t *testing.T, changelog, version string) (string, string, int) {
	t.Helper()

	script, err := filepath.Abs("release-notes.sh")
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	cmd := exec.Command(script, version, changelog)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); !ok {
			t.Fatalf("run script: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// writeChangelog puts a fixture changelog in a temp dir and returns its path.
func writeChangelog(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write changelog: %v", err)
	}
	return path
}

const fixture = `# Changelog

## [0.5.0] — 2026-09-15

Intro line.

### The claims

- A run survives process death.

## [0.4.0] — 2026-09-14

Older release.

## [0.10.0] — 2026-01-01

Double-digit minor.
`

func TestExtractsTheRequestedSection(t *testing.T) {
	t.Parallel()

	path := writeChangelog(t, fixture)
	out, _, code := run(t, path, "v0.5.0")

	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "Intro line.") {
		t.Errorf("body missing the intro:\n%s", out)
	}
	if !strings.Contains(out, "survives process death") {
		t.Errorf("body missing the claim:\n%s", out)
	}
}

// The section must stop at the next release heading. A body that runs on
// would republish every previous release's notes.
func TestStopsAtTheNextRelease(t *testing.T) {
	t.Parallel()

	path := writeChangelog(t, fixture)
	out, _, _ := run(t, path, "v0.5.0")

	if strings.Contains(out, "Older release.") {
		t.Errorf("body leaked the next section:\n%s", out)
	}
	if strings.Contains(out, "## [0.4.0]") {
		t.Errorf("body leaked the next heading:\n%s", out)
	}
}

// The leading `v` on a tag is optional; the headings never carry one.
func TestVersionAcceptsALeadingV(t *testing.T) {
	t.Parallel()

	path := writeChangelog(t, fixture)
	withV, _, codeV := run(t, path, "v0.5.0")
	bare, _, codeBare := run(t, path, "0.5.0")

	if codeV != 0 || codeBare != 0 {
		t.Fatalf("exits = %d/%d, want 0/0", codeV, codeBare)
	}
	if withV != bare {
		t.Errorf("v0.5.0 and 0.5.0 gave different bodies")
	}
}

// A tag with no section must fail the release, naming the heading, rather
// than publishing an empty body that states neither claims nor limits.
func TestMissingSectionFailsAndNamesTheHeading(t *testing.T) {
	t.Parallel()

	path := writeChangelog(t, fixture)
	out, errOut, code := run(t, path, "v9.9.9")

	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for a missing section")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout should be empty on failure, got:\n%s", out)
	}
	if !strings.Contains(errOut, "## [9.9.9]") {
		t.Errorf("error does not name the missing heading:\n%s", errOut)
	}
}

// `0.5` must not match `0.5.0`, and `0.1.0` must not match `0.10.0`.
// A prefix match would publish the wrong release's notes.
func TestVersionMatchIsExactNotAPrefix(t *testing.T) {
	t.Parallel()

	path := writeChangelog(t, fixture)

	for _, version := range []string{"0.5", "0.1.0"} {
		if _, _, code := run(t, path, version); code == 0 {
			t.Errorf("version %q matched by prefix; want no match", version)
		}
	}

	// The exact double-digit minor still resolves.
	out, _, code := run(t, path, "v0.10.0")
	if code != 0 {
		t.Fatalf("exit = %d for v0.10.0, want 0", code)
	}
	if !strings.Contains(out, "Double-digit minor.") {
		t.Errorf("wrong section for v0.10.0:\n%s", out)
	}
}

// latestReleased returns the newest version heading in CHANGELOG.md that is
// an actual release, skipping [Unreleased]. At release time the section being
// cut is the newest one, so this is the version about to ship.
func latestReleased(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if !strings.HasPrefix(line, "## [") {
			continue
		}
		version := line[len("## ["):]
		if i := strings.Index(version, "]"); i >= 0 {
			version = version[:i]
		}
		if strings.EqualFold(version, "Unreleased") {
			continue
		}
		return version
	}
	t.Fatal("CHANGELOG.md has no released version heading")
	return ""
}

// The newest released section must state the claims and the limits. T-011
// requires it of every release, and the manual edit that used to enforce it
// is gone, so this is what is left to catch a release that forgets.
//
// It deliberately tracks the newest version rather than a fixed one: pinned
// to a single version it would pass forever while checking a section that
// shipped long ago, which is the whole failure it exists to prevent.
func TestRepositoryChangelogStatesClaimsAndLimits(t *testing.T) {
	t.Parallel()

	version := latestReleased(t, "../CHANGELOG.md")
	out, _, code := run(t, "../CHANGELOG.md", version)
	if code != 0 {
		t.Fatalf("exit = %d for version %s, want 0", code, version)
	}

	for _, want := range []string{
		"survives process death",
		"parks indefinitely",
		"reachable over HTTP",
		"Known limits",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the %s notes never say %q", version, want)
		}
	}
}
