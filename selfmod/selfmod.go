// Package selfmod lets a BONNIE agent change its own authored source.
//
// It has two tools, with two different trust models:
//
//   - [Local] adds modify_agent for local development. `bonnie dev`
//     installs it by default. A subagent edits a copy of the tree, the
//     changed tree must build, and then the change is written to the live
//     tree. The dev loop rebuilds and restarts the agent, and the next message
//     on the same run is answered by the new build, with the new tools.
//   - [Remote] adds propose_agent_change for a deployed agent. A subagent
//     edits a checkout of the repository and the tool opens a draft pull
//     request. It never changes the running deployment: a change takes
//     effect only after a human reviews, merges and deploys it.
//
// Both tools run the subagent with BONNIE's sandbox tools in a staged copy
// that holds only authored files (see [Authored]). The subagent never sees
// the live tree, the journal, a .env file, or a credential.
//
// Both tools are replay-safe. A call that a crash or a dev restart cut off
// continues from its durable state in the journal directory, and a repeated
// call after the change landed returns the first result.
package selfmod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// Names of the tool factories, for [bonnie.WithNamedToolFactory]. A second
// option with the same name replaces the first, so an explicit [Local] in
// main.go replaces the default that `bonnie dev` installs.
const (
	LocalName  = "selfmod.local"
	RemoteName = "selfmod.remote"
)

// Names of the model-facing tools.
const (
	LocalTool  = "modify_agent"
	RemoteTool = "propose_agent_change"
)

// Caller is what an authorization function knows about a request.
type Caller struct {
	// RunID is the run that would call the tool.
	RunID string
	// Origin is the channel and surface the run's conversation is on.
	Origin runtime.Origin
	// Principal is the identity that sent the run's most recent message,
	// or nil when the channel recorded none. A channel without an
	// authenticator records no verified identity.
	Principal *channel.Principal
}

// Authorizer decides whether a caller may use a tool. The tool is hidden from
// the model when it returns false or an error. It runs on each Start or
// Resume of a run.
type Authorizer func(ctx context.Context, c Caller) (bool, error)

// WorkspaceFunc returns a sandbox provider whose work directory is dir. The
// subagent's tools run in it. Only a provider that can use one exact host
// directory works here: [sandbox.Landlock] and [sandbox.Local].
type WorkspaceFunc func(ctx context.Context, dir string) (sandbox.Provider, error)

// Config configures the self-modification subagent. The zero value is the
// default.
type Config struct {
	// Model is the subagent's model, as "provider/name". Empty uses the
	// agent's own model.
	Model string

	// Timeout bounds one subagent attempt. Zero means 20 minutes.
	Timeout time.Duration

	// MaxAttempts is how many times the subagent may correct a change that
	// does not build. Zero means 3.
	MaxAttempts int

	// Authorize decides who may use the tool. It is optional for [Local],
	// which only runs under `bonnie dev`, and required for [Remote].
	Authorize Authorizer

	// Workspace opens the subagent's sandbox. Nil uses [sandbox.Landlock]
	// with the staged copy as its shared work directory.
	Workspace WorkspaceFunc

	// Disabled removes the tool. It is how main.go turns off the default
	// that `bonnie dev` installs.
	Disabled bool

	// subagent runs one subagent. Nil uses [kit.Kit.Subagent]. Tests
	// replace it, because a scripted model cannot drive a child Kit.
	subagent func(context.Context, *kit.Kit, kit.SubagentConfig) (*kit.SubagentResult, error)
	// buildEnv is the environment of the verification build. Nil inherits
	// the process environment.
	buildEnv []string
}

// normalize fills the defaults.
func (c Config) normalize() Config {
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.Workspace == nil {
		c.Workspace = landlockWorkspace
	}
	if c.subagent == nil {
		c.subagent = func(ctx context.Context, k *kit.Kit, cfg kit.SubagentConfig) (*kit.SubagentResult, error) {
			return k.Subagent(ctx, cfg)
		}
	}
	return c
}

// landlockWorkspace is the default subagent sandbox: Landlock confined to
// the staged copy.
func landlockWorkspace(ctx context.Context, dir string) (sandbox.Provider, error) {
	p := sandbox.Landlock()
	if err := p.UseSharedDirectory(dir); err != nil {
		return nil, err
	}
	if err := p.Available(ctx); err != nil {
		return nil, fmt.Errorf("%w (set selfmod.Config.Workspace to use another sandbox)", err)
	}
	return p, nil
}

// authorize runs the configured authorizer, if any, for scope.
func authorize(ctx context.Context, fn Authorizer, scope bonnie.ToolScope) (bool, error) {
	if fn == nil {
		return true, nil
	}
	caller := Caller{RunID: scope.RunID}
	if scope.Session != nil {
		caller.Origin = scope.Session.Origin()
	}
	if scope.Caller != nil {
		p, err := scope.Caller(ctx)
		if err != nil {
			return false, err
		}
		caller.Principal = p
	}
	return fn(ctx, caller)
}

// Local adds modify_agent, which changes the agent's own tree in local
// development. `bonnie dev` installs Local with a zero [Config] by default;
// pass Local in main.go to choose the model, and Config.Disabled to remove
// it. `bonnie dev --no-self-modify` removes the default only.
//
// The tool exists only in a process that `bonnie dev` started: it reads the
// tree root from [bonnie.DevRootEnv]. A binary from `bonnie build`, or one
// started with `go run`, has no such variable and no tool, also when main.go
// passes Local.
//
// A call works in four steps. The tool copies the authored files to a
// staging directory under .bonnie/selfmod. A subagent edits the copy with
// sandbox tools. The tool builds the tree with the changes; when the build
// fails, the subagent gets the compiler output and tries again. Then the
// tool writes the changed files to the tree and returns a diff. The dev loop
// sees the change, rebuilds, and restarts the agent. The run keeps its
// conversation in the journal, so the next message on it is answered by the
// new build. A turn that the restart interrupts resumes in the new build.
//
// When a file changed in the tree while the subagent worked, nothing is
// written and the tool reports the conflict.
func Local(cfg Config) bonnie.Option {
	return bonnie.WithNamedToolFactory(LocalName, localFactory(cfg.normalize(), os.Getenv))
}

// localFactory builds the tool factory of [Local]. getenv reads the
// environment, so a test can run in parallel.
func localFactory(cfg Config, getenv func(string) string) bonnie.ToolFactory {
	return func(ctx context.Context, scope bonnie.ToolScope) ([]kit.Tool, error) {
		root := getenv(bonnie.DevRootEnv)
		if cfg.Disabled || root == "" {
			return nil, nil
		}
		ok, err := authorize(ctx, cfg.Authorize, scope)
		if err != nil || !ok {
			// A failed check hides the tool. It must not stop the run: the
			// agent works without self-modification.
			return nil, nil //nolint:nilerr // hidden, not failed
		}
		if scope.Session != nil {
			scope.Session.AddReplaySafeTools(LocalTool)
		}
		return []kit.Tool{localTool(cfg, scope, root)}, nil
	}
}

func localTool(cfg Config, scope bonnie.ToolScope, root string) kit.Tool {
	type input struct {
		Request string `json:"request" description:"What to change in this agent: its instructions, tools, skills or context files. Describe the result you want, not the edits."`
	}
	return kit.NewTool(LocalTool,
		"Change this agent's own source: its instructions, tools, skills and context files. "+
			"A coding subagent edits a copy of the agent tree, the changed tree must build, and then the change is written to the tree. "+
			"The development server then rebuilds and restarts the agent; the change takes effect from the next message in this conversation. "+
			"Use it only when the user asks to change the agent itself.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			if strings.TrimSpace(in.Request) == "" {
				return kit.ErrorResult("request is empty"), nil
			}
			out, err := runLocal(ctx, cfg, scope, root, in.Request)
			if err != nil {
				return kit.ErrorResult(err.Error()), nil
			}
			return kit.TextResult(out), nil
		})
}

func runLocal(ctx context.Context, cfg Config, scope bonnie.ToolScope, root, request string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("bonnie: selfmod: tree root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("bonnie: selfmod: %s is not an agent tree: %w", root, err)
	}
	unlock := lockTree(root)
	defer unlock()

	j := &job{
		cfg:          cfg,
		kit:          scope.Kit,
		live:         root,
		work:         workDir(filepath.Join(root, bonnie.DefaultJournal, "selfmod"), scope.RunID, callID(ctx, LocalTool, request)),
		requireBuild: true,
	}
	st, err := j.prepare(request)
	if err != nil {
		return "", err
	}
	if st.Status == statusDone {
		return st.Result, nil
	}
	if st.Status == statusStaged {
		if err := j.stage(ctx, st); err != nil {
			return "", err
		}
		if len(st.Changes) == 0 {
			st.Status = statusDone
			st.Result = "No change was made.\n\n" + st.Summary
			return st.Result, j.save(st)
		}
		st.Diff = renderDiff(j.baseDir(), j.treeDir(), st.Changes)
		st.Status = statusApplying
		if err := j.save(st); err != nil {
			return "", err
		}
	}
	// statusApplying: the write is idempotent, so a call that stopped here
	// writes again and finds the files already in place.
	if err := applyChanges(root, j.treeDir(), st.Changes); err != nil {
		if errors.Is(err, errConflict) {
			// The staged change cannot land. Record the outcome so a repeat
			// does not try again on a tree the user is editing.
			st.Status, st.Result = statusDone, err.Error()+". The change was not applied; ask again to start from the current tree."
			_ = j.save(st)
		}
		return "", err
	}
	st.Status = statusDone
	st.Result = localResult(st)
	if err := j.save(st); err != nil {
		return "", err
	}
	return st.Result, nil
}

// ignoredNote names the files that were not applied, or is "".
func ignoredNote(st *state) string {
	if len(st.Ignored) == 0 {
		return ""
	}
	return "\n\nNot applied, because they are outside the agent tree's layout: " + strings.Join(st.Ignored, ", ")
}

func localResult(st *state) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Applied %d file change(s) to the agent tree. The development server now rebuilds and restarts the agent. The change takes effect from the next message in this conversation: tools added now are not available in this turn. Do not call %s again for this request.\n\n", len(st.Changes), LocalTool)
	b.WriteString("Summary from the subagent:\n")
	b.WriteString(st.Summary)
	b.WriteString(ignoredNote(st))
	b.WriteString("\n\nDiff:\n```diff\n")
	b.WriteString(st.Diff)
	b.WriteString("```\n")
	return b.String()
}
