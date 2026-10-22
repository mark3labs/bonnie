package selfmod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
)

// TokenEnv is the environment variable [Credentials].PAT reads.
const TokenEnv = "BONNIE_SELF_MODIFICATION_GITHUB_TOKEN"

// Capabilities a [CredentialRequest] asks for.
const (
	// CapabilityRead clones the repository.
	CapabilityRead = "read"
	// CapabilityWrite pushes the branch and opens the pull request.
	CapabilityWrite = "write"
)

// CredentialRequest is what [Credentials].Resolve is asked for.
type CredentialRequest struct {
	// Capability is [CapabilityRead] or [CapabilityWrite].
	Capability string
	// Repository is the repository in "github.com/owner/repo" form.
	Repository string
}

// Credentials supply the GitHub token. Set exactly one field.
//
// The token is used by the BONNIE process only: for the clone, the push, and
// the GitHub API. It goes to git in the environment of the git process, not
// on its command line and not in .git/config, and the subagent's sandbox
// holds only a copy of the authored files. No command the subagent runs can
// read it.
type Credentials struct {
	// PAT reads a token from [TokenEnv]. The token needs contents and pull
	// request write permission on the repository.
	PAT bool
	// Resolve returns a token for one capability, for example a GitHub App
	// installation token.
	Resolve func(ctx context.Context, req CredentialRequest) (string, error)
}

func (c Credentials) token(ctx context.Context, capability, repo string) (string, error) {
	if c.Resolve != nil {
		tok, err := c.Resolve(ctx, CredentialRequest{Capability: capability, Repository: repo})
		if err != nil {
			return "", fmt.Errorf("bonnie: selfmod: resolve %s credential: %w", capability, err)
		}
		return tok, nil
	}
	if tok := os.Getenv(TokenEnv); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("bonnie: selfmod: %s is not set", TokenEnv)
}

// RemoteConfig configures [Remote].
type RemoteConfig struct {
	// Config configures the subagent. Config.Authorize is required.
	Config

	// Repository is the GitHub repository, as "github.com/owner/repo".
	Repository string
	// Directory is the agent tree's directory relative to the repository
	// root. Empty or "." is the root.
	Directory string
	// Branch is the branch that is cloned and that pull requests target.
	Branch string
	// Credentials supply the GitHub token. Required.
	Credentials Credentials

	// WorkDir holds the clones and the staged changes. Empty means
	// .bonnie/selfmod-remote under the working directory.
	WorkDir string
	// APIURL is the GitHub REST API root. Empty means https://api.github.com.
	APIURL string

	// cloneURL replaces the clone URL derived from Repository. Tests use a
	// local repository.
	cloneURL string
	client   *http.Client
}

// validate reports a configuration that cannot work.
func (c RemoteConfig) validate() (owner, repo string, err error) {
	if c.Authorize == nil {
		return "", "", errors.New("bonnie: selfmod: Remote needs Config.Authorize")
	}
	if c.Credentials.Resolve == nil && !c.Credentials.PAT {
		return "", "", errors.New("bonnie: selfmod: Remote needs Credentials")
	}
	if c.Credentials.Resolve != nil && c.Credentials.PAT {
		return "", "", errors.New("bonnie: selfmod: set Credentials.PAT or Credentials.Resolve, not both")
	}
	if c.Branch == "" {
		return "", "", errors.New("bonnie: selfmod: Remote needs Branch")
	}
	rest, ok := strings.CutPrefix(c.Repository, "github.com/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("bonnie: selfmod: Repository %q is not github.com/owner/repo", c.Repository)
	}
	dir := path.Clean(filepath.ToSlash(c.Directory))
	if path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") {
		return "", "", fmt.Errorf("bonnie: selfmod: Directory %q is outside the repository", c.Directory)
	}
	return parts[0], parts[1], nil
}

// Remote adds propose_agent_change, which proposes a change to the agent's
// source as a draft pull request.
//
// A call clones Branch of Repository, lets a subagent edit a staged copy of
// the tree under Directory, builds it when the host has a go toolchain,
// commits the change on its own branch, and opens a draft pull request
// against Branch. It never changes the running deployment. The change takes
// effect only after a human reviews, merges and deploys it.
//
// Config.Authorize is required: the tool is hidden from a caller it does not
// accept. The tool is also hidden, before Authorize runs, from a turn whose
// sender is not verified ([runtime.Caller].Verified): an identity that a
// caller only claimed, such as the auth field of an HTTP request with no
// authenticator, is never enough.
//
// The tool is never offered in a process that `bonnie dev` started; use
// [Local] there. Mount both: they use different names and do not replace
// each other.
//
// A configuration error, such as a missing Authorize, stops each run that
// would offer the tool, so it cannot go unnoticed.
func Remote(cfg RemoteConfig) bonnie.Option {
	cfg.Config = cfg.normalize()
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(bonnie.DefaultJournal, "selfmod-remote")
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	if cfg.client == nil {
		cfg.client = http.DefaultClient
	}
	return bonnie.WithNamedToolFactory(RemoteName, remoteFactory(cfg, os.Getenv))
}

// remoteFactory builds the tool factory of [Remote]. getenv reads the
// environment, so a test can run in parallel.
func remoteFactory(cfg RemoteConfig, getenv func(string) string) bonnie.ToolFactory {
	return func(ctx context.Context, scope bonnie.ToolScope) ([]kit.Tool, error) {
		if cfg.Disabled || getenv(bonnie.DevRootEnv) != "" {
			return nil, nil
		}
		owner, repo, err := cfg.validate()
		if err != nil {
			return nil, err
		}
		// A remote change needs a proven sender, whatever the authorizer
		// says: an unverified identity is a claim any caller can make.
		if p := callerOf(scope).Principal; p == nil || !p.Verified {
			return nil, nil
		}
		ok, err := authorize(ctx, cfg.Authorize, scope)
		if err != nil || !ok {
			return nil, nil //nolint:nilerr // hidden, not failed
		}
		if scope.Session != nil {
			scope.Session.AddReplaySafeTools(RemoteTool)
		}
		r := &remote{cfg: cfg, owner: owner, repo: repo, scope: scope}
		return []kit.Tool{r.tool()}, nil
	}
}

type remote struct {
	cfg         RemoteConfig
	owner, repo string
	scope       bonnie.ToolScope
}

func (r *remote) tool() kit.Tool {
	type input struct {
		Request string `json:"request" description:"What to change in this agent: its instructions, tools, skills or context files. Describe the result you want, not the edits."`
		Title   string `json:"title,omitempty" description:"A short title for the pull request."`
	}
	return kit.NewTool(RemoteTool,
		"Propose a change to this agent's own source as a draft pull request. "+
			"A coding subagent edits a checkout of the agent's repository and the change is opened as a draft pull request for human review. "+
			"Nothing changes in the running agent; the change takes effect only after it is reviewed, merged and deployed. "+
			"Use it only when the user asks to change the agent itself.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			if strings.TrimSpace(in.Request) == "" {
				return kit.ErrorResult("request is empty"), nil
			}
			out, err := r.run(ctx, in.Request, in.Title)
			if err != nil {
				return kit.ErrorResult(err.Error()), nil
			}
			return kit.TextResult(out), nil
		})
}

func (r *remote) run(ctx context.Context, request, title string) (string, error) {
	work, err := filepath.Abs(workDir(r.cfg.WorkDir, r.scope.RunID, callID(ctx, RemoteTool, request, title)))
	if err != nil {
		return "", fmt.Errorf("bonnie: selfmod: work directory: %w", err)
	}
	unlock := lockTree(work)
	defer unlock()

	clone := filepath.Join(work, "repo")
	j := &job{cfg: r.cfg.Config, kit: r.scope.Kit, live: filepath.Join(clone, filepath.FromSlash(path.Clean(filepath.ToSlash(r.cfg.Directory)))), work: work}
	st, err := j.load()
	if err != nil {
		return "", err
	}
	if st != nil && st.Status == statusDone {
		return st.Result, nil
	}
	if st == nil {
		if err := r.clone(ctx, clone); err != nil {
			return "", err
		}
	}
	if st, err = j.prepare(request); err != nil {
		return "", err
	}
	if st.Status == statusStaged {
		if err := j.stage(ctx, st); err != nil {
			return "", err
		}
		if len(st.Changes) == 0 {
			st.Status = statusDone
			st.Result = "No change was proposed.\n\n" + st.Summary
			return st.Result, j.save(st)
		}
		st.Diff = renderDiff(j.baseDir(), j.treeDir(), st.Changes)
		st.Status = statusApplying
		if err := j.save(st); err != nil {
			return "", err
		}
	}
	if err := applyChanges(j.live, j.treeDir(), st.Changes); err != nil {
		return "", err
	}
	head := r.headBranch(work)
	if err := r.commitAndPush(ctx, clone, head, title, request); err != nil {
		return "", err
	}
	pr, err := r.pullRequest(ctx, head, title, request, st)
	if err != nil {
		return "", err
	}
	st.Status = statusDone
	st.Result = fmt.Sprintf("Opened draft pull request %s against %s. Nothing changed in the running agent: the change takes effect after a human reviews, merges and deploys it.\n\nSummary from the subagent:\n%s%s", pr, r.cfg.Branch, st.Summary, ignoredNote(st))
	if st.Unverified {
		st.Result += "\n\nThe change was not built: this host has no go toolchain. The repository's CI must build it."
	}
	if err := j.save(st); err != nil {
		return "", err
	}
	return st.Result, nil
}

// headBranch is the pull request branch. It is derived from the call's work
// directory, so a repeated call pushes to the same branch.
func (r *remote) headBranch(work string) string {
	sum := sha256.Sum256([]byte(work))
	return "bonnie/self-modification-" + hex.EncodeToString(sum[:6])
}

func (r *remote) cloneURL() string {
	if r.cfg.cloneURL != "" {
		return r.cfg.cloneURL
	}
	return "https://" + r.cfg.Repository + ".git"
}

func (r *remote) clone(ctx context.Context, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("bonnie: selfmod: reset clone: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("bonnie: selfmod: clone: %w", err)
	}
	tok, err := r.cfg.Credentials.token(ctx, CapabilityRead, r.cfg.Repository)
	if err != nil {
		return err
	}
	_, err = r.git(ctx, filepath.Dir(dest), tok, "clone", "--depth", "1", "--single-branch", "--branch", r.cfg.Branch, r.cloneURL(), filepath.Base(dest))
	return err
}

func (r *remote) commitAndPush(ctx context.Context, clone, head, title, request string) error {
	if _, err := r.git(ctx, clone, "", "checkout", "-B", head); err != nil {
		return err
	}
	dir := path.Clean(filepath.ToSlash(r.cfg.Directory))
	if _, err := r.git(ctx, clone, "", "add", "-A", "--", dir); err != nil {
		return err
	}
	staged, err := r.git(ctx, clone, "", "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	if strings.TrimSpace(staged) != "" {
		msg := prTitle(title, request) + "\n\nProposed by the agent's " + RemoteTool + " tool.\n\nRequest:\n" + request
		if _, err := r.git(ctx, clone, "", "-c", "user.name=BONNIE", "-c", "user.email=bonnie@users.noreply.github.com", "commit", "-q", "-m", msg); err != nil {
			return err
		}
	}
	tok, err := r.cfg.Credentials.token(ctx, CapabilityWrite, r.cfg.Repository)
	if err != nil {
		return err
	}
	// Force: the branch is this call's own, and a repeat pushes the same
	// change again.
	_, err = r.git(ctx, clone, tok, "push", "--force", "-q", r.cloneURL(), "HEAD:refs/heads/"+head)
	return err
}

// git runs one git command. A token goes to git as an HTTP header in the
// process environment, so it is not in the arguments and not in a file.
func (r *remote) git(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
		)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if token != "" {
			text = strings.ReplaceAll(text, token, "[token]")
		}
		return "", fmt.Errorf("bonnie: selfmod: git %s: %w: %s", args[0], err, text)
	}
	return string(out), nil
}

func prTitle(title, request string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	line, _, _ := strings.Cut(strings.TrimSpace(request), "\n")
	if len(line) > 72 {
		line = line[:69] + "..."
	}
	return "Agent change: " + line
}

// pullRequest returns the URL of the open pull request from head, and opens
// a draft one when there is none.
func (r *remote) pullRequest(ctx context.Context, head, title, request string, st *state) (string, error) {
	tok, err := r.cfg.Credentials.token(ctx, CapabilityWrite, r.cfg.Repository)
	if err != nil {
		return "", err
	}
	q := url.Values{"head": {r.owner + ":" + head}, "state": {"open"}}
	var existing []struct {
		HTMLURL string `json:"html_url"`
	}
	if err := r.api(ctx, tok, http.MethodGet, "/repos/"+r.owner+"/"+r.repo+"/pulls?"+q.Encode(), nil, &existing); err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return existing[0].HTMLURL, nil
	}
	body := fmt.Sprintf("Proposed by the agent's `%s` tool in run `%s`.\n\n## Request\n\n%s\n\n## Summary\n\n%s\n",
		RemoteTool, r.scope.RunID, strings.TrimSpace(request), st.Summary)
	if st.Unverified {
		body += "\n> The change was not built before it was proposed: the agent's host has no go toolchain.\n"
	}
	body += "\n<details><summary>Diff</summary>\n\n```diff\n" + st.Diff + "```\n\n</details>\n"
	var created struct {
		HTMLURL string `json:"html_url"`
	}
	req := map[string]any{"title": prTitle(title, request), "head": head, "base": r.cfg.Branch, "body": body, "draft": true}
	if err := r.api(ctx, tok, http.MethodPost, "/repos/"+r.owner+"/"+r.repo+"/pulls", req, &created); err != nil {
		return "", err
	}
	return created.HTMLURL, nil
}

func (r *remote) api(ctx context.Context, token, method, route string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("bonnie: selfmod: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(r.cfg.APIURL, "/")+route, body)
	if err != nil {
		return fmt.Errorf("bonnie: selfmod: github request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.cfg.client.Do(req)
	if err != nil {
		return fmt.Errorf("bonnie: selfmod: github %s %s: %w", method, route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("bonnie: selfmod: github response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bonnie: selfmod: github %s %s: %s: %s", method, route, resp.Status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("bonnie: selfmod: decode github response: %w", err)
	}
	return nil
}
