package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/internal/treetest"
	"github.com/mark3labs/bonnie/runtime"
)

// syncBuffer is an io.Writer safe for concurrent writes and reads. The child
// process writes to it from an exec copy goroutine while the test reads it,
// so a plain bytes.Buffer would race.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// hermeticMain is a main.go for the hermetic agent tree. It is the scaffolded
// one-call shape with a single option: an agent factory that needs no
// provider. A fresh run parks on ask_human, and a resume completes with the
// tree's instructions as the response — taken from bonnie.Registered(), the
// copy a bonnie build embeds, so a test can prove they were served without the
// file on disk.
//
// Everything else — the journal, the HTTP channel, the listen address, the
// graceful stop — comes from bonnie.Serve, which is the point: this is the real
// serving path, with only the model replaced.
const hermeticMain = `package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// prompt is the tree's system prompt: the embedded copy a bonnie build ships.
// It is read inside the turn, not into a package-level variable, because the
// generated file registers from init and Go initialises package variables
// first.
func prompt() string { return bonnie.Registered().Instructions }

type parkingAgent struct{ session *runtime.Session }

func (a *parkingAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	if a.session == nil {
		return nil, fmt.Errorf("no session")
	}
	if len(a.session.GetMessages()) == 0 {
		// A fresh start: journal the input and park on ask_human.
		if _, err := a.session.AppendMessage(kit.NewLLMUserMessage(msg)); err != nil {
			return nil, err
		}
		if _, err := a.session.AppendMessage(kit.LLMMessage{Role: kit.LLMMessageRole("assistant"),
			Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "parked"}}}); err != nil {
			return nil, err
		}
		return &kit.TurnResult{
			Response:     "parked",
			FinalValue:   runtime.SuspendRequest{Kind: runtime.SuspendQuestion, Prompt: "which region?"},
			HaltedByTool: "ask_human",
		}, nil
	}
	// A resume: journal the answer and complete with the embedded prompt.
	if _, err := a.session.AppendMessage(kit.NewLLMUserMessage(msg)); err != nil {
		return nil, err
	}
	if _, err := a.session.AppendMessage(kit.LLMMessage{Role: kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: prompt()}}}); err != nil {
		return nil, err
	}
	return &kit.TurnResult{Response: prompt()}, nil
}

func (a *parkingAgent) InjectSteer(string) {}
func (a *parkingAgent) Close() error       { return nil }

func main() {
	bonnie.New(
		bonnie.WithAddr("127.0.0.1:0"),
		bonnie.WithAgentFactory(func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
			return &parkingAgent{session: s}, nil
		}),
	).Serve()
}
`

// httpRun is the subset of the wire run response the tests decode.
type httpRun struct {
	RunID    string `json:"run_id"`
	State    string `json:"state"`
	Response string `json:"response"`
	Suspend  *struct {
		Prompt string `json:"prompt"`
	} `json:"suspend"`
}

// buildHermeticTree scaffolds a --tools tree, installs the hermetic main.go,
// and points its go.mod at this checkout so a subprocess build compiles the
// code under test. It returns the tree's root.
func buildHermeticTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "my-agent")
	if _, err := agent.Scaffold(root, agent.InitOptions{Tools: true}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(hermeticMain), 0o644); err != nil {
		t.Fatal(err)
	}
	treetest.LinkToCheckout(t, root)
	return root
}

// waitForStart waits until the child log holds n startup banners and returns
// the address the nth one reports.
//
// The count, not a change of address, is what marks a restart: the dev loop
// deliberately reuses the address across restarts so a connected TUI survives
// one, so two consecutive children report the same port.
func waitForStart(t *testing.T, log *syncBuffer, n int) string {
	t.Helper()
	re := regexp.MustCompile(`serving on (\S+)`)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindAllStringSubmatch(log.String(), -1); len(m) >= n {
			return m[n-1][1]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child never reported start %d; log:\n%s", n, log.String())
	return ""
}

// postRun starts a run and decodes the wire response.
func postRun(t *testing.T, addr, path string, body any) httpRun {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s -> %d: %s", path, resp.StatusCode, raw)
	}
	var out httpRun
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", path, err, raw)
	}
	return out
}

// TestBuildOutputServesEmbeddedInstructions is the graduation claim: a bonnie
// build binary serves a full run on a host with no Go and no BONNIE install,
// and the instructions come from the embedded copy, not the disk. The binary
// is run from a fresh directory with no instructions.md, so the only source of
// the prompt is the build's embed.
func TestBuildOutputServesEmbeddedInstructions(t *testing.T) {
	t.Parallel()
	root := buildHermeticTree(t)

	// The tree's instructions are what the binary must embed.
	wantPrompt, err := os.ReadFile(filepath.Join(root, "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}

	if err := runBuild(root, buildOpts{output: filepath.Join(root, "agent"), env: treetest.BuildEnv()}); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	bin := filepath.Join(root, "agent")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("build did not produce the binary: %v", err)
	}

	// Run it from a fresh directory with no agent tree beside it — no Go, no
	// BONNIE install, no instructions.md on disk.
	private := t.TempDir()
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = private
	var log syncBuffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start built binary: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	addr := waitForStart(t, &log, 1)

	// A full run: start (parks), then answer it — the response is the
	// embedded instructions.
	started := postRun(t, addr, "/bonnie/v1/runs", map[string]any{"text": "deploy the app"})
	if started.State != string(runtime.RunWaiting) {
		t.Fatalf("start state = %q, want %q", started.State, runtime.RunWaiting)
	}
	done := postRun(t, addr, "/bonnie/v1/runs/"+started.RunID+"/respond", map[string]any{
		"responses": []map[string]any{{"text": "eu-west-1"}},
	})
	if done.State != string(runtime.RunCompleted) {
		t.Fatalf("done state = %q, want %q", done.State, runtime.RunCompleted)
	}
	if done.Response != string(wantPrompt) {
		t.Fatalf("response = %q, want the embedded instructions %q", done.Response, wantPrompt)
	}
}

// TestDevRestartCompletesParkedRun is the dev-loop durability claim: a run
// parks in a child, the dev loop restarts it, and the respond in the new child
// completes it — crossing a process boundary with only the journal shared.
func TestDevRestartCompletesParkedRun(t *testing.T) {
	t.Parallel()
	root := buildHermeticTree(t)

	var log syncBuffer
	d := newDevServer(root, devOpts{shutdown: 10 * time.Second})
	d.env = treetest.BuildEnv()
	d.log = &log
	if err := d.restart(); err != nil {
		t.Fatalf("first restart: %v", err)
	}
	defer d.stopChild()

	addrA := waitForStart(t, &log, 1)

	started := postRun(t, addrA, "/bonnie/v1/runs", map[string]any{"text": "deploy the app"})
	if started.State != string(runtime.RunWaiting) {
		t.Fatalf("start state = %q, want %q", started.State, runtime.RunWaiting)
	}

	// The dev loop restarts: a new process, same journal, same address.
	if err := d.restart(); err != nil {
		t.Fatalf("restart after park: %v", err)
	}
	addrB := waitForStart(t, &log, 2)

	// The respond goes to the new child and completes the parked run.
	done := postRun(t, addrB, "/bonnie/v1/runs/"+started.RunID+"/respond", map[string]any{
		"responses": []map[string]any{{"text": "eu-west-1"}},
	})
	if done.State != string(runtime.RunCompleted) {
		t.Fatalf("done state = %q, want %q", done.State, runtime.RunCompleted)
	}
	if addrB != addrA {
		t.Fatalf("the restarted child moved from %s to %s; a connected client would be dropped", addrA, addrB)
	}
}

// TestRunBuildDryRun prints the discovery plan without writing or building.
func TestRunBuildDryRun(t *testing.T) {
	t.Parallel()
	root := buildHermeticTree(t)
	// A dry run does not require the module to resolve; it only discovers.

	// A dry run must not write. Delete the scaffold's generated stub first, so
	// the assertion is that the dry run wrote nothing.
	gen := filepath.Join(root, "bonnie_gen.go")
	if err := os.Remove(gen); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error {
		return runBuild(root, buildOpts{dryRun: true})
	})
	for _, want := range []string{"module: my-agent", "tools:", "echo", "instructions.md", "output: bonnie_gen.go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run does not contain %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(gen); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote bonnie_gen.go")
	}
}

// captureStdout runs fn and returns the stdout it wrote plus any error.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = fn()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	return string(out)
}
