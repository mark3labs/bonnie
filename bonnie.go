// Package bonnie is the entry point of an authored agent tree.
//
// A BONNIE agent is a directory of files whose meaning comes from their
// paths, and one Go file that starts it:
//
//	package main
//
//	import "github.com/mark3labs/bonnie"
//
//	func main() { bonnie.Main() }
//
// That is the whole default agent. Every slot in the tree has a framework
// default, and authoring the slot replaces it: instructions.md is the system
// prompt, workspace/ is the directory the agent's files live in, tools/ holds
// one directory per tool, and .bonnie holds the journal. Configuration that
// is not a file is code — an [Option] on [Main]:
//
//	func main() {
//		bonnie.Main(
//			bonnie.WithModel("anthropic/claude-sonnet-4-5"),
//			bonnie.WithSandbox(sandbox.Docker()),
//		)
//	}
//
// There is no manifest file. Data-shaped settings live at their default
// paths, and everything else is a Go call, so a setting that does not exist
// is a compile error rather than a key that is accepted and ignored.
//
// The tree's code — its tools — and the copies of its data files that a
// `bonnie build` binary carries are wired by codegen into bonnie_gen.go,
// which calls [Register] from its init. main.go never has to name them.
package bonnie

import (
	"embed"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// The default layout of a scaffolded tree. These are the paths `bonnie init`
// writes, the paths codegen embeds, and the paths [Run] reads when no option
// overrides them. They are constants rather than five copies of a string
// literal, because the scaffold, the generator, the dev loop, and the runtime
// must agree on the answer: a rule written more than once is a rule one
// caller can honour while another misses it.
const (
	// DefaultInstructions is the system prompt file, relative to the tree.
	DefaultInstructions = "instructions.md"

	// DefaultWorkspace is the directory the agent's files live in: the
	// working directory of the host's file tools without a sandbox, and the
	// seed mirrored into the sandbox with one.
	DefaultWorkspace = "workspace"

	// DefaultSkills is the tree's skills directory.
	DefaultSkills = "skills"

	// DefaultJournal is the directory the run journal is written to.
	DefaultJournal = ".bonnie"

	// DefaultAddr is the address the HTTP channel binds when none is given.
	DefaultAddr = ":8080"
)

// Tree is what codegen discovered in an agent tree: the code it wired and the
// data files it embedded. The generated bonnie_gen.go builds one and hands it
// to [Register] from its init, so main.go never names a tool or an embed.
//
// A tree run from its source directory reads its data files from disk and
// uses the embedded copies only as a fallback. A binary from `bonnie build`
// has no tree beside it, so the embedded copies are all it has.
type Tree struct {
	// Tools are the tools discovered under tools/, one per directory.
	Tools []kit.Tool

	// Instructions is the embedded copy of the tree's instructions file.
	Instructions string

	// Skills is the embedded copy of the tree's skills directory.
	//
	// Reserved: it is embedded so a later skill loader has it, and nothing
	// reads it today. It is stated here rather than implied, because a field
	// that quietly does nothing is the failure this package exists to avoid.
	Skills embed.FS

	// Workspace is the embedded copy of the tree's workspace seed files.
	// [Run] materialises them beside a built binary that has no tree, and
	// never overwrites a file that is already there.
	Workspace embed.FS
}

var (
	treeMu sync.RWMutex
	tree   Tree
)

// Register hands the generated wiring to the runtime. The generated
// bonnie_gen.go calls it from init, before main runs.
//
// It is the seam that keeps main.go short: adding a tool to the tree changes
// the generated file and nothing the author wrote.
func Register(t Tree) {
	treeMu.Lock()
	defer treeMu.Unlock()
	tree = t
}

// Registered returns what the generated file registered. It is the zero
// [Tree] when a tree has not been generated yet, which is what a hand-written
// main.go with no tools sees.
//
// Call it at run time — inside main, or later. The generated file registers
// from init, and Go initialises every package-level variable before it runs
// any init function, so a package-level `var x = bonnie.Registered()` reads
// the empty tree.
func Registered() Tree {
	treeMu.RLock()
	defer treeMu.RUnlock()
	return tree
}
