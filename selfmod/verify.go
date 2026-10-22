package selfmod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mark3labs/bonnie/agent"
)

// errBuild reports that a staged change does not generate or compile. Its
// message is the tool output the subagent reads to correct the change.
var errBuild = errors.New("bonnie: selfmod: the changed tree does not build")

// maxBuildOutput bounds the compiler output handed back to the subagent.
const maxBuildOutput = 16 << 10

// verify proves that the tree with the staged changes generates and builds,
// before one byte of the live tree changes.
//
// The build runs in the live tree's directory with a `go build -overlay`
// that replaces each changed file by its staged copy. It does not build the
// staged copy itself: a tree's go.mod can name a relative replace directive,
// and a copy at another path would resolve it to nothing. The overlay also
// replaces bonnie_gen.go with the wiring generated from the staged tree, so
// a tool added or removed under tools/ is wired the same way the dev loop
// wires it after the change lands.
//
// The go command must be on PATH. A host without it returns errNoToolchain,
// and the caller decides whether an unverified change may continue.
func verify(ctx context.Context, env []string, live, staging, work string, changes []change) error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return errNoToolchain
	}
	gen, plan, err := agent.Generate(staging)
	if err != nil {
		return fmt.Errorf("%w: generate wiring: %v", errBuild, err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return fmt.Errorf("bonnie: selfmod: verify: %w", err)
	}
	genPath := filepath.Join(work, plan.OutFile)
	if err := os.WriteFile(genPath, gen, 0o644); err != nil {
		return fmt.Errorf("bonnie: selfmod: verify: %w", err)
	}
	replace := map[string]string{filepath.Join(live, plan.OutFile): genPath}
	for _, c := range changes {
		target := filepath.Join(live, filepath.FromSlash(c.Path))
		if c.Op == opDelete {
			replace[target] = ""
			continue
		}
		replace[target] = filepath.Join(staging, filepath.FromSlash(c.Path))
	}
	overlay, err := json.Marshal(struct {
		Replace map[string]string `json:"Replace"`
	}{replace})
	if err != nil {
		return fmt.Errorf("bonnie: selfmod: verify: %w", err)
	}
	overlayPath := filepath.Join(work, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o644); err != nil {
		return fmt.Errorf("bonnie: selfmod: verify: %w", err)
	}
	cmd := exec.CommandContext(ctx, goBin, "build", "-overlay", overlayPath, "-o", os.DevNull, ".")
	cmd.Dir = live
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		text := strings.TrimSpace(string(out))
		if len(text) > maxBuildOutput {
			text = text[:maxBuildOutput] + "\n[output truncated]"
		}
		return fmt.Errorf("%w:\n%s", errBuild, text)
	}
	return nil
}

// errNoToolchain reports that the host has no go command to verify with.
var errNoToolchain = errors.New("bonnie: selfmod: no go toolchain on PATH")
