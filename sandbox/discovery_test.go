package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryWrappersPreserveCapabilities(t *testing.T) {
	t.Parallel()
	for _, env := range []bool{false, true} {
		t.Run(map[bool]string{false: "seed", true: "environment"}[env], func(t *testing.T) {
			t.Parallel()
			var p Provider = Local(WithLocalRoot(t.TempDir()))
			p = Seeded(p, t.TempDir())
			if env {
				p = EnvInjected(p, map[string]string{"DISCOVERY_TEST": "1"})
			}
			sb, err := p.Open(context.Background(), "discovery")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sb.Close() }()
			reader, ok := sb.(DiscoveryReader)
			if !ok {
				t.Fatal("lost discovery")
			}
			for _, capable := range []bool{implementsDirectory(sb), implementsJobs(sb), implementsDelete(sb)} {
				if !capable {
					t.Fatal("lost optional capability")
				}
			}
			if err := sb.WriteFile(context.Background(), "regular", []byte("data")); err != nil {
				t.Fatal(err)
			}
			file, err := reader.ReadDiscovery(context.Background(), "regular", 10)
			if err != nil || string(file.Data) != "data" {
				t.Fatalf("read=%s err=%v", file.Data, err)
			}
		})
	}
}
func implementsDirectory(s Sandbox) bool { _, ok := s.(DirectoryReader); return ok }
func implementsJobs(s Sandbox) bool      { _, ok := s.(JobSandbox); return ok }
func implementsDelete(s Sandbox) bool    { _, ok := s.(Deleter); return ok }
func TestDiscoveryLocalRootIsPinned(t *testing.T) {
	t.Parallel()
	p := Local(WithLocalRoot(t.TempDir()))
	sb, err := p.Open(context.Background(), "pinned")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()
	if _, _, err := sb.(DiscoveryReader).ListDiscovery(context.Background(), ".", 10); err != nil {
		t.Fatal(err)
	}
	dir := p.WorkingDir("pinned")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := sb.(DiscoveryReader).ReadDiscovery(context.Background(), "file", 10)
	if err != nil || string(file.Data) != "saved" {
		t.Fatalf("root redirected: %s %v", file.Data, err)
	}
}
