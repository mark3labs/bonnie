package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/agent"
)

// buildOpts carries the parsed flags of `bonnie build`.
type buildOpts struct {
	dryRun bool
	output string

	// nativeLoader keeps the Go toolchain's own ELF interpreter. See
	// loaderEnv.
	nativeLoader bool

	// env overrides the environment the go build inherits. It is test-only
	// plumbing: a hermetic test points the child build at a local checkout.
	env []string
}

// newBuildCmd mounts `bonnie build`.
func newBuildCmd() *cobra.Command {
	var o buildOpts
	cmd := &cobra.Command{
		Use:   "build [dir]",
		Short: "Compile an agent tree into one static binary",
		Long: `Compile the agent tree at dir into a single binary: generate the
tool wiring, embed the instructions, skills, and context files, and build the
module with CGO disabled. The output binary serves the agent on a host with no
Go toolchain and no BONNIE install — the tree graduates into a binary it owns.

Most trees give a static binary. A tree that loads a C library at run time
(for example through purego) gives a dynamically linked binary. On Linux, such
a binary names the standard loader for its architecture, for example
/lib64/ld-linux-x86-64.so.2, so it runs on each glibc distribution, also when
the Go toolchain comes from Nix. --native-loader keeps the toolchain's own
loader: use it for a binary that runs only on the Nix host that built it.

The build machine needs Go. The host needs nothing. Binary output is <tree>/<module>
(or --output), where module is the base name from the tree's go.mod. The
mark3labs modules are public, so the tree's go.mod resolves them from the proxy
— no repository access needed.

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
	f.StringVar(&o.output, "output", "", "binary output path, relative to the tree (default: ./<module>)")
	f.BoolVar(&o.nativeLoader, "native-loader", false, "keep the Go toolchain's ELF loader path instead of the standard Linux one")
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

	if !filepath.IsAbs(output) {
		output = filepath.Join(root, output)
	}

	env := staticBuildEnv(o.env)
	if !o.nativeLoader {
		if env, err = loaderEnv(root, env); err != nil {
			return err
		}
	}

	build := exec.Command("go", "build", "-o", output, ".")
	build.Dir = root
	build.Env = env
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("bonnie: go build: %w", err)
	}

	fmt.Fprintf(os.Stderr, "bonnie: built %s\n", output)
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

// staticBuildEnv disables CGO even when the caller enables it.
func staticBuildEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, "CGO_ENABLED=") {
			out = append(out, item)
		}
	}
	return append(out, "CGO_ENABLED=0")
}

// linuxLoaders is the ELF interpreter of a glibc Linux system for each GOARCH,
// and the musl one, as upstream cmd/link names them (Linuxdynld and
// LinuxdynldMusl in cmd/link/internal/<arch>/obj.go).
var linuxLoaders = map[string]struct{ glibc, musl string }{
	"386":      {"/lib/ld-linux.so.2", "/lib/ld-musl-i386.so.1"},
	"amd64":    {"/lib64/ld-linux-x86-64.so.2", "/lib/ld-musl-x86_64.so.1"},
	"arm":      {"/lib/ld-linux.so.3", "/lib/ld-musl-arm.so.1"},
	"arm64":    {"/lib/ld-linux-aarch64.so.1", "/lib/ld-musl-aarch64.so.1"},
	"loong64":  {"/lib64/ld-linux-loongarch-lp64d.so.1", "/lib/ld-musl-loongarch64.so.1"},
	"ppc64":    {"/lib64/ld64.so.2", "/lib/ld-musl-powerpc64.so.1"},
	"ppc64le":  {"/lib64/ld64.so.2", "/lib/ld-musl-powerpc64le.so.1"},
	"riscv64":  {"/lib/ld-linux-riscv64-lp64d.so.1", "/lib/ld-musl-riscv64.so.1"},
	"s390x":    {"/lib/ld64.so.1", "/lib/ld-musl-s390x.so.1"},
	"mips":     {"/lib/ld.so.1", ""},
	"mipsle":   {"/lib/ld.so.1", ""},
	"mips64":   {"/lib64/ld64.so.1", ""},
	"mips64le": {"/lib64/ld64.so.1", ""},
}

// loaderEnv sets GO_LDSO to the standard ELF interpreter of the target, so a
// dynamically linked agent runs on a host other than the build host.
//
// A Go toolchain can be built with its own default interpreter. The Go
// toolchain from nixpkgs names a loader in /nix/store, and a binary that
// carries that path stops on every other host with "cannot execute: required
// file not found". A tree is dynamically linked when it loads a C library at
// run time through purego, also with CGO disabled
// (examples/self-contained-agent does this), so CGO_ENABLED=0 alone does not
// prevent it. A static binary has no interpreter, and GO_LDSO has no effect.
//
// The choice copies upstream cmd/link: the glibc loader, or the musl loader
// when the build host has only the musl one. A GO_LDSO that the caller sets
// is kept. A target that is not Linux, or a GOARCH not in linuxLoaders, keeps
// the toolchain default.
func loaderEnv(root string, env []string) ([]string, error) {
	for _, item := range env {
		if strings.HasPrefix(item, "GO_LDSO=") {
			return env, nil
		}
	}
	cmd := exec.Command("go", "env", "GOOS", "GOARCH")
	cmd.Dir = root
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bonnie: go env: %w", err)
	}
	target := strings.Fields(string(out))
	if len(target) != 2 {
		return nil, fmt.Errorf("bonnie: go env: unexpected output %q", out)
	}
	ldso := linuxLoader(target[0], target[1], fileExists)
	if ldso == "" {
		return env, nil
	}
	return append(env, "GO_LDSO="+ldso), nil
}

// linuxLoader returns the standard ELF interpreter for goos and goarch, or ""
// when there is none to set. exists reports whether a path is on the build
// host.
func linuxLoader(goos, goarch string, exists func(string) bool) string {
	if goos != "linux" {
		return ""
	}
	l, ok := linuxLoaders[goarch]
	if !ok {
		return ""
	}
	if l.musl != "" && !exists(l.glibc) && exists(l.musl) {
		return l.musl
	}
	return l.glibc
}

// fileExists reports whether path names a file on this host.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
