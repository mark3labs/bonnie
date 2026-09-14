// Package treetest builds throwaway agent trees that compile against this
// checkout, for tests that need a real `go build` of a scaffolded module.
//
// A scaffolded tree requires github.com/mark3labs/bonnie. A test cannot let
// that resolve from the proxy: it would build the last release, not the code
// under test. The tree therefore gets a replace directive pointing at this
// checkout, written into its own go.mod — never into BONNIE's.
//
// It is a replace and not a go.work on purpose. A workspace has to name every
// module in the graph, so it needed a kit checkout beside this repo: tests
// skipped where there was none, and where there was one they built against
// whatever that checkout happened to be rather than the version go.mod pins.
// A replace names only bonnie, and kit resolves through BONNIE's own go.mod —
// so these tests build the same pair of versions CI and a user's `go mod
// tidy` do.
package treetest

import (
	"fmt"
	"os"
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
// GOWORK is cleared: a developer with a workspace file above their checkout
// would otherwise have it silently pulled in, and the build under test would
// stop being the build CI runs.
//
// GOFLAGS=-mod=mod lets the build write the tree's go.sum. A scaffolded go.mod
// carries no sums, and the default readonly mode refuses rather than add them.
// Every module it needs is already in the cache, because this repository
// requires the same ones, so no network call is made.
func BuildEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GOWORK=") || strings.HasPrefix(kv, "GOFLAGS=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOWORK=off", "GOFLAGS=-mod=mod")
}
