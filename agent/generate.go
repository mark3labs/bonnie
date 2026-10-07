// Package agent implements BONNIE's L2 discovery: the authored agent tree.
//
// An agent is a directory of files with meaning from their paths. This
// package scaffolds new trees and generates the wiring for them, and holds
// the rules that keep discovery honest:
//
//   - Discovery refuses what it cannot fully honor. A tool directory that
//     does not export Tool(), or two tools that declare one name, is an
//     error naming the directories — never a partial generation.
//   - Generated files are disposable; authored files are sacred. Only
//     bonnie_gen.go is written by BONNIE, and init never overwrites.
//
// There is no manifest. A tree's data lives at the default paths the
// [github.com/mark3labs/bonnie] package names, embedded at build time; its
// code resolves at build time by codegen. There is no run-time plugin
// loading anywhere in BONNIE.
package agent

import (
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/bonnie"
)

// This file is BONNIE's L2 codegen: the build-time half of discovery. A tree's
// code — its custom tools — resolves here, because Go compiles. The generator
// walks tools/<name>/*.go, requires each directory to export func Tool()
// kit.Tool, and emits the one file BONNIE owns: bonnie_gen.go.
//
// The generator emits only imports it is allowed to (the tool packages,
// kit/pkg/kit, and embed) — it cannot emit a boundary violation, because it
// never reaches for one.

// Tool is one discovered tool, named by its directory per the L2 contract.
type Tool struct {
	// Name is the tool's name, which is the directory name under tools/.
	Name string

	// Dir is the absolute path of the tool directory.
	Dir string

	// ImportPath is the Go import path of the tool's package, which is
	// Module + "/tools/" + Name.
	ImportPath string

	// DeclaredName is the name the tool's code passes to kit.NewTool. It is
	// a best-effort parse used only for duplicate detection; when the code
	// builds the tool through a helper, it is empty and no false positive is
	// reported.
	DeclaredName string
}

// Plan is a discovery of one agent tree: what codegen found and what it will
// embed. It is the input to [Render] and the body of a --dry-run report.
type Plan struct {
	// Root is the agent root directory.
	Root string

	// Module is the module path from the tree's go.mod.
	Module string

	// Tools is the discovered tools, sorted by name.
	Tools []Tool

	// Embeds are the paths codegen embeds into the binary. instructions.md
	// is always present in an authored tree; skills and context appear
	// when their directory holds real files (never a bare .gitkeep). The
	// order is stable, so two runs over one tree are byte-identical.
	Embeds []Embed

	// OutFile is the generated file's name, always bonnie_gen.go.
	OutFile string
}

// ErrNotAModule is returned when a tree has no go.mod, so codegen cannot know
// the module path to import a tool package by.
var ErrNotAModule = fmt.Errorf("bonnie: agent: not a Go module: no go.mod")

// Embed is one path the generator embeds into the binary. A scalar (a single
// file, the instructions) binds to a string; a directory (skills, context)
// binds to an embed.FS. Var is the identifier the //go:embed directive binds
// to, so the generated accessors can expose it.
type Embed struct {
	Path string
	Var  string
	Dir  bool
}

// sentinel errors returned by codegen. Test with [errors.Is].
var (
	// ErrDuplicateTool means two tools in one tree declare the same runtime
	// name, which a provider would silently resolve to one. It names both
	// directories.
	ErrDuplicateTool = errors.New("bonnie: agent: duplicate tool name")

	// ErrToolMissingToolFunc means a tools/<name>/ directory does not export
	// func Tool() kit.Tool, which codegen requires.
	ErrToolMissingToolFunc = errors.New("bonnie: agent: tool does not export Tool()")
)

// Discover walks the agent tree at root and returns its build-time discovery
// plan. It reads go.mod for the module path, walks tools/ for one directory
// per tool, and computes the embed set from the default layout: the
// instructions file, skills/, and context/.
//
// Discovery is a build-time idea. It fails on anything it cannot fully honor:
// a tool directory that does not export Tool(), or two tools that declare the
// same name. This is invariant 13 — refuse, never partially generate.
func Discover(root string) (*Plan, error) {
	module, err := readModule(root)
	if err != nil {
		return nil, err
	}

	plan := &Plan{
		Root:    root,
		Module:  module,
		OutFile: "bonnie_gen.go",
	}

	toolsDir := filepath.Join(root, "tools")
	entries, err := os.ReadDir(toolsDir)
	switch {
	case os.IsNotExist(err):
		// No tools directory: a data-only tree. Codegen emits tools for
		// nothing and embeds the data half. That is legitimate — build of a
		// zero-G-Go tree still needs a module, which the caller checks.
	case err != nil:
		return nil, fmt.Errorf("bonnie: agent: read tools: %w", err)
	}

	byName := make(map[string]Tool)
	var order []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tool, err := discoverTool(root, module, e.Name())
		if err != nil {
			return nil, err
		}
		byName[tool.Name] = tool
		order = append(order, tool.Name)
	}
	sort.Strings(order)
	for _, name := range order {
		plan.Tools = append(plan.Tools, byName[name])
	}

	// Duplicate detection on the declared runtime name. The directory name is
	// the tool's name (naming from paths), but a provider keys tools by the
	// name their code passes to NewTool. Two directories that both declare
	// "echo" would silently collapse to one tool at run time, so codegen
	// refuses before it can happen.
	declared := make(map[string][]string)
	for _, t := range plan.Tools {
		if t.DeclaredName != "" {
			declared[t.DeclaredName] = append(declared[t.DeclaredName], t.Name)
		}
	}
	var dup []string
	for name, dirs := range declared {
		if len(dirs) > 1 {
			dup = append(dup, fmt.Sprintf("%s (%s)", name, strings.Join(quote(dirs), ", ")))
		}
	}
	if len(dup) > 0 {
		return nil, fmt.Errorf("%w: %s — a provider would silently keep only one", ErrDuplicateTool, strings.Join(dup, "; "))
	}

	// The embed set: the tree's data files, at the default layout's paths.
	// instructions.md is always present in an authored tree; skills/ and
	// context/ appear only when they hold real files. The order is stable,
	// so two runs over one tree are byte-identical.
	if hasRealFile(filepath.Join(root, bonnie.DefaultInstructions)) {
		plan.Embeds = append(plan.Embeds, Embed{Path: bonnie.DefaultInstructions, Var: "_instructions"})
	}
	for _, slot := range []struct{ path, varName string }{
		{bonnie.DefaultSkills, "_skills"},
		{bonnie.DefaultContextFiles, "_contextFiles"},
	} {
		if hasRealContent(filepath.Join(root, slot.path)) {
			plan.Embeds = append(plan.Embeds, Embed{Path: slot.path, Var: slot.varName, Dir: true})
		}
	}

	return plan, nil
}

// discoverTool validates one tools/<name> directory and returns its Tool.
func discoverTool(root, module, name string) (Tool, error) {
	dir := filepath.Join(root, "tools", name)
	tool := Tool{
		Name:       name,
		Dir:        dir,
		ImportPath: module + "/tools/" + name,
	}

	// At least one non-test .go file defining func Tool() kit.Tool is
	// required. A missing function is a generator error naming the
	// directory; the compiler backstop would catch it, but discovery should
	// not wait for the build to fail.
	fset := token.NewFileSet()
	sawToolFunc := false
	entries, err := os.ReadDir(dir)
	if err != nil {
		return tool, fmt.Errorf("bonnie: agent: read tool %s: %w", name, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		astFile, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return tool, fmt.Errorf("bonnie: agent: parse %s: %w", path, err)
		}
		for _, decl := range astFile.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "Tool" || fn.Type.Params.NumFields() != 0 || fn.Type.Results.NumFields() != 1 {
				continue
			}
			sawToolFunc = true
		}
		if declared := declaredToolName(astFile); declared != "" && tool.DeclaredName == "" {
			tool.DeclaredName = declared
		}
	}
	if !sawToolFunc {
		return tool, fmt.Errorf("%w: %s: want func Tool() kit.Tool", ErrToolMissingToolFunc, dir)
	}
	return tool, nil
}

// readModule parses the module path from a tree's go.mod.
func readModule(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotAModule
		}
		return "", fmt.Errorf("bonnie: agent: read go.mod: %w", err)
	}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("bonnie: agent: go.mod carries no module path")
}

// hasRealFile reports whether path is a regular file, not a symlink.
func hasRealFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// hasRealContent follows Go embed directory rules: hidden names, symlinks,
// empty directories, and nested modules do not supply an eligible file.
func hasRealContent(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == "go.mod" {
			return false
		}
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if hasRealContent(path) {
				return true
			}
		} else if hasRealFile(path) {
			return true
		}
	}
	return false
}

// declaredToolName reads direct constructor returns from the exported Tool
// function only. Helper-built, shadowed, or conditional names remain unknown:
// duplicate detection must not reject a tree on an unrelated constructor.
func declaredToolName(file *ast.File) string {
	alias := ""
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err == nil && path == "github.com/mark3labs/kit/pkg/kit" {
			alias = "kit"
			if spec.Name != nil {
				alias = spec.Name.Name
			}
		}
	}
	if alias == "" || alias == "." || alias == "_" {
		return ""
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "Tool" || fn.Body == nil {
			continue
		}
		// Only an unconditional, final return establishes a name. Calls in
		// helpers or other branches do not establish the returned tool.
		if len(fn.Body.List) == 0 {
			return ""
		}
		ret, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return ""
		}
		returns := 0
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if _, ok := node.(*ast.FuncLit); ok {
				return false
			}
			if _, ok := node.(*ast.ReturnStmt); ok {
				returns++
			}
			return true
		})
		if returns != 1 {
			return ""
		}
		call, ok := ret.Results[0].(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return ""
		}
		fun := call.Fun
		if indexed, ok := fun.(*ast.IndexExpr); ok {
			fun = indexed.X
		}
		if indexed, ok := fun.(*ast.IndexListExpr); ok {
			fun = indexed.X
		}
		sel, ok := fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewTool" {
			return ""
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != alias || pkg.Obj != nil {
			return ""
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if ok && literal.Kind == token.STRING {
			name, err := strconv.Unquote(literal.Value)
			if err == nil {
				return name
			}
		}
	}
	return ""
}

// Render produces the byte-identical content of the generated wiring file for
// a plan. It is pure, so two runs over one tree are identical.
//
// The output is gofmt-clean. The template alone was not: a tree with no tools
// rendered an empty composite literal across two lines, so every tree's
// bonnie_gen.go failed a `gofmt -l` check the moment it was committed. The
// examples, which are committed trees, are what found it.
//
// The file declares nothing the author could collide with: it binds the embed
// slots to unexported variables and hands everything to [bonnie.Register]
// from init. The three slots — instructions, skills, context — are always
// declared, so the embed import always compiles; a //go:embed directive is
// emitted only for the slots the plan found.
func (p *Plan) Render() ([]byte, error) {
	var b strings.Builder
	b.WriteString(`// Code generated by BONNIE (bonnie build / bonnie dev). DO NOT EDIT.
//
// bonnie_gen.go is disposable: delete it and regenerate. Every other file in
// this tree is authored and BONNIE never rewrites it. Discovery walks
// tools/<name>/tool.go; each directory exports func Tool() kit.Tool and the
// directory name is the tool's name.
package main

import (
	"embed"

	"github.com/mark3labs/bonnie"
`)
	for i, t := range p.Tools {
		fmt.Fprintf(&b, "\ttool%d %q\n", i, t.ImportPath)
	}
	if len(p.Tools) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(`	kit "github.com/mark3labs/kit/pkg/kit"
)

`)

	// The embed slots, in the plan's stable order. A scalar binds to a
	// string, a directory to an embed.FS. Any canonical slot the plan did not
	// find is declared empty, so the init below and the embed import always
	// compile.
	done := map[string]bool{}
	for _, e := range p.Embeds {
		done[e.Var] = true
		fmt.Fprintf(&b, "//go:embed %s\n", e.Path)
		if e.Dir {
			fmt.Fprintf(&b, "var %s embed.FS\n\n", e.Var)
			continue
		}
		fmt.Fprintf(&b, "var %s string\n\n", e.Var)
	}
	for _, slot := range []struct{ varName, typ string }{
		{"_instructions", "string"},
		{"_skills", "embed.FS"},
		{"_contextFiles", "embed.FS"},
	} {
		if !done[slot.varName] {
			fmt.Fprintf(&b, "var %s %s\n\n", slot.varName, slot.typ)
		}
	}

	b.WriteString(`// init hands the tree's discovered code and embedded data to the runtime
// before main runs, so main.go never has to name a tool or an embed. Each
// tools/<name>/tool.go exports func Tool() kit.Tool.
func init() {
	bonnie.Register(bonnie.Tree{
		Instructions: _instructions,
		Skills:       _skills,
		ContextFiles:    _contextFiles,
		Tools: []kit.Tool{
`)
	for i := range p.Tools {
		fmt.Fprintf(&b, "\t\t\ttool%d.Tool(),\n", i)
	}
	b.WriteString(`		},
	})
}
`)
	out, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("bonnie: agent: format generated file: %w", err)
	}
	return out, nil
}

// String is the --dry-run report: the files found, the tools generated, and
// the embed set.
func (p *Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "discovery plan for %s\n", p.Root)
	fmt.Fprintf(&b, "  module: %s\n", p.Module)
	if len(p.Tools) == 0 {
		b.WriteString("  tools: (none)\n")
	} else {
		b.WriteString("  tools:\n")
		for _, t := range p.Tools {
			fmt.Fprintf(&b, "    %s  (%s)  ->  %s\n", t.Name, filepath.Join("tools", t.Name), t.ImportPath)
		}
	}
	if len(p.Embeds) == 0 {
		b.WriteString("  embed: (none)\n")
	} else {
		b.WriteString("  embed:\n")
		for _, e := range p.Embeds {
			b.WriteString("    ")
			b.WriteString(e.Path)
			if e.Dir {
				b.WriteString("/")
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "  output: %s\n", p.OutFile)
	return b.String()
}

// Generate runs discovery and renders the generated file for root. It does not
// write anything — callers that build write [Plan.Render]'s output to
// root/bonnie_gen.go themselves.
func Generate(root string) ([]byte, *Plan, error) {
	p, err := Discover(root)
	if err != nil {
		return nil, nil, err
	}
	b, err := p.Render()
	if err != nil {
		return nil, nil, err
	}
	return b, p, nil
}
