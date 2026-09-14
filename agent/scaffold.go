package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mark3labs/bonnie"
)

// gitkeep is the empty-directory marker the scaffold writes. Version
// control tracks files, not directories, and the scaffolded skills/ and
// workspace/ start empty. The seeder skips it: it is bookkeeping, not seed
// content.
const gitkeep = ".gitkeep"

// InitOptions configures [Scaffold].
type InitOptions struct {
	// Model is written into main.go as a bonnie.WithModel option when set.
	// When empty, the option is left as a comment and Kit's default applies.
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
// Every tree is a Go module, and there is no manifest: the tree's data lives
// at the default paths ([bonnie.DefaultInstructions], [bonnie.DefaultWorkspace],
// [bonnie.DefaultSkills]) and everything else is code in main.go. main.go is
// authored and BONNIE never rewrites it; bonnie_gen.go is the wiring BONNIE
// owns and regenerates. `go build ./...` passes on the fresh scaffold.
// --tools adds a sample tool to tools/.
func Scaffold(dir string, opts InitOptions) ([]string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("bonnie: agent: resolve directory: %w", err)
	}

	module := moduleName(filepath.Base(abs))
	files := []scaffoldFile{
		{path: bonnie.DefaultInstructions, content: instructionsTemplate(module), mode: 0o644},
		{path: "go.mod", content: goModTemplate(module, pinVersion(opts.Version)), mode: 0o644},
		{path: "main.go", content: mainTemplate(opts.Model), mode: 0o644},
		{path: "bonnie_gen.go", content: genTemplate(), mode: 0o644},
		{path: filepath.Join(bonnie.DefaultSkills, gitkeep), content: "", mode: 0o644},
		{path: filepath.Join(bonnie.DefaultWorkspace, gitkeep), content: "", mode: 0o644},
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
// exists so a directory like "My Agent!" still builds.
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

// quote quotes every string, for error messages that list files.
func quote(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

func instructionsTemplate(name string) string {
	return fmt.Sprintf(`You are %s, an agent running on BONNIE.

Keep answers short and concrete. Say when you do not know something instead
of guessing. This file is your system prompt: edit it to make the agent
yours. `+"`bonnie dev`"+` picks the change up live, and `+"`bonnie build`"+` ships it
in the binary.
`, name)
}

func goModTemplate(module, bonnieVersion string) string {
	if bonnieVersion == "" {
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
`, module, bonnieVersion)
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
// BONNIE never rewrites it — and it is one call, because every slot in the
// tree already has a default: instructions.md is the prompt, workspace/ is
// the agent's root for files, .bonnie is the journal, and tools/ is wired by
// codegen into bonnie_gen.go.
//
// Everything that is not a file in the tree is an option here. The commented
// lines are the ones an author reaches for first, so the file doubles as the
// menu.
func mainTemplate(model string) string {
	modelLine := `		// bonnie.WithModel("anthropic/claude-sonnet-4-5"),`
	if model != "" {
		modelLine = fmt.Sprintf("\t\tbonnie.WithModel(%q),", model)
	}
	return fmt.Sprintf(`// Command serves the agent defined by this tree.
//
// This file is yours; BONNIE never rewrites it. The tree's data lives at its
// default paths — instructions.md is the system prompt, workspace/ is the
// agent's root for files — and the tools under tools/ are wired by codegen
// into bonnie_gen.go. Everything else is an option below.
package main

import (
	"github.com/mark3labs/bonnie"
)

func main() {
	bonnie.Main(
%s

		// Tool calls run as this process until a sandbox is set — fine at a
		// desk, wrong for a server reachable from outside. Uncomment to
		// isolate them; see docs/SANDBOX.md.
		//
		//	bonnie.WithSandbox(sandbox.Docker()),
		//	bonnie.WithNetwork(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}),

		// Chat channels mount beside the HTTP channel. Their credentials
		// come from the environment, never from this file.
		//
		//	bonnie.WithSlack(slack.Config{}),
		//	bonnie.WithDiscord(discord.Config{}),
		//	bonnie.WithTelegram(telegram.Config{Username: "mybot"}),
	)
}
`, modelLine)
}

// genTemplate renders the disposable generated-wiring stub. The generator
// rewrites exactly this file from the tree on the next build or dev run,
// replacing it with the real tool set and the embedded instructions, skills,
// and workspace a `bonnie build` ships in the binary. The stub registers an
// empty tree so the fresh scaffold compiles and runs before codegen has been
// anywhere near it.
func genTemplate() string {
	return `// Code generated as part of the scaffold; DO NOT EDIT.
//
// bonnie_gen.go is disposable: delete it and regenerate with bonnie build
// or bonnie dev. Everything else in this tree is authored and BONNIE never
// rewrites it.
package main

import (
	"embed"

	"github.com/mark3labs/bonnie"

	kit "github.com/mark3labs/kit/pkg/kit"
)

var _instructions string

var _skills embed.FS

var _workspace embed.FS

// init hands the tree's discovered code and embedded data to the runtime
// before main runs. The scaffold's stub registers nothing; bonnie build and
// bonnie dev rewrite this file from what they find in the tree.
func init() {
	bonnie.Register(bonnie.Tree{
		Instructions: _instructions,
		Skills:       _skills,
		Workspace:    _workspace,
		Tools:        []kit.Tool{},
	})
}
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
