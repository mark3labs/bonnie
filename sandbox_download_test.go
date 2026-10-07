package bonnie

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/bonnie/sandbox"
)

// Startup must find a journal-local CLI even with downloads disabled. No
// version policy is imposed on an existing binary: Available owns its probe.
func TestSandboxDownloadStartup(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	c := New(WithSandbox(sandbox.Microsandbox()), WithJournal(root), WithSandboxDownload(false)).cfg
	if _, err := c.agentFactory(t.Context(), "", nil); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("missing CLI: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "msb"), []byte("#!/bin/sh\necho 'msb test'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	factory, err := c.agentFactory(t.Context(), "", nil)
	if err != nil || factory == nil {
		t.Fatalf("local CLI: %v", err)
	}
	if defaults().noSandboxDownload {
		t.Fatal("downloads must be enabled by default")
	}
}
