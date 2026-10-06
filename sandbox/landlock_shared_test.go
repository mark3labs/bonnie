package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLandlockSharedWorkspacePersistenceCleanupAndConfinement(t *testing.T) {
	if err := landlockSupported(); err != nil {
		t.Skipf("Landlock unavailable: %v", err)
	}
	ctx := context.Background()
	base := t.TempDir()
	shared := filepath.Join(base, "exact-workspace")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Landlock(WithLandlockRoot(filepath.Join(base, "unused")), WithLandlockCleanup())
	if err := p.UseSharedWorkspace(shared); err != nil {
		t.Fatal(err)
	}
	first, err := p.Open(ctx, "run-one")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.WorkingDir("run-one"); got != shared {
		t.Fatalf("WorkingDir = %q, want exact %q", got, shared)
	}
	if got, err := first.Exec(ctx, Command{Args: []string{"sh", "-c", "pwd"}}); err != nil || strings.TrimSpace(got.Stdout) != shared {
		t.Fatalf("pwd = %q, err %v", got.Stdout, err)
	}
	if _, err := p.Open(ctx, "run-two"); err == nil {
		t.Fatal("overlapping shared run accepted")
	}
	if err := first.WriteFile(ctx, "saved.txt", []byte("persist")); err != nil {
		t.Fatal(err)
	}
	if removed, err := p.DeleteRun(ctx, "run-one"); err != nil || removed {
		t.Fatalf("DeleteRun = %v, %v", removed, err)
	}
	if err := first.(Deleter).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shared, "saved.txt")); err != nil {
		t.Fatalf("shared data removed: %v", err)
	}
	second, err := p.Open(ctx, "run-two")
	if err != nil {
		t.Fatal(err)
	}
	data, err := second.ReadFile(ctx, "saved.txt")
	if err != nil || string(data) != "persist" {
		t.Fatalf("reopened data %q, %v", data, err)
	}
	result, err := second.Exec(ctx, Command{Args: []string{"sh", "-c", "cat \"$1\"", "sh", outside}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatal("read outside workspace succeeded")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
