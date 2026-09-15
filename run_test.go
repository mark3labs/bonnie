package bonnie

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel/discord"
	"github.com/mark3labs/bonnie/channel/github"
	"github.com/mark3labs/bonnie/channel/slack"
	"github.com/mark3labs/bonnie/channel/telegram"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// resolve builds an agent and returns its configuration, so a test can read
// what the options resolved to without serving.
func resolve(opts ...Option) *config { return New(opts...).cfg }

// TestDefaultsAreTheScaffoldedLayout is the claim that replaced the manifest:
// an agent with no configuration at all reads its data from the paths `bonnie
// init` writes. If these drift from the scaffold, a fresh tree stops working
// with an empty main.go and nothing says why.
func TestDefaultsAreTheScaffoldedLayout(t *testing.T) {
	t.Parallel()
	c := resolve()
	cases := []struct{ got, want, what string }{
		{c.instrPath, "instructions.md", "instructions"},
		{c.workspace, "workspace", "workspace"},
		{c.journal, ".bonnie", "journal"},
		{c.addr, ":8080", "address"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("default %s = %q, want %q", tc.what, tc.got, tc.want)
		}
	}
	if c.sandbox != nil {
		t.Error("the default must be no sandbox, loudly warned about, not a silent one")
	}
}

// TestWorkspaceIsTheAgentRoot: the agent's files live in the workspace, so a
// model's write cannot land on the instructions, the journal, or the source
// beside them. The default is the directory bonnie init scaffolds.
func TestWorkspaceIsTheAgentRoot(t *testing.T) {
	t.Parallel()

	t.Run("default is absolute and inside the tree", func(t *testing.T) {
		t.Parallel()
		dir, err := resolve().workspaceDir()
		if err != nil {
			t.Fatalf("workspaceDir: %v", err)
		}
		if !filepath.IsAbs(dir) {
			t.Fatalf("workspace %q is not absolute", dir)
		}
		if filepath.Base(dir) != DefaultWorkspace {
			t.Fatalf("workspace = %q, want a %s directory", dir, DefaultWorkspace)
		}
		// It is never the tree root itself: a write there would land on the
		// instructions and the journal.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if dir == wd {
			t.Fatal("the workspace is the tree root itself: a write would land on the agent's own files")
		}
	})

	// A host with no tree asks for no workspace, and its own directory stays
	// the root — what `bonnie serve` does.
	t.Run("no tree", func(t *testing.T) {
		t.Parallel()
		dir, err := resolve(WithWorkspace("")).workspaceDir()
		if err != nil {
			t.Fatalf("workspaceDir: %v", err)
		}
		if dir != "" {
			t.Fatalf("workspace = %q, want empty without a tree", dir)
		}
	})
}

// TestSandboxedWorkspaceBecomesASeed: with a sandbox, the workspace is not a
// working directory but a seed mirrored into [sandbox.Workspace]. Accepting
// the setting and ignoring it is what invariant 13 forbids.
func TestSandboxedWorkspaceBecomesASeed(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "sandbox.Seeded(provider, workspace)") {
		t.Fatal("the sandboxed branch does not seed the workspace: " +
			"the setting would be accepted and ignored")
	}
}

// TestNoHostToolsReachTheAgent is a security guard, not a style check.
//
// It replaces TestSandboxedAgentGetsNoHostTools, which read run.go for the
// placement of hostWorkspaceOptions. That function is gone: every run is
// sandboxed now, so there are no host tools to misplace. The property it
// protected is not gone, though, and is worth more than before — Kit honours
// Options.Tools even when DisableCoreTools is set, and [sandbox.Agent]
// applies a caller's options AFTER its own, so any route that reintroduces
// kit.WithTools(kit.AllTools(...)) hands the model a real host shell inside a
// sandbox.
//
// The check is the absence of that shape in the source. A guard that matches
// nothing would be worse than none, so it also asserts the sandbox call it
// depends on is still there.
//
// Comments are stripped before the check. The code that explains why these
// shapes are forbidden has to be free to name them — a guard that fires on
// its own tombstone comment teaches the next person to delete the comment.
func TestNoHostToolsReachTheAgent(t *testing.T) {
	t.Parallel()
	body := sourceWithoutComments(t, "run.go")

	if strings.Contains(body, "kit.AllTools(") {
		t.Fatal("run.go builds Kit's host core tools again: " +
			"with sandbox.Agent applying caller options last, those tools would " +
			"give the model a host shell inside the sandbox")
	}
	if strings.Contains(body, "kit.WithWorkDir(") {
		t.Fatal("run.go roots host file tools at a working directory again: " +
			"WithWorkDir is a base and not a jail, which is the defect issue #1 fixed")
	}
	if strings.Contains(body, "runtime.KitAgent(") {
		t.Fatal("run.go builds a bare Kit agent again: that is the unsandboxed " +
			"path this change removed")
	}
	if !strings.Contains(body, "sandbox.Agent(provider") {
		t.Fatal("run.go no longer wraps the agent in a sandbox: this guard reads " +
			"a shape that is gone, so it is no longer guarding anything")
	}
}

// sourceWithoutComments returns a Go file's code with every comment removed,
// so a guard that greps for a forbidden call cannot be tripped by prose that
// mentions it.
func sourceWithoutComments(t *testing.T, path string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0) // 0: drop comments
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, file); err != nil {
		t.Fatalf("print %s: %v", path, err)
	}
	return buf.String()
}

// TestEverySandboxedAgentIsSandboxed is the positive half: whatever the
// configuration, agentFactory must not produce an agent whose tools are this
// process. The factory is opaque, so the assertion is that a configuration
// with no sandbox still resolves a real provider.
func TestEverySandboxedAgentIsSandboxed(t *testing.T) {
	t.Parallel()
	p := resolve().defaultSandbox()
	if p == nil {
		t.Fatal("a run with no WithSandbox resolved no provider: tool calls would run on the host")
	}
	if p.Name() == "local" {
		t.Fatal("the default backend is local, which provides no isolation: " +
			"that is the no-sandbox default under another name")
	}
	if p.Name() != "landlock" {
		t.Fatalf("default backend = %q, want landlock", p.Name())
	}
}

// TestSandboxIsTheDefault is the inversion of TestNoSandboxIsTheDefault,
// which documented the old behaviour and is deliberately gone. A default
// bonnie.New() must build a working, confined agent factory.
func TestSandboxIsTheDefault(t *testing.T) {
	t.Parallel()
	if err := sandbox.Landlock().Available(context.Background()); err != nil {
		t.Skipf("landlock unavailable: %v", err)
	}
	f, err := resolve().agentFactory(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("agentFactory: %v", err)
	}
	if f == nil {
		t.Fatal("no factory returned")
	}
}

// TestDenyNetworkOnTheDefaultSandboxIsRejected stops a false sense of safety.
//
// It used to assert that a network policy with NO sandbox fails. There is no
// such configuration now, so the honest version of the same rule is one level
// down: the default backend confines the filesystem and not the network, so a
// policy it cannot enforce must be refused and must name a backend that can
// (docs/SPEC.md §8, invariant 10).
func TestDenyNetworkOnTheDefaultSandboxIsRejected(t *testing.T) {
	t.Parallel()
	c := resolve(WithNetwork(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}))
	_, err := c.agentFactory(context.Background(), "", nil)
	if err == nil {
		t.Fatal("want an error for a network policy the default backend cannot enforce")
	}
	if !strings.Contains(err.Error(), "cannot control the network") {
		t.Fatalf("error does not say what is wrong: %v", err)
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Fatalf("error does not name a backend that can enforce it: %v", err)
	}
}

// TestDenyNetworkOnLocalIsRejected is the same honesty rule one level down: a
// backend that cannot enforce the policy must refuse it. Local cannot control
// egress at all, so it does not implement [sandbox.Networked].
func TestDenyNetworkOnLocalIsRejected(t *testing.T) {
	t.Parallel()
	c := resolve(
		WithSandbox(sandbox.Local()),
		WithNetwork(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}),
	)
	if _, err := c.agentFactory(context.Background(), "", nil); err == nil {
		t.Fatal("want an error: the local sandbox cannot control the network")
	}
}

// TestUnavailableSandboxFailsAtStartup is why Available exists. An operator
// must learn that the backend is down when the server starts, not on the first
// tool call an hour later.
func TestUnavailableSandboxFailsAtStartup(t *testing.T) {
	t.Parallel()
	c := resolve(WithSandbox(sandbox.Microsandbox()))
	_, err := c.agentFactory(context.Background(), "", nil)
	if err == nil {
		t.Skip("msb is installed here, so this path cannot be exercised")
	}
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// A host-supplied agent factory owns the agent, so an option that configures
// the agent BONNIE would have built cannot apply. Refusing names both; the
// alternative is a model setting that silently does nothing.
func TestAgentFactoryRefusesConflictingOptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opt  Option
	}{
		{"WithModel", WithModel("anthropic/claude-sonnet-4-5")},
		{"WithSystemPrompt", WithSystemPrompt("be terse")},
		{"WithSandbox", WithSandbox(sandbox.Local())},
		{"WithTools", WithTools(kit.NewTool("x", "x", func(context.Context, struct{}) (kit.ToolOutput, error) {
			return kit.TextResult(""), nil
		}))},
		{"WithKit", WithKit(kit.WithModel("anthropic/claude-sonnet-4-5"))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := resolve(WithAgentFactory(stubFactory), c.opt)
			_, err := cfg.agentFactory(context.Background(), "", nil)
			if err == nil {
				t.Fatalf("%s was accepted beside a host factory; it would be ignored", c.name)
			}
			if !strings.Contains(err.Error(), c.name) {
				t.Fatalf("the refusal does not name %s: %v", c.name, err)
			}
		})
	}

	// Alone, the factory is honoured.
	cfg := resolve(WithAgentFactory(stubFactory))
	if _, err := cfg.agentFactory(context.Background(), "", nil); err != nil {
		t.Fatalf("a factory on its own was refused: %v", err)
	}
}

// TestSystemPromptFallsBackToTheEmbeddedCopy is what makes a built binary
// serve on a bare host: there is no instructions.md beside it, so the copy
// codegen embedded is the prompt.
func TestSystemPromptFallsBackToTheEmbeddedCopy(t *testing.T) {
	t.Parallel()

	t.Run("disk wins", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "instructions.md")
		if err := os.WriteFile(path, []byte("from disk"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resolve(WithInstructions(path)).systemPrompt()
		if err != nil {
			t.Fatalf("systemPrompt: %v", err)
		}
		if got != "from disk" {
			t.Fatalf("prompt = %q, want the file on disk", got)
		}
	})

	t.Run("embedded fallback", func(t *testing.T) {
		// Register is process-wide, so this subtest is not parallel and puts
		// the registry back when it is done.
		before := Registered()
		t.Cleanup(func() { Register(before) })
		Register(Tree{Instructions: "from the embed"})

		got, err := resolve(WithInstructions(filepath.Join(t.TempDir(), "absent.md"))).systemPrompt()
		if err != nil {
			t.Fatalf("systemPrompt: %v", err)
		}
		if got != "from the embed" {
			t.Fatalf("prompt = %q, want the embedded copy", got)
		}
	})

	t.Run("no file and no embed is refused", func(t *testing.T) {
		before := Registered()
		t.Cleanup(func() { Register(before) })
		Register(Tree{})

		if _, err := resolve(WithInstructions(filepath.Join(t.TempDir(), "absent.md"))).systemPrompt(); err == nil {
			t.Fatal("a tree with no instructions was accepted: the agent would run with no prompt and no warning")
		}
	})

	t.Run("a host with no tree needs none", func(t *testing.T) {
		t.Parallel()
		got, err := resolve(WithInstructions("")).systemPrompt()
		if err != nil || got != "" {
			t.Fatalf("systemPrompt = %q, %v; a host with no tree must not be refused", got, err)
		}
	})
}

// A channel's credentials come from the environment, and a missing one is a
// startup error naming the variable. A webhook that does not verify its caller
// is a door with no lock, so it must never mount quietly.
func TestChannelNeedsItsSecrets(t *testing.T) {
	t.Parallel()
	if err := require("slack", named{"SLACK_BOT_TOKEN", ""}); err == nil {
		t.Fatal("a channel mounted without its verification secret")
	} else if !strings.Contains(err.Error(), "SLACK_BOT_TOKEN") {
		t.Fatalf("the error does not name the variable: %v", err)
	}
	if err := require("slack", named{"SLACK_BOT_TOKEN", "xoxb-1"}); err != nil {
		t.Fatalf("a complete credential set was refused: %v", err)
	}
}

// TestChatChannelOptionsMount drives the option path end to end: with the
// platform's variables set, each option builds a channel and contributes its
// webhook route; with one missing, it refuses and names the variable.
//
// The refusal is what matters. [require] is tested directly above, but only
// this test proves each option is actually wired to it — an option that built
// its channel without checking would mount an unverified webhook, which is the
// failure docs/CHANNELS.md exists to prevent.
func TestChatChannelOptionsMount(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		opt   Option
		route string
	}{
		{
			name:  "slack",
			env:   map[string]string{"SLACK_BOT_TOKEN": "xoxb-1", "SLACK_SIGNING_SECRET": "s3cret"},
			opt:   WithSlack(slack.Config{}),
			route: slack.DefaultPath,
		},
		{
			name: "discord",
			// Discord's public key is 32 bytes of hex and is parsed at mount,
			// so the fixture has to be well formed.
			env: map[string]string{
				"DISCORD_BOT_TOKEN":  "tok",
				"DISCORD_PUBLIC_KEY": strings.Repeat("ab", 32),
			},
			opt:   WithDiscord(discord.Config{}),
			route: discord.DefaultPath,
		},
		{
			name:  "telegram",
			env:   map[string]string{"TELEGRAM_BOT_TOKEN": "tok", "TELEGRAM_WEBHOOK_SECRET": "s3cret"},
			opt:   WithTelegram(telegram.Config{Username: "mybot"}),
			route: telegram.DefaultPath,
		},
		{
			name: "github",
			// The GitHub App's private key must parse at mount, so the
			// fixture generates a real one. It is a test-only key; the fake
			// API never verifies it.
			env: map[string]string{
				"GITHUB_APP_ID":          "1234",
				"GITHUB_APP_PRIVATE_KEY": testRSAPrivateKey(t),
				"GITHUB_WEBHOOK_SECRET":  "s3cret",
			},
			opt:   WithGitHub(github.Config{BotName: "my-agent"}),
			route: github.DefaultPath,
		},
	}

	runner := runtime.NewRunner(runtime.NewMemoryJournal(), stubFactory)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Setenv forbids t.Parallel, which is why this test is serial.
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			build := resolve(c.opt).channels
			if len(build) != 1 {
				t.Fatalf("the option added %d channels, want 1", len(build))
			}
			ch, err := build[0](runner)
			if err != nil {
				t.Fatalf("the channel did not mount with its credentials set: %v", err)
			}
			var paths []string
			for _, rt := range ch.Routes() {
				paths = append(paths, rt.Path)
			}
			if !slices.Contains(paths, c.route) {
				t.Fatalf("%s mounted %v, want its webhook at %s", c.name, paths, c.route)
			}

			// Drop one credential: the same option must now refuse, naming it.
			for k := range c.env {
				t.Setenv(k, "")
				if _, err := resolve(c.opt).channels[0](runner); err == nil {
					t.Fatalf("%s mounted with %s empty: an unverified webhook", c.name, k)
				} else if !strings.Contains(err.Error(), k) {
					t.Fatalf("the refusal does not name %s: %v", k, err)
				}
				t.Setenv(k, c.env[k])
			}
		})
	}
}

// testRSAPrivateKey generates a throwaway RSA key for the mount test.
func testRSAPrivateKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// The workspace seed never overwrites. A file already in the workspace is
// either the author's seed from an earlier start or the model's own work, and
// both outrank a copy compiled in months ago.
func TestSeedNeverOverwrites(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	kept := filepath.Join(dest, "seed.txt")
	if err := os.WriteFile(kept, []byte("the model wrote this"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := seedFromEmbed(testSeed, dest); err != nil {
		t.Fatalf("seedFromEmbed: %v", err)
	}
	b, err := os.ReadFile(kept)
	if err != nil || string(b) != "the model wrote this" {
		t.Fatalf("the seed overwrote existing work: %q, %v", b, err)
	}
	// A file that was not there is written, with the embed's own directory
	// stripped from the path.
	if _, err := os.Stat(filepath.Join(dest, "fresh.txt")); err != nil {
		t.Fatalf("the seed did not materialise a missing file: %v", err)
	}
}

func TestCloseStreamsOnShutdown(t *testing.T) {
	t.Parallel()
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	ended := make(chan struct{}, 1)
	h := closeStreamsOnShutdown(shutdownCtx, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
		ended <- struct{}{}
	}))

	req := httptest.NewRequest(http.MethodGet, "/bonnie/v1/runs/run-1/stream", nil)
	go h.ServeHTTP(httptest.NewRecorder(), req)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream handler did not start")
	}
	shutdown()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("stream handler did not stop on shutdown")
	}
}

func TestCloseStreamsDoesNotCancelTurn(t *testing.T) {
	t.Parallel()
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	observed := make(chan error, 1)
	h := closeStreamsOnShutdown(shutdownCtx, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed <- r.Context().Err()
	}))
	shutdown()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/bonnie/v1/runs/run-1", nil))
	if err := <-observed; err != nil {
		t.Fatalf("turn context was cancelled: %v", err)
	}
}

// TestDefaultSandboxCannotReadTheJournal is the regression test for the
// incident that opened issue #1.
//
// A live Slack agent ran `find /home/<user>/Workspace/my-agent -type f` and
// the result listed main.go, instructions.md, and .bonnie/journal.db — the
// journal that makes its own runs durable. Nothing refused it, because an
// absolute path never consults a working directory.
//
// This drives the DEFAULT configuration, not a hand-built provider: the whole
// point of the change is that a host which configures nothing is confined.
// The journal sits where a real tree puts it, and the workspaces root is the
// one bonnie.New() derives from it.
func TestDefaultSandboxCannotReadTheJournal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	journalDir := filepath.Join(root, DefaultJournal)
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(journalDir, "journal.db")
	if err := os.WriteFile(journal, []byte("SQLite format 3\x00SECRET-JOURNAL-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The tree's own source, beside the journal, is the rest of what the
	// live agent listed.
	if err := os.WriteFile(filepath.Join(root, "instructions.md"), []byte("SECRET-PROMPT"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := resolve(WithJournal(journalDir)).defaultSandbox()
	if err := provider.Available(context.Background()); err != nil {
		t.Skipf("the default backend is unavailable here: %v", err)
	}

	ctx := context.Background()
	sb, err := provider.Open(ctx, "journal-guard")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if d, ok := sb.(interface{ Delete(context.Context) error }); ok {
			_ = d.Delete(ctx)
		}
	})

	// By absolute path, by the shell, and by the traversal a model would try
	// next. "any path" is what the acceptance criterion says.
	for _, line := range []string{
		"cat " + journal,
		"cat " + filepath.Join(root, "instructions.md"),
		"find " + root + " -type f",
		"cat ../../" + DefaultJournal + "/journal.db",
		"sh -c 'cat " + journal + "'",
	} {
		res, err := sb.Exec(ctx, sandbox.Shell(line))
		if err != nil {
			t.Fatalf("Exec(%q): %v", line, err)
		}
		if strings.Contains(res.Stdout, "SECRET") {
			t.Fatalf("a default run read outside its workspace with %q: %q", line, res.Stdout)
		}
	}

	// And the file tools, which run in this process and are not covered by
	// the kernel restriction.
	if _, err := sb.ReadFile(ctx, journal); err == nil {
		t.Fatal("the read_file tool reached the journal by absolute path")
	}
}
