package bonnie

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/sandbox"
)

func TestPersistentWorkspaceProviders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		provider sandbox.Provider
	}{
		{"default-landlock", nil},
		{"explicit-landlock", sandbox.Landlock()},
		{"explicit-local", sandbox.Local()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.provider
			if p == nil {
				p = sandbox.Landlock(sandbox.WithLandlockRoot(filepath.Join(t.TempDir(), "roots")))
			}
			cfg := resolve(WithSandbox(p), WithSharedDirectory(t.TempDir()))
			if _, err := cfg.agentFactory(context.Background(), "", nil); err != nil {
				t.Fatalf("agentFactory: %v", err)
			}
		})
	}
}

func TestPersistentWorkspaceDefaultLandlockWithFactory(t *testing.T) {
	cfg := resolve(WithSharedDirectory(t.TempDir()))
	f, err := cfg.agentFactory(context.Background(), "", nil)
	if err != nil || f == nil {
		t.Fatalf("factory=%v err=%v", f, err)
	}
}

func TestPersistentWorkspaceConflictsWithContextFilesBothOrders(t *testing.T) {
	for _, opts := range [][]Option{
		{WithContextFiles(DefaultContextFiles), WithSharedDirectory("shared")},
		{WithSharedDirectory("shared"), WithContextFiles(DefaultContextFiles)},
	} {
		_, err := resolve(opts...).agentFactory(context.Background(), "", nil)
		if err == nil || !strings.Contains(err.Error(), "WithContextFiles") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestPersistentWorkspaceRejectsEmptyPath(t *testing.T) {
	_, err := resolve(WithSharedDirectory("")).agentFactory(context.Background(), "", nil)
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("err=%v", err)
	}
}

func TestPersistentWorkspaceRejectsUnsupportedProvider(t *testing.T) {
	_, err := resolve(WithSandbox(sandbox.Docker()), WithSharedDirectory("shared")).agentFactory(context.Background(), "", nil)
	if err == nil || !strings.Contains(err.Error(), "supports sandbox.Landlock and sandbox.Local") {
		t.Fatalf("err=%v", err)
	}
}

func TestPersistentWorkspaceConflictsWithFactory(t *testing.T) {
	_, err := resolve(WithAgentFactory(stubFactory), WithSharedDirectory("shared")).agentFactory(context.Background(), "", nil)
	if err == nil || !strings.Contains(err.Error(), "WithSharedDirectory") {
		t.Fatalf("err=%v", err)
	}
}
