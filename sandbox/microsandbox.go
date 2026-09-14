package sandbox

import (
	"context"
	"encoding/json"
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
	_ Imaged    = (*MicrosandboxProvider)(nil)
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

// Image implements [Imaged].
func (p *MicrosandboxProvider) Image() string { return p.image }

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

// SetNetworkPolicy implements [Networked].
//
// Every mode is enforced. msb applies a network policy when a sandbox is
// created, and this adapter passes the configured policy through as create
// flags:
//
//	allow-all  → no flags, the msb default: egress to the public internet
//	deny-all   → --no-net
//	allow-list → --no-net --net-rule allow@<host> for each host
//
// A policy is a property of the sandbox, not of the handle. `msb modify`
// cannot change network rules, so a sandbox that already exists keeps the
// policy it was created with. Open verifies the live policy still matches
// the configured one and returns [ErrPolicyMismatch] when it does not:
// an operator who tightened the policy and restarted the host must hear
// that the old sandbox keeps the old rules, not find out from a leak.
func (p *MicrosandboxProvider) SetNetworkPolicy(policy NetworkPolicy) error {
	switch policy.Mode {
	case NetworkAllowAll, NetworkDenyAll:
	case NetworkAllowList:
		for _, host := range policy.Allow {
			if strings.TrimSpace(host) == "" {
				return fmt.Errorf("bonnie: sandbox: network policy allow-list has an empty host")
			}
		}
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrPolicyUnsupported, policy.Mode)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policy = policy
	return nil
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
			// -f stops the sandbox first. Without it msb refuses to remove
			// a running sandbox ("still running"), and because a delete
			// failure is usually ignored by the caller, the microVM is
			// leaked in silence along with its memory.
			_, stderr, code, err := runCLI(ctx, nil, c.bin, "rm", "--force", c.name)
			if err != nil {
				return err
			}
			if code != 0 {
				return fmt.Errorf("bonnie: sandbox: rm %s: %s", c.name, firstLine(stderr))
			}
			return nil
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
		p.start(ctx, sb.name)
		// After start, not before: `msb inspect` reports no active config
		// for a stopped sandbox, so the policy check needs the sandbox up.
		// On a mismatch Open fails with the sandbox running, which is the
		// same leftover every Open failure leaves; reclaiming those is
		// T-012's reconciler.
		if err := p.checkPolicy(ctx, sb); err != nil {
			return err
		}
		return p.ensureWorkspace(ctx, sb)
	}

	net, err := netArgs(p.policy)
	if err != nil {
		return err
	}
	args := []string{"create", "--name", sb.name}
	if p.memory > 0 {
		args = append(args, "--memory", fmt.Sprint(p.memory))
	}
	if p.cpus > 0 {
		args = append(args, "--cpus", fmt.Sprint(p.cpus))
	}
	args = append(args, net...)
	args = append(args, p.image)

	if _, stderr, code, err := runCLI(ctx, nil, p.bin, args...); err != nil || code != 0 {
		return fmt.Errorf("bonnie: sandbox: create microsandbox from %s: %s",
			p.image, firstLine(stderr))
	}
	p.start(ctx, sb.name)
	return p.ensureWorkspace(ctx, sb)
}

// start brings a sandbox up and tolerates one that is already up.
//
// `msb create` starts the sandbox it creates, so the start that follows a
// create reports "already running". That is the wanted state, not a failure,
// and the call is kept because nothing documents create-implies-start as a
// promise. Only the guest command that follows proves the sandbox is usable.
func (p *MicrosandboxProvider) start(ctx context.Context, name string) {
	_, _, _, _ = runCLI(ctx, nil, p.bin, "start", name)
}

// netArgs maps a policy onto the create flags msb understands.
//
// `--no-net` is sugar for a default-deny policy, and msb composes it with
// `--net-rule` entries into an allow-list: without rules the guest has no
// reachability at all, with rules only the listed hosts. Both behaviors were
// verified against msb 0.6.18, not read from documentation alone.
func netArgs(policy NetworkPolicy) ([]string, error) {
	switch policy.Mode {
	case NetworkAllowAll:
		return nil, nil
	case NetworkDenyAll:
		return []string{"--no-net"}, nil
	case NetworkAllowList:
		args := []string{"--no-net"}
		for _, host := range policy.Allow {
			args = append(args, "--net-rule", "allow@"+host)
		}
		return args, nil
	default:
		return nil, fmt.Errorf("%w: unknown mode %q", ErrPolicyUnsupported, policy.Mode)
	}
}

// msbRule is one network rule as `msb inspect --format json` reports it.
type msbRule struct {
	Action      string `json:"action"`
	Direction   string `json:"direction"`
	Destination struct {
		Domain       string `json:"domain"`
		DomainSuffix string `json:"domain_suffix"`
	} `json:"destination"`
}

// msbNetworkPolicy is the policy object under `active_config.network.policy`.
// A null policy means no policy was set at create time, which is the msb
// default: egress to the public internet.
type msbNetworkPolicy struct {
	DefaultEgress string    `json:"default_egress"`
	Rules         []msbRule `json:"rules"`
}

// checkPolicy verifies that an existing sandbox carries the configured
// network policy.
//
// The sandbox must be running: a stopped one reports no active config, so
// this runs after start. When the policy differs from the configured one the
// error says what to do, because the two remedies are both destructive in
// different ways — restore the old policy, or delete the sandbox and lose
// the workspace.
func (p *MicrosandboxProvider) checkPolicy(ctx context.Context, sb *cliSandbox) error {
	stdout, stderr, code, err := runCLI(ctx, nil, p.bin, "inspect", "--format", "json", sb.name)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("bonnie: sandbox: inspect %s: %s", sb.name, firstLine(stderr))
	}
	var report struct {
		ActiveConfig struct {
			Network struct {
				Policy *msbNetworkPolicy `json:"policy"`
			} `json:"network"`
		} `json:"active_config"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		return fmt.Errorf("bonnie: sandbox: inspect %s: %w", sb.name, err)
	}
	if policyMatches(report.ActiveConfig.Network.Policy, p.policy) {
		return nil
	}
	return fmt.Errorf("%w: %s was created with a different network policy and "+
		"msb fixes policy at create time; restore the matching policy, or "+
		"delete the sandbox and lose the workspace",
		ErrPolicyMismatch, sb.name)
}

// policyMatches reports whether the policy msb reports for a sandbox is the
// one this provider would create it with.
//
// The shapes below are the ones `msb inspect --format json` produces for each
// mode, recorded from msb 0.6.18:
//
//	allow-all  → policy is null
//	deny-all   → default_egress "deny" and no rules
//	allow-list → default_egress "deny" and one egress allow rule per host;
//	             a plain domain reports {"domain": host}, a wildcard
//	             (*.example.com) reports {"domain_suffix": "example.com"}
func policyMatches(got *msbNetworkPolicy, want NetworkPolicy) bool {
	switch want.Mode {
	case NetworkAllowAll:
		return got == nil
	case NetworkDenyAll:
		return got != nil && strings.EqualFold(got.DefaultEgress, "deny") && len(got.Rules) == 0
	case NetworkAllowList:
		if got == nil || !strings.EqualFold(got.DefaultEgress, "deny") || len(got.Rules) != len(want.Allow) {
			return false
		}
		balance := make(map[string]int, len(want.Allow))
		for _, host := range want.Allow {
			balance[allowKey(host)]++
		}
		for _, r := range got.Rules {
			if r.Action != "allow" || r.Direction != "egress" {
				return false
			}
			var key string
			switch {
			case r.Destination.Domain != "":
				key = "domain=" + strings.ToLower(r.Destination.Domain)
			case r.Destination.DomainSuffix != "":
				key = "suffix=" + strings.ToLower(r.Destination.DomainSuffix)
			default:
				return false
			}
			balance[key]--
			if balance[key] < 0 {
				return false
			}
		}
		for _, n := range balance {
			if n != 0 {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// allowKey normalises one Allow entry to the comparison key. A plain domain
// and a wildcard are different rules in msb — example.com is a domain rule,
// *.example.com is a suffix rule — so the two must not collide.
func allowKey(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if rest, ok := strings.CutPrefix(h, "*."); ok {
		return "suffix=" + rest
	}
	return "domain=" + h
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

// exists reports whether a named sandbox is known to msb, running or not.
//
// --all is required. Without it msb lists only running sandboxes, so a
// sandbox that Stop released looks absent, Open tries to create it again, and
// msb refuses with "sandbox already exists" — which strands a run that did
// nothing wrong but park between turns.
//
// The listing is decoded rather than searched as text. A substring match also
// hits the image, command, and status fields, so a run whose ID resembles a
// value in any of them would be reported as existing when it does not.
func (p *MicrosandboxProvider) exists(ctx context.Context, name string) bool {
	stdout, _, code, err := runCLI(ctx, nil, p.bin, "ps", "--all", "--format", "json")
	if err != nil || code != 0 {
		return false
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

// msbMissingPath reports whether an `msb cp` failure means the guest path is
// absent, rather than any other failure.
//
// msb words this as `error: stat <path>`, which shares no wording with the
// shell shapes [isMissingPath] knows, so the generic helper never fires here.
// It must also stay clear of `error: sandbox not found: <name>`: that means
// the workspace itself is gone, which is a different condition and must not
// reach the model as a plain missing file. Anchoring on the path keeps the
// two apart.
func msbMissingPath(stderr, guestPath string) bool {
	return strings.Contains(stderr, "stat "+guestPath)
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
		if msbMissingPath(stderr, guestPath) {
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
