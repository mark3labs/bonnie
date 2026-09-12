package main

import (
	"context"
	"encoding/json"

	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// parseServe parses args through the real command so the tests exercise the
// flag wiring, and reconstructs the opts the RunE closure would see.
func parseServe(t *testing.T, args ...string) (serveOpts, *pflag.FlagSet) {
	t.Helper()
	cmd := newServeCmd()
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	f := cmd.Flags()
	get := func(name string) string { v, _ := f.GetString(name); return v }
	o := serveOpts{
		addr:        get("addr"),
		journal:     get("journal"),
		model:       get("model"),
		prompt:      get("system-prompt"),
		sandboxKind: get("sandbox"),
		sandboxImg:  get("sandbox-image"),
		denyNetwork: func() bool { v, _ := f.GetBool("sandbox-deny-network"); return v }(),
		shutdown:    30 * time.Second,
		agentDir:    get("agent"),
		configPath:  get("config"),
	}
	return o, f
}

const treeManifest = `apiVersion: bonnie.dev/v0alpha
title: deploy-bot
model: anthropic/claude-sonnet-4-5
instructions: prompts/system.md
sandbox:
  kind: docker
  network:
    mode: allow-list
    allow: ["api.example.com"]
channels:
  http:
    addr: ":9090"
`

// TestResolveServePrecedence pins the rule that decides every setting:
// flags over manifest over defaults — and the banner names the winner.
func TestResolveServePrecedence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := map[string]string{
		"agent.yaml":         treeManifest,
		"prompts/system.md":  "TREE PROMPT\n",
		"workspace/.gitkeep": "",
		"skills/.gitkeep":    "",
		"instructions.md":    "DEFAULT PROMPT\n",
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("manifest wins over defaults", func(t *testing.T) {
		t.Parallel()
		o, flags := parseServe(t, "--agent", root, "--journal", filepath.Join(t.TempDir(), "j"))
		cfg, err := resolveServe(flags, o)
		if err != nil {
			t.Fatalf("resolveServe: %v", err)
		}
		if cfg.model != "anthropic/claude-sonnet-4-5" || cfg.addr != ":9090" ||
			cfg.sandboxKind != "docker" || cfg.title != "deploy-bot" {
			t.Fatalf("manifest values did not win: %+v", cfg)
		}
		if cfg.prompt != "TREE PROMPT\n" {
			t.Fatalf("the instructions file was not read: %q", cfg.prompt)
		}
		if cfg.network == nil || cfg.network.Mode != sandbox.NetworkAllowList ||
			len(cfg.network.Allow) != 1 || cfg.network.Allow[0] != "api.example.com" {
			t.Fatalf("the manifest network policy did not apply: %+v", cfg.network)
		}
		// The banner names the manifest as the source.
		for _, want := range []string{"model anthropic/claude-sonnet-4-5 (agent.yaml)", "serving on :9090 (agent.yaml)", "instructions prompts/system.md (agent.yaml)"} {
			if !containsLine(cfg.banner, want) {
				t.Fatalf("banner misses %q:\n%s", want, strings.Join(cfg.banner, "\n"))
			}
		}
	})

	t.Run("flags win over the manifest", func(t *testing.T) {
		t.Parallel()
		o, flags := parseServe(t, "--agent", root, "--model", "opencode/kimi-k2.5",
			"--addr", ":1", "--sandbox", "microsandbox",
			"--journal", filepath.Join(t.TempDir(), "j"))
		cfg, err := resolveServe(flags, o)
		if err != nil {
			t.Fatalf("resolveServe: %v", err)
		}
		if cfg.model != "opencode/kimi-k2.5" || cfg.addr != ":1" || cfg.sandboxKind != "microsandbox" {
			t.Fatalf("flags did not win: %+v", cfg)
		}
		for _, want := range []string{"model opencode/kimi-k2.5 (flag)", "serving on :1 (flag)"} {
			if !containsLine(cfg.banner, want) {
				t.Fatalf("banner misses %q:\n%s", want, strings.Join(cfg.banner, "\n"))
			}
		}
	})

	t.Run("the deny flag wins over the manifest policy", func(t *testing.T) {
		t.Parallel()
		o, flags := parseServe(t, "--agent", root, "--sandbox-deny-network",
			"--journal", filepath.Join(t.TempDir(), "j"))
		// The deny flag with the manifest's docker sandbox is servable; the
		// flag's deny-all must win over the manifest's unset policy.
		cfg, err := resolveServe(flags, o)
		if err != nil {
			t.Fatalf("resolveServe: %v", err)
		}
		// The deny flag must beat the manifest's allow-list, not just agree
		// with it — otherwise the test could pass with the manifest value.
		if cfg.network == nil || cfg.network.Mode != sandbox.NetworkDenyAll || len(cfg.network.Allow) != 0 {
			t.Fatalf("the deny flag did not win over the manifest policy: %+v", cfg.network)
		}
	})

	t.Run("a tree with no instructions file is refused", func(t *testing.T) {
		t.Parallel()
		empty := t.TempDir()
		if err := os.WriteFile(filepath.Join(empty, "agent.yaml"), []byte("apiVersion: bonnie.dev/v0alpha\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		o, flags := parseServe(t, "--agent", empty, "--journal", filepath.Join(t.TempDir(), "j"))
		if _, err := resolveServe(flags, o); err == nil || !strings.Contains(err.Error(), "instructions") {
			t.Fatalf("want an error naming the instructions file: %v", err)
		}
	})
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// TestResolveServeNoneWarning: the manifest must not make "no sandbox"
// quieter than the flag is. kind: none prints the warning.
func TestResolveServeNoneWarning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"),
		[]byte("apiVersion: bonnie.dev/v0alpha\nsandbox:\n  kind: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
	cfg, err := resolveServe(flags, o)
	if err != nil {
		t.Fatalf("resolveServe: %v", err)
	}
	if !containsLine(cfg.banner, "WARNING no sandbox") {
		t.Fatalf("the no-sandbox warning is missing:\n%s", strings.Join(cfg.banner, "\n"))
	}
}

// TestResolveServeRefusesGoTree pins invariant 13: a tree serve cannot fully
// honor is refused with bonnie build named, never partially served.
func TestResolveServeRefusesGoTree(t *testing.T) {
	t.Parallel()

	t.Run("go.mod", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for name, body := range map[string]string{
			"go.mod":          "module x\n",
			"agent.yaml":      "apiVersion: bonnie.dev/v0alpha\n",
			"instructions.md": "x",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
		_, err := resolveServe(flags, o)
		if err == nil || !strings.Contains(err.Error(), "bonnie build") {
			t.Fatalf("err = %v, want the bonnie build refusal", err)
		}
	})

	t.Run("a tools directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "tools", "echo"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"agent.yaml":      "apiVersion: bonnie.dev/v0alpha\n",
			"instructions.md": "x",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
		_, err := resolveServe(flags, o)
		if err == nil || !strings.Contains(err.Error(), "bonnie build") {
			t.Fatalf("err = %v, want the bonnie build refusal", err)
		}
	})

	t.Run("the --tools scaffold is refused", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "my-agent")
		if _, err := agent.Scaffold(dir, agent.InitOptions{Tools: true}); err != nil {
			t.Fatalf("Scaffold: %v", err)
		}
		o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
		_, err := resolveServe(flags, o)
		if err == nil || !strings.Contains(err.Error(), "bonnie build") {
			t.Fatalf("err = %v, want the bonnie build refusal", err)
		}
	})
}

// TestResolveServeNetworkNeedsSandbox: the manifest's network policy without
// a sandbox kind is a control nothing can apply, and is refused.
func TestResolveServeNetworkNeedsSandbox(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(`apiVersion: bonnie.dev/v0alpha
sandbox:
  network:
    mode: deny-all
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
	if _, err := resolveServe(flags, o); err == nil || !strings.Contains(err.Error(), "needs a sandbox") {
		t.Fatalf("err = %v, want a needs-a-sandbox refusal", err)
	}
}

// TestServeAgentRoundTrip is the zero-Go claim, end to end and hermetically:
// scaffold a tree, serve it, and move a run over HTTP. No model credential
// is configured, so the run starts and fails at the model call — which is
// the part the runtime's own live integration covers. What this test proves
// is the path: manifest, instructions, journal, runner, channel, routes.
func TestServeAgentRoundTrip(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "my-agent")
	if _, err := agent.Scaffold(dir, agent.InitOptions{}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	journal := filepath.Join(t.TempDir(), "j")
	// The scaffold names no model, and a model without a credential retries
	// its way through the SDK's backoff before failing. A provider prefix
	// nothing knows fails at construction instead, in milliseconds, which
	// is the failure this test wants: the run must START and durably fail.
	o, flags := parseServe(t, "--agent", dir, "--journal", journal,
		"--model", "bonnie-probe/nonexistent")

	cfg, err := resolveServe(flags, o)
	if err != nil {
		t.Fatalf("resolveServe: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, cfg, time.Second, ln) }()

	base := "http://" + ln.Addr().String()
	// This test's own client, so the shared DefaultClient's keep-alive pool
	// cannot hold a connection open past the shutdown deadline.
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	waitForHTTP(t, client, base)

	// Start a run. With no usable model the turn fails, so the start is an
	// error response — but the run exists and is durable.
	resp, err := client.Post(base+"/runs", "application/json", strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST /runs = %d, want 500 (no model configured): %s", resp.StatusCode, body)
	}

	// The run is in the journal, and the served tree reports it.
	runID := onlyRunID(t, journal)
	get, err := client.Get(base + "/runs/" + runID)
	if err != nil {
		t.Fatalf("GET /runs/%s: %v", runID, err)
	}
	var run struct {
		RunID string           `json:"run_id"`
		State runtime.RunState `json:"state"`
	}
	if err := json.NewDecoder(get.Body).Decode(&run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	_ = get.Body.Close()
	if run.RunID != runID || run.State != runtime.RunFailed {
		t.Fatalf("run = %+v, want the failed run %s", run, runID)
	}

	// An unknown run is a 404 — routing and the journal agree.
	res, err := client.Get(base + "/runs/run-nope")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET an unknown run = %d, want 404", res.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveHTTP: %v", err)
	}
}

// A chat channel's manifest key enables it; the secrets come from the
// environment. A missing secret is a startup error that names the variable,
// because a webhook that does not verify its caller is a door with no lock.
func TestChatChannelNeedsItsSecrets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := "apiVersion: " + agent.APIVersion + "\nchannels:\n  telegram: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"))
	_, err := resolveServe(flags, o)
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("err = %v, want the missing-variable refusal", err)
	}
}

// The full path: the manifest enables Telegram, the secrets come from the
// environment, serve mounts the webhook, a message arrives, the run
// completes, and the reply lands on the (faked) Telegram API.
//
// Not parallel: t.Setenv forbids it, and the env vars are the wiring under
// test.
func TestServeMountsChatChannels(t *testing.T) {
	dir := t.TempDir()
	manifest := "apiVersion: " + agent.APIVersion + "\nchannels:\n  telegram: {}\nsandbox:\n  kind: none\n"
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The fake Telegram API, and the env that points the channel at it.
	var mu sync.Mutex
	var sent []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sent = append(sent, body.Text)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(fake.Close)
	t.Setenv("TELEGRAM_BOT_TOKEN", "t0ken")
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", "s3cret")
	t.Setenv("TELEGRAM_API_URL", fake.URL)

	o, flags := parseServe(t, "--agent", dir, "--journal", filepath.Join(t.TempDir(), "j"),
		"--model", "bonnie-probe/nonexistent")
	cfg, err := resolveServe(flags, o)
	if err != nil {
		t.Fatalf("resolveServe: %v", err)
	}
	if !containsLine(cfg.banner, "channel telegram /telegram (agent.yaml)") {
		t.Fatalf("the banner does not name the channel:\n%s", strings.Join(cfg.banner, "\n"))
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, cfg, time.Second, ln) }()

	base := "http://" + ln.Addr().String()
	waitForHTTP(t, http.DefaultClient, base)

	// Drive the webhook the way Telegram would.
	upd, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"text": "hello over telegram",
			"from": map[string]any{"id": 7, "username": "ada", "is_bot": false},
			"chat": map[string]any{"id": 42, "type": "private"},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, base+"/telegram", strings.NewReader(string(upd)))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook = %d", resp.StatusCode)
	}

	// The run fails at the model (no credential here); the failure is
	// delivered to the chat, which is the point: silence helps nobody.
	waitForCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) > 0
	})
	mu.Lock()
	last := sent[0]
	mu.Unlock()
	if !strings.Contains(last, "failed") {
		t.Fatalf("delivered %q, want the failure notice", last)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveHTTP: %v", err)
	}
}

// onlyRunID finds the single operator-visible run in a journal directory.
// The channel's own bookkeeping is a reserved run and never counts (SPEC §8,
// invariant 8).
func onlyRunID(t *testing.T, journal string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(journal, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if runtime.IsReservedRun(id) {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 {
		t.Fatalf("want exactly one run in the journal, got %v", ids)
	}
	return ids[0]
}

// waitForHTTP dials until the server answers, for a bounded time.
func waitForHTTP(t *testing.T, client *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/runs/health-probe")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the server never came up")
}

// waitForCond polls until check is true, with a timeout.
func waitForCond(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the condition")
}
