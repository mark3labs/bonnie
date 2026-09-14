package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/agent"
)

// buildOpts carries the parsed flags of `bonnie build`.
type buildOpts struct {
	dryRun bool
	output string

	// env overrides the environment the go build inherits. It is test-only
	// plumbing: a hermetic test points a temp go.work at the tree.
	env []string
}

// newBuildCmd mounts `bonnie build`.
func newBuildCmd() *cobra.Command {
	var o buildOpts
	cmd := &cobra.Command{
		Use:   "build [dir]",
		Short: "Compile an agent tree into one static binary",
		Long: `Compile the agent tree at dir into a single static binary: generate the
tool wiring, embed the instructions, skills, and workspace, and build the
module. The output binary serves the agent on a host with no Go toolchain and
no BONNIE install — the tree graduates into a binary it owns.

The build machine needs Go. The host needs nothing. Binary output is ./<module>
(or --output), where module is the base name from the tree's go.mod. The
mark3labs modules are public, so the tree's go.mod resolves them from the proxy
— no go.work or repository access needed.

--dry-run prints the discovery plan — the files found, the tools generated, the
embed set — without writing or building.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runBuild(dir, o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.dryRun, "dry-run", false, "print the discovery plan without writing or building")
	f.StringVar(&o.output, "output", "", "binary output path (default: ./<module>)")
	return cmd
}

// runBuild is the `bonnie build` implementation, separated from the cobra
// wiring so tests drive it directly.
func runBuild(root string, o buildOpts) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("bonnie: resolve build dir: %w", err)
	}

	// Codegen is the build-time half of discovery. Discovery itself refuses a
	// tree it cannot fully honor (false duplicates, a tool without Tool()),
	// so failing here is honest, not a failing build.
	content, plan, err := agent.Generate(root)
	if err != nil {
		return err
	}
	if o.dryRun {
		fmt.Print(plan.String())
		return nil
	}

	// The one file BONNIE owns. Every other file in the tree is authored.
	gen := filepath.Join(root, plan.OutFile)
	if err := os.WriteFile(gen, content, 0o644); err != nil {
		return fmt.Errorf("bonnie: write %s: %w", plan.OutFile, err)
	}

	output := o.output
	if output == "" {
		output = defaultBinaryName(plan.Module)
	}

	build := exec.Command("go", "build", "-o", output, ".")
	build.Dir = root
	build.Env = o.env
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("bonnie: go build: %w", err)
	}

	abs, _ := filepath.Abs(output)
	fmt.Fprintf(os.Stderr, "bonnie: built %s\n", abs)
	return nil
}

// defaultBinaryName is the output path when --output is empty: ./<module>,
// the base name of the module path in the tree's go.mod. The agent is named
// by the directory it lives in, the way every tool in the tree is named by
// its own — naming from paths, with no second place to say it.
func defaultBinaryName(module string) string {
	if module != "" {
		// The binary name must be a plain file name; a module element with an
		// awkward character is folded into a safe one.
		return "./" + agentName(filepath.Base(module))
	}
	return "./agent"
}

// agentName sanitises a name into a binary file name element.
func agentName(name string) string {
	var b []byte
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b = append(b, byte(r))
		default:
			b = append(b, '-')
		}
	}
	return string(b)
}
