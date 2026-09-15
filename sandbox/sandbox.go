// Package sandbox gives a BONNIE run an isolated place to run tool calls.
//
// BONNIE executes the tool calls a model chooses. Without a sandbox those
// calls run as the host process, with its files, its network, and its
// credentials. This package is the seam that stops that.
//
// # The layers
//
//	Provider   opens one Sandbox per run
//	Sandbox    runs commands and moves files
//	Tools      the model-facing bash/read_file/write_file, which proxy in
//
// Tools run in the BONNIE process and proxy into the sandbox. The model never
// holds a handle to the sandbox and never sees a credential: it drives work
// through tool calls and reads their results. That keeps every sandbox call on
// the same journalling, approval, and event path as any other tool.
//
// # Lifetimes are decoupled
//
// A run parks for human input without holding sandbox compute. [Agent] opens
// the sandbox on the first tool call that needs it, not when the run starts,
// and [Sandbox.Stop] releases compute while keeping the workspace. A run that
// waits a week costs nothing until it resumes.
//
// # One path namespace
//
// Every backend roots the agent's files at [Workspace]. A path means the same
// thing whether the backend is local, Docker, or microsandbox, so a
// conversation that resumes on a different backend still finds its files.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Workspace is the working directory for every command, on every backend. A
// relative path resolves from here; an absolute path is used unchanged.
const Workspace = "/workspace"

// Sentinel errors. Test with [errors.Is].
var (
	// ErrUnavailable means the backend is not usable on this host: the
	// binary is missing, the daemon is down, or the platform is wrong.
	ErrUnavailable = errors.New("bonnie: sandbox backend unavailable")

	// ErrNotFound means the path does not exist inside the sandbox.
	ErrNotFound = errors.New("bonnie: path not found in sandbox")

	// ErrOutsideWorkspace means a path would leave the sandbox workspace.
	// Backends that map the workspace onto a host directory refuse such a
	// path rather than following it, because a file tool runs in the BONNIE
	// process and is not covered by a kernel restriction.
	ErrOutsideWorkspace = errors.New("bonnie: path is outside the sandbox workspace")

	// ErrClosed means the sandbox handle is closed.
	ErrClosed = errors.New("bonnie: sandbox is closed")

	// ErrPolicyUnsupported means the backend cannot enforce the requested
	// network policy. It is never returned for a policy a backend can
	// enforce more strictly than asked.
	ErrPolicyUnsupported = errors.New("bonnie: sandbox cannot enforce this network policy")

	// ErrPolicyMismatch means a sandbox already exists with a different
	// network policy than the one configured now. Network policy is fixed at
	// create time in every CLI backend, so the operator must decide: restore
	// the matching policy, or delete the sandbox and lose the workspace.
	ErrPolicyMismatch = errors.New("bonnie: sandbox exists with a different network policy")
)

// Command is one command to run inside a sandbox.
//
// Args is an argv, not a shell string: Args[0] is the program. To run a shell
// command, pass it explicitly, for example
// []string{"sh", "-lc", "ls | wc -l"}. Backends must not add a shell of their
// own, because a caller cannot then predict how quoting behaves.
type Command struct {
	// Args is the program and its arguments. It must not be empty.
	Args []string
	// Dir is the working directory. Empty means [Workspace]. A relative
	// path resolves from [Workspace].
	Dir string
	// Env adds environment variables, as "KEY=value".
	Env []string
	// Stdin is fed to the command.
	Stdin []byte
	// Timeout kills the command after this long. Zero means no limit beyond
	// the context.
	Timeout time.Duration
}

// Shell builds a [Command] that runs one shell command line. It is what the
// model-facing bash tool uses.
func Shell(line string) Command {
	return Command{Args: []string{"sh", "-lc", line}}
}

// Result is the outcome of a finished command.
//
// A command that ran and failed is a Result with a non-zero ExitCode, not a Go
// error. Only a failure to run the command at all is an error. This matters
// for an agent loop: "the build failed" is information the model must see and
// act on, not a transport fault.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// OK reports whether the command exited zero.
func (r *Result) OK() bool { return r != nil && r.ExitCode == 0 }

// Output returns stdout, or stderr when stdout is empty. It is what a tool
// hands back to the model.
func (r *Result) Output() string {
	if r == nil {
		return ""
	}
	if strings.TrimSpace(r.Stdout) == "" {
		return r.Stderr
	}
	return r.Stdout
}

// Sandbox is one isolated workspace, bound to one run.
//
// Implementations must be safe for concurrent use: an agent may run tools in
// parallel.
type Sandbox interface {
	// ID is stable for the life of the sandbox and survives a reconnect.
	// Use it as a key for per-sandbox state.
	ID() string

	// Exec runs one command to completion.
	//
	// Every call must actually run the command. An adapter over a build
	// engine or any other memoizing runtime must defeat its own cache. A
	// cached tool result is indistinguishable from a real one to the model,
	// so a repeated side effect becomes invisible: the run believes it
	// charged a customer twice when it charged once.
	//
	// A non-zero exit is a Result, not an error.
	Exec(ctx context.Context, cmd Command) (*Result, error)

	// ReadFile reads a file. It returns [ErrNotFound] when the path is
	// absent. The bytes are returned unchanged, so binary files are safe.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// WriteFile writes a file, creating parent directories as needed.
	WriteFile(ctx context.Context, path string, data []byte) error

	// Stop releases compute but keeps the workspace, so a later Open with
	// the same run ID finds the same files. This is what a parked run
	// calls: it is the difference between a suspended run that costs
	// nothing and one that holds a VM for a week.
	Stop(ctx context.Context) error

	// Close releases this handle. It does not destroy the sandbox.
	Close() error
}

// Deleter is implemented by a [Sandbox] that can destroy its own state.
// Type-assert for it; not every backend can.
type Deleter interface {
	// Delete removes the sandbox and its workspace for good.
	Delete(ctx context.Context) error
}

// Provider opens sandboxes. It is the thing a host chooses: local for
// development, Docker or microsandbox for real isolation.
type Provider interface {
	// Name identifies the backend in logs, errors, and journal records.
	Name() string

	// Available reports whether this backend can run here. It returns
	// [ErrUnavailable] wrapped with the reason when it cannot, so an
	// operator gets "docker daemon not reachable" rather than a failure on
	// the first tool call.
	Available(ctx context.Context) error

	// Open returns the sandbox for a run, creating it when new and
	// reattaching when it already exists. Calling it twice with the same
	// run ID must give the same workspace.
	Open(ctx context.Context, runID string) (Sandbox, error)
}

// NetworkMode is the coarse egress setting of a sandbox.
type NetworkMode string

const (
	// NetworkAllowAll permits all egress. It is the default, and it is the
	// wrong choice for untrusted work.
	NetworkAllowAll NetworkMode = "allow-all"
	// NetworkDenyAll blocks all egress, including DNS.
	NetworkDenyAll NetworkMode = "deny-all"
	// NetworkAllowList permits only the hosts in Allow.
	NetworkAllowList NetworkMode = "allow-list"
)

// NetworkPolicy describes what a sandbox may reach.
//
// Backends differ in what they can enforce, and the difference is not
// cosmetic. A backend that cannot enforce a policy returns
// [ErrPolicyUnsupported] rather than quietly running with open egress, because
// a policy that silently does nothing is worse than no policy: the operator
// believes they are protected.
type NetworkPolicy struct {
	Mode NetworkMode
	// Allow lists permitted hosts when Mode is [NetworkAllowList]. An entry
	// may be a domain or a wildcard such as "*.github.com".
	Allow []string
}

// Networked is implemented by a [Provider] whose sandboxes can constrain
// egress. Type-assert for it before promising a caller any isolation.
type Networked interface {
	// SetNetworkPolicy applies a policy to sandboxes this provider opens.
	SetNetworkPolicy(p NetworkPolicy) error
}

// Imaged is implemented by a [Provider] that runs a named image. It is how a
// host reports the image that is really in force — the default one, or the
// override it asked for — rather than the one it hopes reached the backend.
// A provider that runs no image, such as [LocalProvider], does not implement
// it.
type Imaged interface {
	// Image returns the image reference sandboxes are opened from.
	Image() string
}

// WorkingDirReporter is implemented by a [Provider] whose commands do not run
// at [Workspace].
//
// Most backends give the agent a guest filesystem, so [Workspace] is both the
// namespace and the real path. A backend that maps the workspace onto a host
// directory instead — [LocalProvider], [LandlockProvider] — runs commands at
// that host path, and `pwd` reports it.
//
// This exists so the system prompt can name the directory the tools actually
// use. Kit renders a working directory into the prompt, and a model believes
// the prompt over its own observation: telling it /workspace when `pwd` says
// otherwise is a disagreement a live model hit — "my workspace is not
// actually /workspace" — when the value was hard-coded.
//
// It takes a run ID because the directory is per run, and it must not open
// the sandbox: the prompt is built before the first tool call, and opening
// would start compute a parked run should not hold.
//
// An implementation returns the empty string when the backend does run at
// [Workspace] after all. That matters for a wrapper such as [Seeded], which
// must forward this method to stay transparent and cannot know in advance
// whether the provider beneath it maps to a host path.
type WorkingDirReporter interface {
	// WorkingDir returns the path commands for runID really run at, or ""
	// when that path is [Workspace].
	WorkingDir(runID string) string
}

// Resolve anchors a path to [Workspace]. An absolute path passes through
// unchanged; a relative one resolves from the workspace root.
//
// It also cleans the result, so "a/../../etc/passwd" cannot climb out of the
// workspace by accident. It is not a security control: a command inside the
// sandbox can name any path it likes. The isolation comes from the backend,
// never from this function.
func Resolve(p string) string {
	if p == "" {
		return Workspace
	}
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(Workspace, p)
}

// validate checks a command before a backend tries to run it.
func (c Command) validate() error {
	if len(c.Args) == 0 {
		return fmt.Errorf("bonnie: sandbox: command has no arguments")
	}
	for _, e := range c.Env {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("bonnie: sandbox: env entry %q is not KEY=value", e)
		}
	}
	return nil
}

// workdir returns the directory the command runs in.
func (c Command) workdir() string {
	if c.Dir == "" {
		return Workspace
	}
	return Resolve(c.Dir)
}
