package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The nix-release-pin job runs scripts/bump-flake-release-pin.sh after a tag
// is pushed, and pushes the result to master. A mistake there breaks
// `nix profile add github:mark3labs/bonnie` for everyone, and is seen only
// after a release. Each test runs the script on a copy of the real flake.nix,
// so a marker that moves or breaks in flake.nix fails here first.

var pinLines = regexp.MustCompile(`(?m)^.*# nix:bonnie-release-(tag|version)$`)

// bumpPin copies the script and flake.nix into a temporary tree, applies
// edit to the flake, and runs the script with tag. It returns the flake
// before and after, the combined output, and the exit code.
func bumpPin(t *testing.T, tag string, edit func(string) string) (before, after, out string, code int) {
	t.Helper()

	script, err := os.ReadFile("bump-flake-release-pin.sh")
	if err != nil {
		t.Fatal(err)
	}
	flake, err := os.ReadFile("../flake.nix")
	if err != nil {
		t.Fatal(err)
	}
	before = string(flake)
	if edit != nil {
		before = edit(before)
	}

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "scripts", "bump-flake-release-pin.sh")
	if err := os.WriteFile(path, script, 0o644); err != nil {
		t.Fatal(err)
	}
	flakePath := filepath.Join(root, "flake.nix")
	if err := os.WriteFile(flakePath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run through bash, not the file itself: a parallel test that forks while
	// this file is open for writing makes exec fail with ETXTBSY.
	output, err := exec.Command("bash", path, tag).CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run script: %v", err)
	}
	got, err := os.ReadFile(flakePath)
	if err != nil {
		t.Fatal(err)
	}
	return before, string(got), string(output), code
}

// currentTag is the tag the real flake.nix pins.
func currentTag(t *testing.T) string {
	t.Helper()
	flake, err := os.ReadFile("../flake.nix")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`"github:mark3labs/bonnie/(v[0-9.]+)"; # nix:bonnie-release-tag`).FindSubmatch(flake)
	if m == nil {
		t.Fatal("flake.nix has no bonnie-release-tag pin")
	}
	return string(m[1])
}

func TestBumpFlakeReleasePinRewritesBothPins(t *testing.T) {
	t.Parallel()

	before, after, out, code := bumpPin(t, "v9.8.7", nil)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	pins := pinLines.FindAllString(after, -1)
	if len(pins) != 2 ||
		!strings.Contains(pins[0], `"github:mark3labs/bonnie/v9.8.7";`) ||
		!strings.Contains(pins[1], `"9.8.7";`) {
		t.Fatalf("pins after bump: %q", pins)
	}
	// Only the two pin lines may change.
	if pinLines.ReplaceAllString(before, "") != pinLines.ReplaceAllString(after, "") {
		t.Fatal("the bump changed lines other than the two pins")
	}
}

func TestBumpFlakeReleasePinIsIdempotent(t *testing.T) {
	t.Parallel()

	before, after, out, code := bumpPin(t, currentTag(t), nil)
	if code != 0 || before != after || !strings.Contains(out, "already installs") {
		t.Fatalf("exit %d, changed %v: %s", code, before != after, out)
	}
}

func TestBumpFlakeReleasePinRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		tag  string
		edit func(string) string
	}{
		{name: "tag without v", tag: "9.8.7"},
		{name: "no tag", tag: ""},
		{name: "pins that disagree", tag: "v9.8.7", edit: func(s string) string {
			return regexp.MustCompile(`"[0-9.]+"; # nix:bonnie-release-version`).
				ReplaceAllString(s, `"0.0.1"; # nix:bonnie-release-version`)
		}},
		{name: "a second tag marker", tag: "v9.8.7", edit: func(s string) string {
			return s + "\n# url = \"github:mark3labs/bonnie/v0.0.1\"; # nix:bonnie-release-tag\n"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before, after, out, code := bumpPin(t, tc.tag, tc.edit)
			if code == 0 {
				t.Fatalf("exit 0, want a refusal: %s", out)
			}
			if before != after {
				t.Fatal("a refused bump changed flake.nix")
			}
		})
	}
}
