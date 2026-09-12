package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MicrosandboxProvider runs each run in a microVM, driven through the msb CLI.
//
// This is the strongest isolation BONNIE offers. A microVM has its own guest
// kernel, so a container escape is not enough to reach the host, and msb can
// enforce a domain-level network allow-list that Docker cannot.
//
// It is driven through the CLI on purpose. The microsandbox Go SDK is a CGO
// wrapper around an embedded Rust library: linking it would force
// CGO_ENABLED=1, need a cross C toolchain per release target, and drop
// darwin/amd64 support. BONNIE would stop being a single static binary, which
// is the property it is built around. The CLI costs one dependency the user
// installs once, and keeps the release trivial. eve reaches microsandbox the
// same way.
//
// The user installs msb themselves; see
// https://github.com/superradcompany/microsandbox. [MicrosandboxProvider.Available]
// reports a clear error when it is absent.
type MicrosandboxProvider struct {
	bin    string
	image  string
	policy NetworkPolicy
	memory int
	cpus   int

	mu     sync.Mutex
	opened map[string]*cliSandbox
}

var (
	_ Provider  = (*MicrosandboxProvider)(nil)
	_ Networked = (*MicrosandboxProvider)(nil)
)

// MicrosandboxOption configures a [MicrosandboxProvider].
type MicrosandboxOption func(*MicrosandboxProvider)

// WithMicrosandboxImage sets the guest image. The default is "alpine:3.19".
func WithMicrosandboxImage(ref string) MicrosandboxOption {
	return func(p *MicrosandboxProvider) { p.image = ref }
}

// WithMicrosandboxBinary sets the CLI to drive. The default is "msb".
func WithMicrosandboxBinary(bin string) MicrosandboxOption {
	return func(p *MicrosandboxProvider) { p.bin = bin }
}

// WithMicrosandboxMemory sets guest memory in MiB.
func WithMicrosandboxMemory(mib int) MicrosandboxOption {
	return func(p *MicrosandboxProvider) { p.memory = mib }
}

// WithMicrosandboxCPUs sets the guest CPU count.
func WithMicrosandboxCPUs(n int) MicrosandboxOption {
	return func(p *MicrosandboxProvider) { p.cpus = n }
}

// Microsandbox returns a provider backed by microVMs.
func Microsandbox(opts ...MicrosandboxOption) *MicrosandboxProvider {
	p := &MicrosandboxProvider{
		bin:    "msb",
		image:  "alpine:3.19",
		policy: NetworkPolicy{Mode: NetworkAllowAll},
		opened: make(map[string]*cliSandbox),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name implements [Provider].
func (p *MicrosandboxProvider) Name() string { return "microsandbox" }

// Available implements [Provider].
func (p *MicrosandboxProvider) Available(ctx context.Context) error {
	if err := lookPath(p.bin); err != nil {
		return fmt.Errorf("%w (install it from https://github.com/superradcompany/microsandbox)", err)
	}
	_, stderr, code, err := runCLI(ctx, nil, p.bin, "--version")
	if err != nil {
		return fmt.Errorf("%w: %s --version: %v", ErrUnavailable, p.bin, err)
	}
	if code != 0 {
		return fmt.Errorf("%w: %s is not working: %s", ErrUnavailable, p.bin, firstLine(stderr))
	}
	return nil
}

// SetNetworkPolicy implements [Networked]. microsandbox supports every mode,
// including a domain allow-list.
func (p *MicrosandboxProvider) SetNetworkPolicy(policy NetworkPolicy) error {
	switch policy.Mode {
	case NetworkAllowAll, NetworkDenyAll, NetworkAllowList:
		p.policy = policy
		return nil
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrPolicyUnsupported, policy.Mode)
	}
}

// Open implements [Provider].
func (p *MicrosandboxProvider) Open(ctx context.Context, runID string) (Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if sb, ok := p.opened[runID]; ok && !sb.isClosed() {
		return sb, nil
	}

	name := safeName("bonnie-", runID)
	sb := &cliSandbox{
		id:      runID,
		name:    name,
		bin:     p.bin,
		backend: "microsandbox",
		execArgs: func(c *cliSandbox, argv []string, cmd Command) []string {
			args := []string{"exec", c.name, "--no-tty"}
			if dir := cmd.workdir(); dir != "" {
				args = append(args, "--workdir", dir)
			}
			args = append(args, envArgs("--env", cmd.Env)...)
			// Everything after -- is the guest command.
			args = append(args, "--")
			return append(args, argv...)
		},
		startFn: p.ensureRunning,
		stopFn: func(ctx context.Context, c *cliSandbox) error {
			_, _, _, err := runCLI(ctx, nil, c.bin, "stop", c.name)
			return err
		},
		deleteFn: func(ctx context.Context, c *cliSandbox) error {
			_, _, _, err := runCLI(ctx, nil, c.bin, "rm", c.name)
			return err
		},
		// msb has a documented `cp`. Exec-with-stdin is not documented to
		// carry bytes, so file I/O goes through cp, which is.
		readFn:  p.copyOut,
		writeFn: p.copyIn,
	}

	if err := p.ensureRunning(ctx, sb); err != nil {
		return nil, err
	}
	p.opened[runID] = sb
	return sb, nil
}

// ensureRunning creates the sandbox when absent and starts it when stopped.
func (p *MicrosandboxProvider) ensureRunning(ctx context.Context, sb *cliSandbox) error {
	if p.exists(ctx, sb.name) {
		// Starting an already-running sandbox is not an error worth
		// failing on: the goal is only that it is up.
		_, _, _, _ = runCLI(ctx, nil, p.bin, "start", sb.name)
		return p.ensureWorkspace(ctx, sb)
	}

	args := []string{"create", "--name", sb.name}
	if p.memory > 0 {
		args = append(args, "--memory", fmt.Sprint(p.memory))
	}
	if p.cpus > 0 {
		args = append(args, "--cpus", fmt.Sprint(p.cpus))
	}
	args = append(args, p.image)

	if _, stderr, code, err := runCLI(ctx, nil, p.bin, args...); err != nil || code != 0 {
		return fmt.Errorf("bonnie: sandbox: create microsandbox from %s: %s",
			p.image, firstLine(stderr))
	}
	if _, _, _, err := runCLI(ctx, nil, p.bin, "start", sb.name); err != nil {
		return fmt.Errorf("bonnie: sandbox: start %s: %w", sb.name, err)
	}
	return p.ensureWorkspace(ctx, sb)
}

// ensureWorkspace creates the workspace directory inside the guest.
func (p *MicrosandboxProvider) ensureWorkspace(ctx context.Context, sb *cliSandbox) error {
	_, _, _, err := runCLI(ctx, nil, p.bin, "exec", sb.name, "--no-tty", "--",
		"mkdir", "-p", Workspace)
	if err != nil {
		return fmt.Errorf("bonnie: sandbox: create workspace: %w", err)
	}
	return nil
}

// exists reports whether a named sandbox is known to msb.
func (p *MicrosandboxProvider) exists(ctx context.Context, name string) bool {
	stdout, _, code, err := runCLI(ctx, nil, p.bin, "ps", "--format", "json")
	if err != nil || code != 0 {
		return false
	}
	// A name match in the JSON listing is enough: names are unique, and the
	// worst case is one redundant create that msb itself rejects.
	return strings.Contains(stdout, `"`+name+`"`)
}

// copyOut reads a guest file through `msb cp`.
func (p *MicrosandboxProvider) copyOut(ctx context.Context, c *cliSandbox, guestPath string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "bonnie-msb-")
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	local := filepath.Join(tmp, "file")
	_, stderr, code, err := runCLI(ctx, nil, c.bin, "cp", c.name+":"+guestPath, local)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		if isMissingPath(stderr) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, guestPath)
		}
		return nil, fmt.Errorf("bonnie: sandbox: read %s: %s", guestPath, firstLine(stderr))
	}

	data, err := os.ReadFile(local)
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: read copied file: %w", err)
	}
	return data, nil
}

// copyIn writes a guest file through `msb cp`.
func (p *MicrosandboxProvider) copyIn(ctx context.Context, c *cliSandbox, guestPath string, data []byte) error {
	tmp, err := os.MkdirTemp("", "bonnie-msb-")
	if err != nil {
		return fmt.Errorf("bonnie: sandbox: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	local := filepath.Join(tmp, "file")
	if err := os.WriteFile(local, data, 0o600); err != nil {
		return fmt.Errorf("bonnie: sandbox: stage file: %w", err)
	}

	// cp does not create the parent, so make it first.
	if _, _, _, err := runCLI(ctx, nil, c.bin, "exec", c.name, "--no-tty", "--",
		"mkdir", "-p", parentDir(guestPath)); err != nil {
		return fmt.Errorf("bonnie: sandbox: create parent of %s: %w", guestPath, err)
	}

	_, stderr, code, err := runCLI(ctx, nil, c.bin, "cp", local, c.name+":"+guestPath)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("bonnie: sandbox: write %s: %s", guestPath, firstLine(stderr))
	}
	return nil
}
