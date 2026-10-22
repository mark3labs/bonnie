package selfmod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/sandbox"
)

// Status values of a modification's durable state.
const (
	statusStaged   = "staged"
	statusApplying = "applying"
	statusDone     = "done"
)

// state is the durable progress of one modification. It lives in the call's
// work directory beside the staged tree, so a call that a crash or a dev
// restart cut off continues from it instead of starting again.
type state struct {
	Request string   `json:"request"`
	Base    snapshot `json:"base"`
	Status  string   `json:"status"`
	Changes []change `json:"changes,omitempty"`
	// Unverified is set when the host had no go toolchain to build with.
	Unverified bool `json:"unverified,omitempty"`
	// Ignored lists files the subagent wrote outside the authored layout.
	// They are not applied.
	Ignored []string `json:"ignored,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Diff    string   `json:"diff,omitempty"`
	// Result is the tool result of a finished call. A repeated call returns
	// it unchanged.
	Result string `json:"result,omitempty"`
}

// job is one call of a self-modification tool.
type job struct {
	cfg  Config
	kit  *kit.Kit
	live string // the tree root the change applies to
	work string // the call's own directory: state, base, staged tree
	// requireBuild refuses a change that could not be built because the
	// host has no go toolchain. Local modification requires a build; a
	// remote proposal goes to review and CI instead.
	requireBuild bool
}

func (j *job) statePath() string { return filepath.Join(j.work, "state.json") }
func (j *job) baseDir() string   { return filepath.Join(j.work, "base") }
func (j *job) treeDir() string   { return filepath.Join(j.work, "tree") }

// load returns the saved state, or nil when the call is new.
func (j *job) load() (*state, error) {
	b, err := os.ReadFile(j.statePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bonnie: selfmod: read state: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("bonnie: selfmod: corrupt state %s: %w", j.statePath(), err)
	}
	return &st, nil
}

func (j *job) save(st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("bonnie: selfmod: encode state: %w", err)
	}
	return writeFileAtomic(j.statePath(), b)
}

// prepare loads the call's state, or snapshots the live tree into a new
// base copy and staged copy.
func (j *job) prepare(request string) (*state, error) {
	st, err := j.load()
	if err != nil || st != nil {
		return st, err
	}
	base, err := takeSnapshot(j.live)
	if err != nil {
		return nil, err
	}
	// A retry after a failed first attempt starts from a clean copy.
	for _, dir := range []string{j.baseDir(), j.treeDir()} {
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("bonnie: selfmod: reset staging: %w", err)
		}
		if err := copyFiles(j.live, dir, base.sortedKeys()); err != nil {
			return nil, err
		}
	}
	st = &state{Request: request, Base: base, Status: statusStaged}
	if err := j.save(st); err != nil {
		return nil, err
	}
	return st, nil
}

// stage lets the subagent change the staged tree until it builds, and
// records the changes in st. Zero changes is a valid outcome: the subagent
// can decide that nothing needs to change.
func (j *job) stage(ctx context.Context, st *state) error {
	feedback := ""
	for attempt := 1; attempt <= j.cfg.MaxAttempts; attempt++ {
		summary, err := j.runSubagent(ctx, buildPrompt(st, feedback))
		if err != nil {
			return err
		}
		staged, err := takeSnapshot(j.treeDir())
		if err != nil {
			return err
		}
		changes := diffSnapshots(st.Base, staged)
		st.Summary, st.Changes, st.Unverified = summary, changes, false
		st.Ignored = ignoredFiles(j.treeDir())
		if len(changes) == 0 {
			return nil
		}
		err = verify(ctx, j.cfg.buildEnv, j.live, j.treeDir(), filepath.Join(j.work, "verify"), changes)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errNoToolchain) && !j.requireBuild:
			st.Unverified = true
			return nil
		case errors.Is(err, errBuild):
			feedback = err.Error()
			continue
		default:
			return err
		}
	}
	return fmt.Errorf("the change still does not build after %d attempts; the live tree is unchanged.\n%s", j.cfg.MaxAttempts, feedback)
}

// runSubagent runs one subagent turn on the staged tree. The subagent's
// tools are BONNIE's sandbox tools, confined to the staged copy: it never
// sees the live tree, the journal, or a credential.
func (j *job) runSubagent(ctx context.Context, prompt string) (string, error) {
	provider, err := j.cfg.Workspace(ctx, j.treeDir())
	if err != nil {
		return "", fmt.Errorf("bonnie: selfmod: open workspace: %w", err)
	}
	var (
		mu sync.Mutex
		sb sandbox.Sandbox
	)
	open := func(ctx context.Context) (sandbox.Sandbox, error) {
		mu.Lock()
		defer mu.Unlock()
		if sb == nil {
			opened, err := provider.Open(ctx, "self-modification")
			if err != nil {
				return nil, err
			}
			sb = opened
		}
		return sb, nil
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		if sb != nil {
			_ = sb.Close()
		}
	}()
	res, err := j.cfg.subagent(ctx, j.kit, kit.SubagentConfig{
		Prompt:       prompt,
		Model:        j.cfg.Model,
		SystemPrompt: systemPrompt,
		Tools:        sandbox.Tools(open),
		NoSession:    true,
		Timeout:      j.cfg.Timeout,
	})
	if err != nil {
		return "", fmt.Errorf("bonnie: selfmod: subagent: %w", err)
	}
	return strings.TrimSpace(res.Response), nil
}

// workDir is the directory of one tool call under base. The name is a hash,
// so a run or call ID cannot name a path.
func workDir(base, runID, callID string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + callID))
	return filepath.Join(base, hex.EncodeToString(sum[:12]))
}

// callID identifies a tool call. Recovery repeats an interrupted call with
// its recorded ID; without one, the arguments identify the call.
func callID(ctx context.Context, args ...string) string {
	if id := kit.ToolCallIDFromContext(ctx); id != "" {
		return id
	}
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return "args-" + hex.EncodeToString(sum[:12])
}

// locks serialises calls on one tree. Two calls in one model step would
// otherwise both stage from the same base and the second would conflict.
var locks sync.Map

func lockTree(root string) func() {
	v, _ := locks.LoadOrStore(root, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// defaultTimeout bounds one subagent attempt. Kit's own default of five
// minutes is short for a change that adds a tool.
const defaultTimeout = 20 * time.Minute
