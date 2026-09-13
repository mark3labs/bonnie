package agent

import (
	"fmt"
	"os"
	"path/filepath"
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

	// Tools adds the Go module: go.mod, a main.go that serves the tree,
	// one sample tool, and the generated wiring stub. The zero-Go path is
	// the default and stays the default.
	Tools bool
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
// The zero-Go scaffold is the manifest, instructions.md, and two
// directories. With Tools, it is a Go module whose `go build ./...` passes:
// main.go is the user's own wiring, and the generated wiring it calls is the
// one file BONNIE owns (and regenerates).
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

	files := []scaffoldFile{
		{path: manifest.name, content: manifest.body, mode: 0o644},
		{path: "instructions.md", content: instructionsTemplate(opts.Title), mode: 0o644},
		{path: filepath.Join("skills", gitkeep), content: "", mode: 0o644},
		{path: filepath.Join("workspace", gitkeep), content: "", mode: 0o644},
	}
	if opts.Tools {
		module := moduleName(opts.Title)
		files = append(files,
			scaffoldFile{path: "go.mod", content: goModTemplate(module), mode: 0o644},
			scaffoldFile{path: "main.go", content: mainTemplate(module), mode: 0o644},
			scaffoldFile{path: "bonnie_gen.go", content: genTemplate(module), mode: 0o644},
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
yours, and `+"`bonnie serve --agent .`"+` picks the change up on the next run.
`, title)
}

func goModTemplate(module string) string {
	return fmt.Sprintf(`module %s

go 1.27.0
`, module)
}

// mainTemplate renders the user's own serving binary. It is authored —
// BONNIE never rewrites it — and calls the one generated symbol,
// discoveredTools, for the tree's tools.
func mainTemplate(module string) string {
	return fmt.Sprintf(`// Command %[1]s serves the agent defined by this tree.
//
// This file is yours. It wires the journal, the runner, and the HTTP channel
// by hand; extend it as you like. The one piece BONNIE owns is
// bonnie_gen.go, which supplies the tree's tools.
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
	"syscall"
	"time"

	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	model := flag.String("model", "", "model to use, for example anthropic/claude-sonnet-4-5")
	flag.Parse()

	journal, err := runtime.OpenFileJournal(".bonnie")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = journal.Close() }()

	var opts []kit.Option
	if *model != "" {
		opts = append(opts, kit.WithModel(*model))
	}
	// The instructions file is the agent's system prompt, read fresh each
	// start — the same behaviour as bonnie serve --agent. A bonnie build
	// binary has no instructions.md on the host, so it falls back to the
	// copy its generator embedded.
	if b, err := os.ReadFile("instructions.md"); err == nil {
		opts = append(opts, kit.WithSystemPrompt(string(b)))
	} else if !os.IsNotExist(err) {
		log.Fatal(err)
	} else if inst := embeddedInstructions(); inst != "" {
		opts = append(opts, kit.WithSystemPrompt(inst))
	}

	// Tools run as this process. To isolate them, swap the factory:
	//
	//	sandbox.Agent(sandbox.Docker(), append(opts, kit.WithExtraTools(discoveredTools()...))...)
	//
	// See docs/SANDBOX.md. Without it, a model-chosen tool call has this
	// process's files, network, and credentials.
	opts = append(opts, kit.WithExtraTools(discoveredTools()...))
	runner := runtime.NewRunner(journal, runtime.KitAgent(opts...))
	channel := bonniehttp.New(runner)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           channel.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "%[1]s: serving on %%s\n", *addr)
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
`, module)
}

// genTemplate renders the disposable generated-wiring stub. The generator
// that lands with codegen rewrites exactly this file from tools/, replacing it
// with the real discoveredTools and the embedded instructions, skills, and
// workspace a `bonnie build` ships in the binary.
func genTemplate(module string) string {
	return fmt.Sprintf(`// Code generated as part of the scaffold; DO NOT EDIT.
//
// bonnie_gen.go is disposable: delete it and regenerate with bonnie build
// or bonnie dev. Everything else in this tree is authored and BONNIE never
// rewrites it.
package main

import (
	"embed"

	echo "%[1]s/tools/echo"

	kit "github.com/mark3labs/kit/pkg/kit"
)

var (
	_instructions string
	_skills        embed.FS
	_workspace     embed.FS
)

// discoveredTools is the wiring point for the tree's tools. The generator
// rewrites this function from tools/<name>/tool.go — each directory exports
// func Tool() kit.Tool, and the directory name is the tool's name.
func discoveredTools() []kit.Tool {
	return []kit.Tool{
		echo.Tool(),
	}
}

// embeddedInstructions is supplied by the code generator on build; the stub
// leaves it empty until a build embeds the real instructions.
func embeddedInstructions() string { return _instructions }

// embeddedSkills is the tree's skills directory, embedded at build time.
func embeddedSkills() embed.FS { return _skills }

// embeddedWorkspace is the tree's workspace seed directory, embedded at
// build time.
func embeddedWorkspace() embed.FS { return _workspace }
`, module)
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
