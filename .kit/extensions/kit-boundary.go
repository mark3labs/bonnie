//go:build ignore

// Package main holds BONNIE's public-API-boundary guard, a Kit extension.
//
// BONNIE uses the public Kit SDK only: github.com/mark3labs/kit/pkg/kit.
// A direct import of github.com/mark3labs/kit/internal/... breaks that rule
// everywhere. A direct import of charm.land/fantasy breaks it in the code
// BONNIE ships; a test, and the test-only package internal/fakemodel, may
// import fantasy to build a scripted model for a real Kit. This extension
// refuses the `write` and `edit` tool calls that would add a forbidden
// import, so the violation never reaches the disk and the agent gets told why
// in the same turn.
//
// This is the FIRST of three layers. The other two stay in place and remain
// the authority:
//
//	Go compiler  BONNIE's module path is not a prefix of Kit's, so the
//	             `internal` rule applies and the build fails.
//	depguard     .golangci.yml denies both paths; `task lint` and the CI
//	             `lint` job run it on every change, no matter who wrote it.
//
// This extension only runs when a person drives Kit in this repository. It
// cannot see an edit made in an editor, by a different tool, or by a
// dependency bump. Do not delete the depguard rule because this file exists.
//
// Kit loads it automatically from .kit/extensions/. See AGENTS.md.
//
// TODO(kit): this file has no test in this repository. Kit's harness at
// pkg/extensions/test signs its API with internal/extensions types, so a test
// would have to make the very import this guard forbids. Tracked upstream at
// https://github.com/mark3labs/kit/issues/136; write the test when it lands.
package main

import (
	"encoding/json"
	"strings"

	"kit/ext"
)

// forbidden lists the import path prefixes BONNIE must never name directly.
// A rule with testOK set does not apply to test code: a _test.go file, or a
// file in internal/fakemodel.
var forbidden = []struct {
	prefix string
	reason string
	testOK bool
}{
	{
		prefix: "github.com/mark3labs/kit/internal",
		reason: "BONNIE depends on the public Kit SDK only. Use " +
			"github.com/mark3labs/kit/pkg/kit. If what you need is not exported, " +
			"open an issue on Kit to export it from pkg/kit rather than reaching in.",
	},
	{
		prefix: "charm.land/fantasy",
		reason: "BONNIE's shipped code names Kit model types through the aliases in " +
			"github.com/mark3labs/kit/pkg/kit (kit.LLMMessage, kit.LLMToolCallPart, ...), " +
			"never through fantasy directly. A direct import pins BONNIE to Kit's own " +
			"transitive dependency and breaks the moment Kit moves it. Only test code " +
			"may import fantasy: a _test.go file, or internal/fakemodel.",
		testOK: true,
	},
	{
		prefix: "github.com/mark3labs/bonnie/internal/fakemodel",
		reason: "internal/fakemodel imports charm.land/fantasy, so it is for tests " +
			"only. Import it from a _test.go file; shipped code must not reach " +
			"fantasy through it.",
		testOK: true,
	},
}

// isTestCode reports whether path is test code: a _test.go file, or a file
// in the test-only package internal/fakemodel.
func isTestCode(path string) bool {
	p := strings.ReplaceAll(path, "\\", "/")
	return strings.HasSuffix(p, "_test.go") || strings.Contains(p, "internal/fakemodel/")
}

// Init registers the guard on the two tools that put text into a Go file.
func Init(api ext.API) {
	api.OnToolCall(func(tc ext.ToolCallEvent, ctx ext.Context) *ext.ToolCallResult {
		name := strings.ToLower(tc.ToolName)
		if name != "write" && name != "edit" {
			return nil
		}

		args := tc.ParsedArgs
		if args == nil {
			// ParsedArgs is nil when Kit could not parse the input. Try
			// again here; a call this guard cannot read must not pass
			// silently, but it also must not fail the turn.
			if err := json.Unmarshal([]byte(tc.Input), &args); err != nil {
				return nil
			}
		}

		path, _ := args["path"].(string)
		if !isGuardedGoFile(path, ctx.CWD) {
			return nil
		}

		for _, text := range addedText(args) {
			for _, line := range strings.Split(text, "\n") {
				imp := importPath(line)
				if imp == "" {
					continue
				}
				for _, f := range forbidden {
					if f.testOK && isTestCode(path) {
						continue
					}
					if imp == f.prefix || strings.HasPrefix(imp, f.prefix+"/") {
						return &ext.ToolCallResult{
							Block: true,
							Reason: "Blocked: this edit adds the import \"" + imp +
								"\" to " + path + ".\n\n" + f.reason +
								"\n\nSee AGENTS.md.",
						}
					}
				}
			}
		}
		return nil
	})
}

// isGuardedGoFile reports whether path is a Go file in THIS repository that
// the rule applies to.
//
// Two exemptions, both learned the hard way:
//
//   - **Outside the repository is not BONNIE's business.** The rule is about
//     BONNIE's module. Kit's own checkout must import `kit/internal` — that
//     is what an internal package is for — and `AGENTS.md` documents working
//     against Kit HEAD through a replace to `../kit`. A guard that blocks an
//     edit there is simply wrong. `cwd` is the session's working directory,
//     which is this repository, because that is how Kit found this extension.
//   - **The guard's own directory.** The file you are reading names both
//     forbidden paths, and a guard that refuses its own repair is a trap.
func isGuardedGoFile(path, cwd string) bool {
	if !strings.HasSuffix(path, ".go") {
		return false
	}
	p := strings.ReplaceAll(path, "\\", "/")
	if strings.Contains(p, ".kit/extensions/") {
		return false
	}

	if strings.HasPrefix(p, "/") {
		root := strings.TrimSuffix(strings.ReplaceAll(cwd, "\\", "/"), "/")
		return root != "" && strings.HasPrefix(p, root+"/")
	}
	// A relative path resolves against cwd, so it is inside the repository
	// unless it climbs out.
	return !strings.HasPrefix(p, "../")
}

// addedText returns every piece of text the call would put into the file:
// the whole body for `write`, each replacement for `edit`.
func addedText(args map[string]any) []string {
	var out []string
	if content, ok := args["content"].(string); ok {
		out = append(out, content)
	}
	if edits, ok := args["edits"].([]any); ok {
		for _, e := range edits {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if newText, ok := m["new_text"].(string); ok {
				out = append(out, newText)
			}
		}
	}
	return out
}

// importPath returns the package path a line imports, or "" when the line is
// not an import spec.
//
// The check is line-shaped rather than AST-shaped on purpose: an `edit` gives
// a fragment, and a fragment does not parse. Matching the spec form — an
// optional alias, then a quoted path, then nothing — is what keeps a mention
// of the path in a comment or a string from being read as an import.
//
// The trailing check is the load-bearing part. Without it a composite-literal
// element on its own line,
//
//	var deny = []string{
//		"github.com/mark3labs/kit/internal",
//
// is indistinguishable from an import spec, and the guard refuses to let
// anyone write a deny-list or a test fixture about this very rule. An import
// spec never carries a comma; a gofmt'd literal element always does.
func importPath(line string) string {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "*") {
		return ""
	}
	s = strings.TrimPrefix(s, "import ")
	s = strings.TrimSpace(s)

	// An alias may come first: `x "path"`, `_ "path"`, `. "path"`.
	if !strings.HasPrefix(s, `"`) {
		alias, rest, found := strings.Cut(s, " ")
		if !found || !isAlias(alias) {
			return ""
		}
		s = strings.TrimSpace(rest)
		if !strings.HasPrefix(s, `"`) {
			return ""
		}
	}

	path, tail, closed := strings.Cut(s[1:], `"`)
	if !closed || path == "" || strings.ContainsAny(path, " \t") {
		return ""
	}

	// Anything after the path other than a comment means this is a literal, a
	// map key, a call argument — not an import.
	if tail = strings.TrimSpace(tail); tail != "" && !strings.HasPrefix(tail, "//") {
		return ""
	}
	return path
}

// isAlias reports whether s is a legal import alias.
func isAlias(s string) bool {
	if s == "_" || s == "." {
		return true
	}
	for i, r := range s {
		ok := r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return s != ""
}
