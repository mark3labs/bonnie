package client

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Client production dependencies must not pull in the journal or server.
// Inspect non-test graphs: client tests deliberately use runtime aliases.
func TestClientDependencies(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "github.com/mark3labs/bonnie/client", "github.com/mark3labs/bonnie/client/iroh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for dep := range strings.FieldsSeq(string(out)) {
		for _, prefix := range []string{
			"github.com/mark3labs/bonnie/runtime",
			"github.com/mark3labs/bonnie/channel",
			"modernc.org/sqlite",
			"github.com/go-git",
		} {
			if dep == prefix || strings.HasPrefix(dep, prefix+"/") {
				t.Errorf("client imports forbidden dependency %s", dep)
			}
		}
	}
}

func TestWireImports(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-f", `{{join .Imports "\n"}}`, "github.com/mark3labs/bonnie/wire")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for dep := range strings.FieldsSeq(string(out)) {
		if strings.Contains(dep, ".") && dep != "github.com/mark3labs/kit/pkg/kit" {
			t.Errorf("wire imports non-stdlib dependency %s", dep)
		}
	}
}
