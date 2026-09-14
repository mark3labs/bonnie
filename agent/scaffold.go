package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// gitkeep is the empty-directory marker the scaffold writes. Version
// control tracks files, not directories, and the scaffolded skills/ and
// workspace/ start empty. The seeder skips it: it is bookkeeping, not seed
// content.
const gitkeep = ".gitkeep"

// InitOptions configures [Scaffold].
type InitOptions struct {
	// Format is the manifest format: "yaml" (the default), "toml", or
	// "json". It picks the file the scaffold writes.
	Format string

	// Title names the agent. When empty, the directory base name is used —
	// the same fallback a host applies when the manifest omits the title.
	Title string

	// Model is written into the manifest when set. When empty, the model
	// line is left as a comment and the host default applies.
	Model string

	// Tools adds a sample Go tool to the module: tools/echo/tool.go. The Go
	// module itself (go.mod, main.go, bonnie_gen.go) is always scaffolded,
	// so a fresh tree builds out of the box; --tools only adds a real tool
	// to a tree that would otherwise ship with none.
	Tools bool

	// Version is the running bonnie release version. When it is a clean
	// semver, the scaffold's go.mod pins `github.com/mark3labs/bonnie` to it
	// and to the kit version it builds against, so `go mod tidy` resolves a
	// specific release instead of whatever @latest the proxy offers. When
	// empty or a non-release (dev) build, no requires are written.
	Version string
}

// scaffoldFiles is one file the scaffold wants to write.
type scaffoldFile struct {
	path    string
	content string
	mode    os.FileMode
}

// Scaffold writes a fresh agent tree at dir.
//
// It never overwrites. Every file the scaffold wants to create is checked
// first; if any exists, Scaffold refuses naming all of them and changes
// nothing. Refusing is what makes `bonnie init .` safe to run twice.
//
// Every tree is a Go module. The manifest and instructions.md carry the data;
// main.go is the authored entry that reads them and wires the default agent;
// bonnie_gen.go is the wiring BONNIE owns (and regenerates). `go build ./...`
// passes on the fresh scaffold. --tools adds a sample tool to tools/.
func Scaffold(dir string, opts InitOptions) ([]string, error) {
	if opts.Format == "" {
		opts.Format = "yaml"
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("bonnie: agent: resolve directory: %w", err)
	}
	if opts.Title == "" {
		opts.Title = filepath.Base(abs)
	}

	manifest, err := manifestScaffold(opts)
	if err != nil {
		return nil, err
	}

	module := moduleName(opts.Title)
	files := []scaffoldFile{
		{path: manifest.name, content: manifest.body, mode: 0o644},
		{path: "instructions.md", content: instructionsTemplate(opts.Title), mode: 0o644},
		{path: "go.mod", content: goModTemplate(module, pinVersion(opts.Version)), mode: 0o644},
		{path: "main.go", content: mainTemplate(), mode: 0o644},
		{path: "bonnie_gen.go", content: genTemplate(), mode: 0o644},
		{path: filepath.Join("skills", gitkeep), content: "", mode: 0o644},
		{path: filepath.Join("workspace", gitkeep), content: "", mode: 0o644},
	}
	if opts.Tools {
		files = append(files,
			scaffoldFile{path: filepath.Join("tools", "echo", "tool.go"), content: toolTemplate(), mode: 0o644},
		)
	}

	// Pre-flight: refuse naming every blocker, before touching anything.
	var exists []string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f.path)); err == nil {
			exists = append(exists, f.path)
		}
	}
	if len(exists) > 0 {
		return nil, fmt.Errorf("bonnie: agent: refusing to scaffold: %s already exist%s; init never overwrites",
			strings.Join(quote(exists), ", "), plural(len(exists)))
	}

	for _, f := range files {
		p := filepath.Join(dir, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, fmt.Errorf("bonnie: agent: create directory: %w", err)
		}
		if err := os.WriteFile(p, []byte(f.content), f.mode); err != nil {
			return nil, fmt.Errorf("bonnie: agent: write %s: %w", f.path, err)
		}
	}

	created := make([]string, len(files))
	for i, f := range files {
		created[i] = f.path
	}
	return created, nil
}

// moduleName turns a directory name into a valid Go module path element.
// A scaffolded module is local, so any clean element will do; the sanitiser
// exists so a title like "My Agent!" still builds.
func moduleName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "agent"
	}
	return out
}

// plural returns "s" for counts above one, for messages that list files.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// manifestScaffold renders the manifest in the requested format. YAML and
// TOML carry comments, because hand-edited config wants them; JSON cannot,
// so it stays minimal and never the recommended form.
func manifestScaffold(opts InitOptions) (struct{ name, body string }, error) {
	type out = struct{ name, body string }

	model := opts.Model
	var body string
	switch strings.ToLower(opts.Format) {
	case "yaml", "yml":
		modelLine := "# model: anthropic/claude-sonnet-4-5"
		if model != "" {
			modelLine = "model: " + model
		}
		body = fmt.Sprintf(`# BONNIE agent manifest — schema %s.
# Every key is strict: an unknown key is an error, never a silent default.
apiVersion: %s

# Shown in operator-facing listings, such as bonnie runs list.
title: %s

# The model, as provider/name. Uncomment to leave the Kit default.
%s

# The system prompt file, read fresh at serve time.
instructions: instructions.md

# Tool calls run as this process until a sandbox is set — fine at a desk,
# wrong for a server. One of: none, docker, microsandbox, local, auto.
#sandbox:
#  kind: docker
#  image: python:3.12-slim
#  network:
#    mode: deny-all              # allow-all | deny-all | allow-list
#    allow: ["api.example.com"]  # hosts for allow-list mode

#channels:
#  http:
#    addr: ":8080"

# Files mirrored into every run's /workspace when its sandbox opens.
# Seeding never overwrites a file the model already wrote.
workspace: workspace/
`, APIVersion, APIVersion, opts.Title, modelLine)
		return out{name: "agent.yaml", body: body}, nil

	case "toml":
		modelLine := "# model = \"anthropic/claude-sonnet-4-5\""
		if model != "" {
			modelLine = "model = \"" + model + "\""
		}
		body = fmt.Sprintf(`# BONNIE agent manifest — schema %s.
# Every key is strict: an unknown key is an error, never a silent default.
apiVersion = "%s"
title = "%s"
%s
instructions = "instructions.md"
workspace = "workspace/"

# [sandbox]
# kind = "docker"
# image = "python:3.12-slim"
# [sandbox.network]
# mode = "deny-all"               # allow-all | deny-all | allow-list
# allow = ["api.example.com"]     # hosts for allow-list mode

# [channels.http]
# addr = ":8080"
`, APIVersion, APIVersion, opts.Title, modelLine)
		return out{name: "agent.toml", body: body}, nil

	case "json":
		m := map[string]string{
			"apiVersion":   APIVersion,
			"title":        opts.Title,
			"instructions": "instructions.md",
			"workspace":    "workspace/",
		}
		if model != "" {
			m["model"] = model
		}
		var b strings.Builder
		b.WriteString("{\n")
		first := true
		for _, k := range []string{"apiVersion", "title", "model", "instructions", "workspace"} {
			v, ok := m[k]
			if !ok {
				continue
			}
			if !first {
				b.WriteString(",\n")
			}
			first = false
			fmt.Fprintf(&b, "  %q: %q", k, v)
		}
		b.WriteString("\n}\n")
		return out{name: "agent.json", body: b.String()}, nil

	default:
		return out{}, fmt.Errorf("bonnie: agent: unknown scaffold format %q: want yaml, toml, or json", opts.Format)
	}
}

func instructionsTemplate(title string) string {
	return fmt.Sprintf(`You are %s, an agent running on BONNIE.

Keep answers short and concrete. Say when you do not know something instead
of guessing. This file is your system prompt: edit it to make the agent
yours. `+"`go run .`"+` picks the change up live, and `+"`bonnie build`"+` ships it
in the binary.
`, title)
}

func goModTemplate(module, bonnie string) string {
	if bonnie == "" {
		return fmt.Sprintf(`module %s

go 1.27.0
`, module)
	}
	// Pin the release the scaffold was generated by. kit resolves
	// transitively through bonnie, so only bonnie needs a require to overcome
	// a proxy whose @latest has not yet surfaced the newest release.
	return fmt.Sprintf(`module %s

go 1.27.0

require github.com/mark3labs/bonnie %s
`, module, bonnie)
}

// pinVersion normalises a running bonnie version into a go.mod require.
// It returns "" when the version is not a clean semver (dev, a snapshot, a
// dirty tree), because pinning something non-release would be wrong.
func pinVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		return ""
	}
	return "v" + v
}

// mainTemplate renders the user's own serving binary. It is authored —
// BONNIE never rewrites it. It reads the manifest for the data (model,
// address, instructions path) and calls the two generated symbols,
// discoveredTools and embeddedManifest, for the tree's tools and its
// embedded data. This is the default agent: edit it to wire a sandbox or
// the chat channels.
func mainTemplate() string {
	return `// Command serves the agent defined by this tree.
//
// This file is yours. It wires the journal, the runner, and the HTTP channel;
// extend it as you like. The pieces BONNIE owns are in bonnie_gen.go — the
// tree's tools and the data a bonnie build embeds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mark3labs/bonnie/agent"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func main() {
	addr := flag.String("addr", "", "address to listen on (overrides the manifest)")
	model := flag.String("model", "", "model to use (overrides the manifest)")
	flag.Parse()

	// The manifest is the data file. It is read from disk while the agent
	// runs from its tree; a bonnie build binary has no manifest on the
	// host, so it falls back to the copy its generator embedded.
	m := loadManifest()

	journal, err := runtime.OpenFileJournal(".bonnie")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = journal.Close() }()

	var opts []kit.Option
	if *model != "" {
		opts = append(opts, kit.WithModel(*model))
	} else if mm := manifestModel(m); mm != "" {
		opts = append(opts, kit.WithModel(mm))
	}
	if prompt := instructions(m); prompt != "" {
		opts = append(opts, kit.WithSystemPrompt(prompt))
	}

	// The workspace is the agent's root for files. Without it, Kit's file
	// tools resolve a relative path against this process's working
	// directory, so a model's write lands on the tree itself — beside the
	// manifest, the instructions, and the journal.
	//
	// Kit's default core tools take no working directory, so the core set is
	// rebuilt with one and passed through WithTools; AllTools is that same
	// default set. If you switch to a sandbox below, DROP this block: the
	// sandbox replaces the core tools with its own, and host tools handed to
	// a sandboxed agent would give the model a shell on this machine. Seed
	// the sandbox with sandbox.Seeded(provider, workspaceDir(m)) instead.
	if dir := workspaceDir(m); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
		opts = append(opts, kit.WithTools(kit.AllTools(kit.WithWorkDir(dir))...))
	}

	// Tools run as this process. To isolate them, swap the factory:
	//
	//	sandbox.Agent(sandbox.Seeded(sandbox.Docker(), workspaceDir(m)),
	//		append(opts, kit.WithExtraTools(discoveredTools()...))...)
	//
	// See docs/SANDBOX.md. Without it, a model-chosen tool call has this
	// process's files, network, and credentials.
	opts = append(opts, kit.WithExtraTools(discoveredTools()...))
	runner := runtime.NewRunner(journal, runtime.KitAgent(opts...))
	channel := bonniehttp.New(runner)

	listen := *addr
	if listen == "" {
		listen = manifestAddr(m)
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           channel.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "serving on %s\n", listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		if err != nil {
			log.Fatal(err)
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatal(err)
	}
}

// loadManifest reads the agent manifest: from disk while running from the
// tree, else from the embedded copy a bonnie build shipped. The embedded
// copy keeps the model and address working on a host with no tree beside it.
func loadManifest() *agent.Manifest {
	if m, _, err := agent.Load("."); err == nil {
		return m
	}
	return embeddedManifest()
}

// manifestModel is the model the manifest names, tolerating no manifest.
func manifestModel(m *agent.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Model
}

// manifestAddr is the HTTP channel the manifest names, else the default.
func manifestAddr(m *agent.Manifest) string {
	if m != nil && m.Channels != nil && m.Channels.HTTP != nil && m.Channels.HTTP.Addr != "" {
		return m.Channels.HTTP.Addr
	}
	return ":8080"
}

// instructions is the system prompt: the manifest's instructions file read
// fresh from disk, else the embedded copy a bonnie build shipped.
func instructions(m *agent.Manifest) string {
	path := "instructions.md"
	if m != nil && m.Instructions != "" {
		path = m.Instructions
	}
	if b, err := os.ReadFile(path); err == nil {
		return string(b)
	}
	return embeddedInstructions()
}

// workspaceDir is the directory the agent's files live in, absolute. It is
// the manifest's workspace, or workspace/ when the key is absent — the
// directory bonnie init scaffolds.
func workspaceDir(m *agent.Manifest) string {
	rel := "workspace"
	if m != nil && m.Workspace != "" {
		rel = m.Workspace
	}
	abs, err := filepath.Abs(filepath.Clean(rel))
	if err != nil {
		return ""
	}
	return abs
}
`
}

// genTemplate renders the disposable generated-wiring stub. The generator
// rewrites exactly this file from tools/ on the next build or dev run,
// replacing it with the real discoveredTools and the embedded instructions,
// manifest, skills, and workspace a `bonnie build` ships in the binary. The
// stub defines the same symbols empty so the fresh scaffold compiles.
func genTemplate() string {
	return `// Code generated as part of the scaffold; DO NOT EDIT.
//
// bonnie_gen.go is disposable: delete it and regenerate with bonnie build
// or bonnie dev. Everything else in this tree is authored and BONNIE never
// rewrites it.
package main

import (
	"embed"

	"github.com/mark3labs/bonnie/agent"
	kit "github.com/mark3labs/kit/pkg/kit"
)

var (
	_instructions string
	_skills        embed.FS
	_workspace     embed.FS
	_manifest      []byte
)

// discoveredTools returns the tree's tools. The scaffold defines it empty;
// the generator rewrites it from tools/<name>/tool.go on the next build.
func discoveredTools() []kit.Tool { return nil }

// embeddedInstructions is supplied by the generator on build.
func embeddedInstructions() string { return _instructions }

// embeddedSkills is the tree's skills directory, embedded at build time.
func embeddedSkills() embed.FS { return _skills }

// embeddedWorkspace is the tree's workspace seed directory, embedded at
// build time.
func embeddedWorkspace() embed.FS { return _workspace }

// embeddedManifest is the manifest a bonnie build embedded, parsed.
func embeddedManifest() *agent.Manifest { return nil }
`
}

// toolTemplate renders the sample tool. The directory name is the tool name
// in discovery, so the package is tools/echo and the tool is "echo".
func toolTemplate() string {
	return `// Package echo holds the scaffolded agent's sample tool.
//
// The directory name is the tool's name in discovery: this package lives in
// tools/echo, so the tool is "echo". Keep the name passed to kit.NewTool
// equal to the directory name.
package echo

import (
	"context"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the echo tool. Discovery calls this function; it must stay
// exported with exactly this signature.
func Tool() kit.Tool {
	type input struct {
		Text string ` + "`json:\"text\" description:\"The text to echo back.\"`" + `
	}
	return kit.NewTool("echo",
		"Echo the given text back. A sample tool: replace it with something real.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return kit.TextResult(in.Text), nil
		})
}
`
}
