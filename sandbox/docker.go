package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// DockerProvider runs each run in its own long-lived container, driven through
// the docker CLI.
//
// The CLI is deliberate. It adds no Go dependency, it works with any
// docker-compatible binary (Docker Desktop, OrbStack, Colima, podman with its
// docker shim), and it does not pin BONNIE to a client library version. eve
// drives Docker the same way, for the same reasons.
//
// Isolation is the container boundary: namespaces and cgroups, not a guest
// kernel. That stops an agent from editing the host filesystem, which is the
// common failure. It is weaker than a microVM against hostile code — use
// [MicrosandboxProvider] when the threat model needs one.
type DockerProvider struct {
	bin    string
	image  string
	policy NetworkPolicy
	user   string
	memory string

	mu     sync.Mutex
	opened map[string]*cliSandbox
}

var (
	_ Provider  = (*DockerProvider)(nil)
	_ Networked = (*DockerProvider)(nil)
)

// DockerOption configures a [DockerProvider].
type DockerOption func(*DockerProvider)

// WithDockerImage sets the image. The default is "alpine:3.19", which is small
// and has a shell; a real agent usually wants a richer one.
func WithDockerImage(ref string) DockerOption {
	return func(p *DockerProvider) { p.image = ref }
}

// WithDockerBinary sets the CLI to drive. The default is "docker". Use it for
// podman or a wrapper.
func WithDockerBinary(bin string) DockerOption {
	return func(p *DockerProvider) { p.bin = bin }
}

// WithDockerUser runs commands as this user, for example "1000:1000". The
// default is the image's user.
func WithDockerUser(user string) DockerOption {
	return func(p *DockerProvider) { p.user = user }
}

// WithDockerMemory caps container memory, for example "512m".
func WithDockerMemory(limit string) DockerOption {
	return func(p *DockerProvider) { p.memory = limit }
}

// Docker returns a provider backed by containers.
func Docker(opts ...DockerOption) *DockerProvider {
	p := &DockerProvider{
		bin:    "docker",
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
func (p *DockerProvider) Name() string { return "docker" }

// Available implements [Provider]. It checks the binary and then the daemon,
// because a present CLI with a dead daemon is the common case and deserves its
// own message.
func (p *DockerProvider) Available(ctx context.Context) error {
	if err := lookPath(p.bin); err != nil {
		return err
	}
	_, stderr, code, err := runCLI(ctx, nil, p.bin, "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return fmt.Errorf("%w: %s info: %v", ErrUnavailable, p.bin, err)
	}
	if code != 0 {
		return fmt.Errorf("%w: %s daemon not reachable: %s",
			ErrUnavailable, p.bin, firstLine(stderr))
	}
	return nil
}

// SetNetworkPolicy implements [Networked].
//
// Docker can attach a container to no network at all, or to the default
// bridge. It cannot filter by domain, so an allow-list is refused rather than
// silently downgraded to open egress.
func (p *DockerProvider) SetNetworkPolicy(policy NetworkPolicy) error {
	switch policy.Mode {
	case NetworkAllowAll, NetworkDenyAll:
		p.policy = policy
		return nil
	case NetworkAllowList:
		return fmt.Errorf("%w: docker cannot filter by domain; use deny-all, "+
			"or microsandbox for an allow-list", ErrPolicyUnsupported)
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrPolicyUnsupported, policy.Mode)
	}
}

// Open implements [Provider]. It reattaches to a container that already exists
// for the run, so a resumed run keeps its files.
func (p *DockerProvider) Open(ctx context.Context, runID string) (Sandbox, error) {
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
		backend: "docker",
		execArgs: func(c *cliSandbox, argv []string, cmd Command) []string {
			args := []string{"exec", "-i", "--workdir", cmd.workdir()}
			args = append(args, envArgs("--env", cmd.Env)...)
			args = append(args, c.name)
			return append(args, argv...)
		},
		startFn: p.ensureRunning,
		stopFn: func(ctx context.Context, c *cliSandbox) error {
			_, _, _, err := runCLI(ctx, nil, c.bin, "stop", c.name)
			return err
		},
		deleteFn: func(ctx context.Context, c *cliSandbox) error {
			_, _, _, err := runCLI(ctx, nil, c.bin, "rm", "-f", c.name)
			return err
		},
	}

	if err := p.ensureRunning(ctx, sb); err != nil {
		return nil, err
	}
	p.opened[runID] = sb
	return sb, nil
}

// ensureRunning creates the container when it is absent and starts it when it
// is merely stopped. This is what makes a parked run resumable: Stop releases
// the compute, and the next tool call brings the same workspace back.
func (p *DockerProvider) ensureRunning(ctx context.Context, sb *cliSandbox) error {
	state, err := p.inspectState(ctx, sb.name)
	if err != nil {
		return err
	}

	switch state {
	case "running":
		return nil
	case "":
		return p.create(ctx, sb)
	default:
		if _, stderr, code, err := runCLI(ctx, nil, p.bin, "start", sb.name); err != nil || code != 0 {
			return fmt.Errorf("bonnie: sandbox: start container %s: %s", sb.name, firstLine(stderr))
		}
		return nil
	}
}

// inspectState returns the container's state, or "" when it does not exist.
func (p *DockerProvider) inspectState(ctx context.Context, name string) (string, error) {
	stdout, _, code, err := runCLI(ctx, nil, p.bin, "inspect", "--format", "{{.State.Status}}", name)
	if err != nil {
		return "", fmt.Errorf("bonnie: sandbox: inspect %s: %w", name, err)
	}
	if code != 0 {
		return "", nil // no such container
	}
	return strings.TrimSpace(stdout), nil
}

// create starts a new container that idles until BONNIE runs something in it.
func (p *DockerProvider) create(ctx context.Context, sb *cliSandbox) error {
	args := []string{
		"run", "--detach", "--name", sb.name,
		"--workdir", Workspace,
		"--label", "bonnie.run=" + sb.id,
	}
	if p.policy.Mode == NetworkDenyAll {
		args = append(args, "--network", "none")
	}
	if p.user != "" {
		args = append(args, "--user", p.user)
	}
	if p.memory != "" {
		args = append(args, "--memory", p.memory)
	}
	// The container must stay alive between tool calls so the workspace
	// persists across a turn. `sleep infinity` is not in every busybox, so
	// a portable loop is safer.
	args = append(args, "--entrypoint", "sh", p.image,
		"-c", "mkdir -p "+Workspace+" && while true; do sleep 3600; done")

	if _, stderr, code, err := runCLI(ctx, nil, p.bin, args...); err != nil || code != 0 {
		return fmt.Errorf("bonnie: sandbox: create container from %s: %s",
			p.image, firstLine(stderr))
	}
	// A fresh image may not have the workspace yet, and the entrypoint runs
	// in parallel with the first exec.
	if _, _, _, err := runCLI(ctx, nil, p.bin, "exec", sb.name, "mkdir", "-p", Workspace); err != nil {
		return fmt.Errorf("bonnie: sandbox: create workspace: %w", err)
	}
	return nil
}

// firstLine trims a CLI error down to something a human reads.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
