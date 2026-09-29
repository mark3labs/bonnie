// Package treetest builds throwaway agent trees that compile against this
// checkout, for tests that need a real `go build` of a scaffolded module.
//
// A scaffolded tree requires github.com/mark3labs/bonnie. A test cannot let
// that resolve from the proxy: it would build the last release, not the code
// under test. The tree therefore gets a replace directive pointing at this
// checkout, written into its own go.mod — never into BONNIE's.
//
// The replace names only bonnie, and kit resolves through BONNIE's own
// go.mod, so these tests build the same pair of versions CI and a user's
// `go mod tidy` do.
package treetest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Root is this repository's absolute path, found from this file's own
// location, so it does not depend on which package's tests are running.
func Root() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	// internal/treetest/treetest.go -> the repository root
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// LinkToCheckout points a scaffolded tree's go.mod at this checkout, so
// `go build` in that tree compiles the BONNIE under test.
//
// It appends the require and the replace rather than rewriting the file, so
// the scaffold's own contents — the module line, the go line, and any pinned
// release — stay exactly as the scaffold wrote them and remain assertable.
func LinkToCheckout(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "go.mod")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("treetest: read go.mod: %v", err)
	}
	body := string(b)

	// A released scaffold already requires bonnie at its pinned version; the
	// replace redirects whatever version is required. A dev scaffold requires
	// nothing yet, so the require is added first.
	if !strings.Contains(body, "require github.com/mark3labs/bonnie") {
		body += "\nrequire github.com/mark3labs/bonnie v0.0.0\n"
	}
	body += fmt.Sprintf("\nreplace github.com/mark3labs/bonnie => %s\n", Root())

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("treetest: write go.mod: %v", err)
	}
}

// BuildEnv is the environment a `go build` of a linked tree runs with.
//
// GOFLAGS=-mod=mod lets the build write the tree's go.sum. A scaffolded go.mod
// carries no sums, and the default readonly mode refuses rather than add them.
// Every module it needs is already in the cache, because this repository
// requires the same ones, so no network call is made.
//
// -ldflags=-w omits DWARF from a binary a test links. Go never caches a link,
// so each test that builds a binary pays for the link in full, and for a tree
// that links Kit, DWARF is half of that cost: about 4 s becomes about 2 s.
// A test never attaches a debugger. Panics and stack traces read the pclntab,
// not DWARF, so the binary behaves the same. A command line that passes its
// own -ldflags replaces this one.
func BuildEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GOFLAGS=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOFLAGS=-mod=mod -ldflags=-w")
}

// Compile compiles every package in the tree at dir with the Go compiler and
// fails the test on the first error. It is for a test whose claim is "this
// compiles". Use `go build` only when the test runs the binary.
//
// `go build ./...` links each main package and then discards the result. The
// link is never cached, and for a tree that links Kit it takes about 4 s, which
// is almost all the cost of such a test. `go list -export` runs the compiler
// on each package as `go build` does, and shares its build cache, but stops
// before the link. A type error, an undefined name, or a bad import fails it
// exactly as it fails a build.
//
// env is the command's environment; nil means [BuildEnv].
func Compile(t *testing.T, dir string, env []string) {
	t.Helper()
	if env == nil {
		env = BuildEnv()
	}
	cmd := exec.Command("go", "list", "-export", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("treetest: %s does not compile: %v\n%s", dir, err, out)
	}
}
