package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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

	policyMu sync.RWMutex
	mu       sync.Mutex
	opened   map[string]*cliSandbox
}

var (
	_ Provider  = (*DockerProvider)(nil)
	_ Networked = (*DockerProvider)(nil)
	_ Imaged    = (*DockerProvider)(nil)
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

// Image implements [Imaged].
func (p *DockerProvider) Image() string { return p.image }

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
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
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
		killFn: func(ctx context.Context, c *cliSandbox) error {
			_, stderr, code, err := runCLI(ctx, nil, c.bin, "kill", c.name)
			return cliError("kill container "+c.name, firstLine(stderr), code, err)
		},
		stopFn: func(ctx context.Context, c *cliSandbox) error {
			_, stderr, code, err := runCLI(ctx, nil, c.bin, "stop", c.name)
			return cliError("stop container "+c.name, firstLine(stderr), code, err)
		},
		deleteFn: func(ctx context.Context, c *cliSandbox) error {
			_, stderr, code, err := runCLI(ctx, nil, c.bin, "rm", "-f", c.name)
			return cliError("remove container "+c.name, firstLine(stderr), code, err)
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
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()
	name, state, err := p.locate(ctx, sb.id)
	if err != nil {
		return err
	}
	sb.name = name
	switch state {
	case "running":
		return nil
	case "":
		return p.create(ctx, sb)
	default:
		_, stderr, code, err := runCLI(ctx, nil, p.bin, "start", sb.name)
		return cliError("start container "+sb.name, firstLine(stderr), code, err)
	}
}

// inspectState returns the container's state, or "" when it does not exist.
func (p *DockerProvider) inspectState(ctx context.Context, name string) (string, error) {
	stdout, stderr, code, err := runCLI(ctx, nil, p.bin, "inspect", "--format", "{{.State.Status}}", name)
	if err != nil {
		return "", fmt.Errorf("bonnie: sandbox: inspect %s: %w", name, err)
	}
	if code != 0 {
		if strings.Contains(strings.ToLower(stderr), "no such object:") || strings.Contains(strings.ToLower(stderr), "no such container:") {
			return "", nil
		}
		return "", cliError("inspect "+name, firstLine(stderr), code, nil)
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
	args = append(args, "--entrypoint", "sh", p.image, "-c", idleScript)

	_, stderr, code, err := runCLI(ctx, nil, p.bin, args...)
	if cerr := cliError("create container from "+p.image, firstLine(stderr), code, err); cerr != nil {
		return cerr
	}
	// A fresh image may not have the workspace yet, and the entrypoint runs
	// in parallel with the first exec.
	_, stderr, code, err = runCLI(ctx, nil, p.bin, "exec", sb.name, "mkdir", "-p", Workspace)
	if err := cliError("create workspace", firstLine(stderr), code, err); err != nil {
		return err
	}
	return nil
}

// idleScript is the container's entrypoint: it keeps the container alive
// between tool calls, so the workspace persists across a turn.
//
// The shell runs as PID 1, and the kernel delivers no signal to PID 1 that
// it has no handler for. A bare `while true; do sleep 3600; done` therefore
// ignored the SIGTERM from `docker stop`. Docker waited its full 10 s grace
// period and then sent SIGKILL, so each [cliSandbox.Stop] — each parked run —
// blocked for 10 s. The trap makes the SIGTERM end the container at once.
// The sleep runs in the background under `wait`, because a shell runs a trap
// only between commands, and a foreground sleep holds it for up to an hour.
// Guard test: TestDockerStopIsPrompt.
//
// `sleep infinity` is not in every busybox, so a portable loop is safer.
const idleScript = "trap 'exit 0' TERM INT; mkdir -p " + Workspace +
	" && while true; do sleep 3600 & wait $!; done"

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

// checkContainer refuses adoption unless identity and host controls match.
// Labels alone do not prove isolation: privileged mode, mounts, namespaces,
// extra capabilities, and a changed network must also be checked.
func (p *DockerProvider) checkContainer(ctx context.Context, name, runID string) error {
	stdout, stderr, code, err := runCLI(ctx, nil, p.bin, "inspect", "--format", "{{json .}}", name)
	if err := cliError("inspect controls "+name, firstLine(stderr), code, err); err != nil {
		return err
	}
	var got struct {
		Config struct {
			Image, User string
			Labels      map[string]string
		}
		HostConfig struct {
			Privileged                                                       bool
			NetworkMode, PidMode, IpcMode, UTSMode, UsernsMode, CgroupnsMode string
			Binds, CapAdd, SecurityOpt                                       []string
			Devices                                                          []json.RawMessage
			Memory                                                           int64
		}
		Mounts []json.RawMessage
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		return fmt.Errorf("bonnie: sandbox: inspect controls: %w", err)
	}
	h := got.HostConfig
	networkOK := h.NetworkMode == "default" || h.NetworkMode == "bridge"
	if p.policy.Mode == NetworkDenyAll {
		networkOK = h.NetworkMode == "none"
	}
	var memoryOK bool
	if p.memory != "" {
		// Docker reports bytes. Compare against Docker's accepted binary suffixes.
		v := strings.ToLower(p.memory)
		multiplier := int64(1)
		if len(v) > 0 {
			switch v[len(v)-1] {
			case 'k':
				multiplier = 1024
			case 'm':
				multiplier = 1024 * 1024
			case 'g':
				multiplier = 1024 * 1024 * 1024
			}
			if multiplier != 1 {
				v = v[:len(v)-1]
			}
		}
		n, err := strconv.ParseInt(v, 10, 64)
		memoryOK = err == nil && h.Memory == n*multiplier
	} else {
		memoryOK = h.Memory == 0
	}
	identity, labeled := got.Config.Labels["bonnie.run"]
	if !labeled || identity != runID || got.Config.Image != p.image || got.Config.User != p.user || !networkOK || !memoryOK || h.Privileged || len(got.Mounts) > 0 || len(h.Binds) > 0 || len(h.CapAdd) > 0 || len(h.Devices) > 0 || len(h.SecurityOpt) > 0 || h.PidMode != "" || h.UTSMode != "" || h.UsernsMode != "" || (h.IpcMode != "" && h.IpcMode != "private" && h.IpcMode != "shareable") || (h.CgroupnsMode != "" && h.CgroupnsMode != "private") {
		return fmt.Errorf("%w: container %s identity or isolation controls differ; refuse adoption", ErrPolicyMismatch, name)
	}
	return nil
}

// locate resolves current or legacy identity without creating a container.
// The same checks apply before adoption and before destructive lifecycle work.
func (p *DockerProvider) locate(ctx context.Context, runID string) (string, string, error) {
	name := safeName("bonnie-", runID)
	state, err := p.inspectState(ctx, name)
	if err != nil {
		return name, "", err
	}
	if state == "" && name != legacySafeName("bonnie-", runID) {
		old := legacySafeName("bonnie-", runID)
		oldState, err := p.inspectState(ctx, old)
		if err != nil {
			return name, "", err
		}
		if oldState != "" {
			name, state = old, oldState
		}
	}
	if state != "" {
		if err := p.checkContainer(ctx, name, runID); err != nil {
			return name, state, err
		}
	}
	return name, state, nil
}
