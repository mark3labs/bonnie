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

// hermeticMain is a main.go for the hermetic agent tree. It serves an HTTP
// channel from a fake agent that needs no provider: a fresh run parks on
// ask_human, and a resume completes with the tree's instructions as the
// response. The instructions come from embeddedInstructions(), the copy a
// bonnie build embeds, so a test can prove they were served without the file
// on disk. The listen address is printed to stdout so a test can connect.
const hermeticMain = `package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// prompt is the tree's system prompt: the embedded copy a bonnie build ships.
var prompt = embeddedInstructions()

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
		Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: prompt}}}); err != nil {
		return nil, err
	}
	return &kit.TurnResult{Response: prompt}, nil
}

func (a *parkingAgent) InjectSteer(string) {}
func (a *parkingAgent) Close() error       { return nil }

func main() {
	journal, err := runtime.OpenFileJournal(".bonnie")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer journal.Close()

	runner := runtime.NewRunner(journal, func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		return &parkingAgent{session: s}, nil
	})
	httpCh := bonniehttp.New(runner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(ln.Addr().String())

	srv := &http.Server{Handler: httpCh.Handler(), ReadTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	// Run until stopped. A SIGTERM ends the process; the journal keeps the
	// finished steps.
	select {}
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

// buildHermeticTree scaffolds a --tools tree and installs the hermetic main.go,
// returning its root and the temp go.work that covers it with the bonnie and
// kit checkouts. It skips when no kit checkout is beside the repo.
func buildHermeticTree(t *testing.T) (root, work string) {
	t.Helper()
	kitRoot, ok := findUpstream()
	if !ok {
		t.Skip("no kit checkout beside this repo; the hermetic build cannot be simulated")
	}
	parent := t.TempDir()
	root = filepath.Join(parent, "my-agent")
	if _, err := agent.Scaffold(root, agent.InitOptions{Tools: true}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(hermeticMain), 0o644); err != nil {
		t.Fatal(err)
	}
	work = filepath.Join(parent, "go.work")
	if err := os.WriteFile(work, []byte("go 1.27.1\n\nuse (\n\t./my-agent\n\t"+bonnieRoot()+"\n\t"+kitRoot+"\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, work
}

// findUpstream looks for the kit checkout the local go.work names.
func findUpstream() (string, bool) {
	p := filepath.Join(filepath.Dir(bonnieRoot()), "kit")
	if _, err := os.Stat(filepath.Join(p, "go.mod")); err != nil {
		return "", false
	}
	return p, true
}

// bonnieRoot returns this repository's absolute path, found by climbing to
// the go.mod whose module is bonnie. The test's working directory is the
// cmd/bonnie package, two levels below the module root.
func bonnieRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(raw), "module github.com/mark3labs/bonnie") {
			return dir
		}
		if parent := filepath.Dir(dir); parent == dir {
			return filepath.Clean(filepath.Join(wd, "..", ".."))
		}
	}
}

// withGoWork returns a child environment that replaces the inherited GOWORK
// with the given file, so a subprocess build resolves the hermetic tree.
func withGoWork(work string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GOWORK=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOWORK="+work)
}

// waitForChild reads the first listen address from the child log, retrying
// until the child prints it or the timeout elapses.
func waitForChild(t *testing.T, log *syncBuffer) string {
	t.Helper()
	re := regexp.MustCompile(`127\.0\.0\.1:\d+`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindString(log.String()); m != "" {
			return m
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child never printed a listen address; log:\n%s", log.String())
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
	root, work := buildHermeticTree(t)

	// The tree's instructions are what the binary must embed.
	wantPrompt, err := os.ReadFile(filepath.Join(root, "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}

	if err := runBuild(root, buildOpts{output: filepath.Join(root, "agent"), env: withGoWork(work)}); err != nil {
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

	addr := waitForAddr(t, &log)

	// A full run: start (parks), then answer it — the response is the
	// embedded instructions.
	started := postRun(t, addr, "/runs", map[string]any{"text": "deploy the app"})
	if started.State != string(runtime.RunWaiting) {
		t.Fatalf("start state = %q, want %q", started.State, runtime.RunWaiting)
	}
	done := postRun(t, addr, "/runs/"+started.RunID+"/respond", map[string]any{
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
	root, work := buildHermeticTree(t)

	var log syncBuffer
	d := newDevServer(root, devOpts{shutdown: 10 * time.Second})
	d.env = withGoWork(work)
	d.log = &log
	if err := d.restart(); err != nil {
		t.Fatalf("first restart: %v", err)
	}
	defer d.stopChild()

	addrA := waitForChild(t, &log)

	started := postRun(t, addrA, "/runs", map[string]any{"text": "deploy the app"})
	if started.State != string(runtime.RunWaiting) {
		t.Fatalf("start state = %q, want %q", started.State, runtime.RunWaiting)
	}

	// The dev loop restarts: a new process, same journal.
	if err := d.restart(); err != nil {
		t.Fatalf("restart after park: %v", err)
	}
	addrB := waitForChildAddr(t, &log, addrA)
	if addrB == "" {
		t.Fatal("restarted child never printed a new address")
	}

	// The respond goes to the new child and completes the parked run.
	done := postRun(t, addrB, "/runs/"+started.RunID+"/respond", map[string]any{
		"responses": []map[string]any{{"text": "eu-west-1"}},
	})
	if done.State != string(runtime.RunCompleted) {
		t.Fatalf("done state = %q, want %q", done.State, runtime.RunCompleted)
	}
}

// waitForChildAddr waits until the log holds an address different from the
// previous one — the restarted child's listen address.
func waitForChildAddr(t *testing.T, log *syncBuffer, previous string) string {
	t.Helper()
	re := regexp.MustCompile(`127\.0\.0\.1:\d+`)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, addr := range re.FindAllString(log.String(), -1) {
			if addr != previous {
				return addr
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// waitForAddr waits for any listen address in the log.
func waitForAddr(t *testing.T, log *syncBuffer) string {
	t.Helper()
	return waitForChildAddr(t, log, "\x00\x00")
}

// TestRunBuildDryRun prints the discovery plan without writing or building.
func TestRunBuildDryRun(t *testing.T) {
	root, _ := buildHermeticTree(t)
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
