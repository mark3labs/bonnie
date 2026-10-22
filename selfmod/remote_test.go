package selfmod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/channel"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newOrigin makes a bare repository whose main branch holds an agent tree
// under agent/.
func newOrigin(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	tree := newTree(t)
	src := t.TempDir()
	gitRun(t, src, "init", "-q")
	if err := os.CopyFS(filepath.Join(src, "agent"), os.DirFS(tree)); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", "-A")
	gitRun(t, src, "commit", "-q", "-m", "agent")
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, filepath.Dir(origin), "clone", "-q", "--bare", src, origin)
	return origin
}

// fakeGitHub records the pull requests opened through it.
type fakeGitHub struct {
	mu      sync.Mutex
	created []map[string]any
	auth    []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if r.URL.Path != "/repos/acme/agents/pulls" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		var open []map[string]string
		for _, pr := range f.created {
			if "acme:"+pr["head"].(string) == r.URL.Query().Get("head") {
				open = append(open, map[string]string{"html_url": "https://github.com/acme/agents/pull/1"})
			}
		}
		_ = json.NewEncoder(w).Encode(open)
	case http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/acme/agents/pull/1"})
	}
}

func remoteTestConfig(t *testing.T, s *scripted, origin, api string) RemoteConfig {
	cfg := RemoteConfig{
		Config:     testConfig(s),
		Repository: "github.com/acme/agents",
		Directory:  "agent",
		Branch:     "main",
		Credentials: Credentials{Resolve: func(_ context.Context, req CredentialRequest) (string, error) {
			if req.Repository != "github.com/acme/agents" {
				t.Errorf("credential for %q", req.Repository)
			}
			return "secret-token", nil
		}},
		WorkDir:  t.TempDir(),
		APIURL:   api,
		cloneURL: origin,
		client:   http.DefaultClient,
	}
	cfg.Authorize = func(_ context.Context, c Caller) (bool, error) {
		return c.Principal != nil && c.Principal.ID == "release-bot", nil
	}
	return cfg
}

func remoteScope(principal *channel.Principal) bonnie.ToolScope {
	return bonnie.ToolScope{
		RunID:  "run-remote",
		Caller: func(context.Context) (*channel.Principal, error) { return principal, nil },
	}
}

func noEnv(string) string { return "" }

// The remote tool is hidden from a caller the authorizer refuses, never
// offered under `bonnie dev`, and refuses to start without an authorizer.
func TestRemoteAuthorization(t *testing.T) {
	t.Parallel()
	cfg := remoteTestConfig(t, &scripted{t: t}, "unused", "unused")
	ctx := context.Background()

	if tools, err := remoteFactory(cfg, noEnv)(ctx, remoteScope(nil)); err != nil || len(tools) != 0 {
		t.Fatalf("anonymous caller: %d tools, %v", len(tools), err)
	}
	if tools, err := remoteFactory(cfg, noEnv)(ctx, remoteScope(&channel.Principal{ID: "someone"})); err != nil || len(tools) != 0 {
		t.Fatalf("other caller: %d tools, %v", len(tools), err)
	}
	bot := remoteScope(&channel.Principal{ID: "release-bot"})
	if tools, err := remoteFactory(cfg, noEnv)(ctx, bot); err != nil || len(tools) != 1 || tools[0].Info().Name != RemoteTool {
		t.Fatalf("authorized caller: %d tools, %v", len(tools), err)
	}
	dev := func(k string) string {
		if k == bonnie.DevRootEnv {
			return "/tree"
		}
		return ""
	}
	if tools, err := remoteFactory(cfg, dev)(ctx, bot); err != nil || len(tools) != 0 {
		t.Fatalf("under dev: %d tools, %v", len(tools), err)
	}
	for name, mutate := range map[string]func(*RemoteConfig){
		"no authorizer":  func(c *RemoteConfig) { c.Authorize = nil },
		"no credentials": func(c *RemoteConfig) { c.Credentials = Credentials{} },
		"no branch":      func(c *RemoteConfig) { c.Branch = "" },
		"bad repository": func(c *RemoteConfig) { c.Repository = "acme/agents" },
		"escaping dir":   func(c *RemoteConfig) { c.Directory = "../other" },
	} {
		bad := cfg
		mutate(&bad)
		if _, err := remoteFactory(bad, noEnv)(ctx, bot); err == nil {
			t.Errorf("%s: want a configuration error", name)
		}
	}
}

// A proposal pushes its own branch and opens one draft pull request against
// the target branch. The token never reaches a file, and a repeated call
// returns the first result without a second pull request.
func TestRemoteOpensADraftPullRequest(t *testing.T) {
	t.Parallel()
	origin := newOrigin(t)
	gh := &fakeGitHub{}
	api := httptest.NewServer(gh)
	t.Cleanup(api.Close)
	s := &scripted{t: t, steps: []func(*testing.T, kit.SubagentConfig) string{
		func(t *testing.T, cfg kit.SubagentConfig) string {
			listing := call(t, cfg.Tools, "list_files", map[string]string{})
			if strings.Contains(listing, ".git") {
				t.Errorf("the subagent can see version control:\n%s", listing)
			}
			writeFile(t, cfg, "instructions.md", "Answer in one sentence.\n")
			return "Made the agent terse."
		},
	}}
	cfg := remoteTestConfig(t, s, origin, api.URL)
	tools, err := remoteFactory(cfg, noEnv)(context.Background(), remoteScope(&channel.Principal{ID: "release-bot"}))
	if err != nil || len(tools) != 1 {
		t.Fatalf("factory: %d tools, %v", len(tools), err)
	}
	args := map[string]string{"request": "answer in one sentence", "title": "Terse answers"}
	res := runTool(t, tools[0], "call-r1", args)
	if res.IsError {
		t.Fatalf("propose failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "https://github.com/acme/agents/pull/1") || !strings.Contains(res.Content, "Nothing changed in the running agent") {
		t.Fatalf("result:\n%s", res.Content)
	}

	if len(gh.created) != 1 {
		t.Fatalf("pull requests opened: %d, want 1", len(gh.created))
	}
	pr := gh.created[0]
	if pr["draft"] != true || pr["base"] != "main" || pr["title"] != "Terse answers" {
		t.Errorf("pull request: %v", pr)
	}
	head := pr["head"].(string)
	if !strings.HasPrefix(head, "bonnie/self-modification-") {
		t.Errorf("head branch %q", head)
	}
	for _, a := range gh.auth {
		if a != "Bearer secret-token" {
			t.Errorf("API authorization %q", a)
		}
	}
	got := gitRun(t, origin, "show", head+":agent/instructions.md")
	if got != "Answer in one sentence.\n" {
		t.Errorf("pushed instructions.md = %q", got)
	}
	if main := gitRun(t, origin, "show", "main:agent/instructions.md"); main == got {
		t.Errorf("the target branch changed")
	}

	_ = filepath.WalkDir(cfg.WorkDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), "secret-token") {
			t.Errorf("token written to %s", p)
		}
		return nil
	})

	again := runTool(t, tools[0], "call-r1", args)
	if again.Content != res.Content || len(gh.created) != 1 || s.calls() != 1 {
		t.Errorf("repeat: same=%v prs=%d subagent=%d", again.Content == res.Content, len(gh.created), s.calls())
	}
}
