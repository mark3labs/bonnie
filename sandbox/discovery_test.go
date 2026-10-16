package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/skillfs"
)

func TestDiscoveryWrappersAndConfinement(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"local", "landlock"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			var p Provider = Local(WithLocalRoot(t.TempDir()))
			if kind == "landlock" {
				p = Landlock(WithLandlockRoot(t.TempDir()))
			}
			p = Seeded(EnvInjected(p, map[string]string{"IGNORED": "value"}), t.TempDir())
			sb, err := p.Open(ctx, "discovery")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sb.Close() }()
			capability, ok := sb.(SkillDiscovery)
			if !ok {
				t.Fatal("wrapper lost discovery")
			}
			if err := sb.WriteFile(ctx, "dir/valid", []byte("body")); err != nil {
				t.Fatal(err)
			}
			file, err := capability.SkillFile(ctx, "dir/valid", 10)
			if err != nil || string(file.Data) != "body" {
				t.Fatalf("read=%+v err=%v", file, err)
			}
			for _, path := range []string{"/etc/passwd", "../escape", "dir/../valid"} {
				if _, err := capability.SkillFile(ctx, path, 100); err == nil {
					t.Fatalf("accepted %s", path)
				}
			}
			if _, err := capability.SkillFile(ctx, "dir/valid", 1); err == nil {
				t.Fatal("byte bound not enforced")
			}
			if _, ok := sb.(DirectoryReader); !ok {
				t.Fatal("directory capability lost")
			}
			if _, ok := sb.(JobSandbox); !ok {
				t.Fatal("jobs lost")
			}
		})
	}
}

func TestDiscoveryHelperPackaging(t *testing.T) {
	t.Parallel()
	for _, arch := range []string{"amd64", "arm64"} {
		data, err := discoveryHelper(arch)
		if err != nil {
			t.Logf("helper is not generated in this source build: %v", err)
			continue
		}
		if len(data) < 4 || string(data[:4]) != "\x7fELF" {
			t.Fatal("invalid packaged helper")
		}
	}
}

func TestDiscoveryRemoteProtocol(t *testing.T) {
	t.Parallel()
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "malformed"}[malformed], func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			script := filepath.Join(dir, "fake-cli")
			response := `{"version":1,"entries":[{"Name":"repo","Kind":"directory"}]}`
			if malformed {
				response = `{"version":999}`
			}
			// The fake backend has no workspace mount and does not read any host source.
			body := "#!/bin/sh\nprintf '%s\\n' '" + response + "'\nprintf '\\n__bonnie_exit_%s:0\\n' \"$1\"\n"
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			c := &cliSandbox{bin: script, backend: "fake-remote", killFn: func(context.Context, *cliSandbox) error { return nil }, discovery: remoteDiscovery{helper: "/private/trusted-helper"}, execArgs: func(_ *cliSandbox, argv []string, cmd Command) []string {
				// Capture the existing exit marker protocol without executing guest argv.
				marker := argv[2]
				_, nonce, found := strings.Cut(marker, markerPrefix)
				if !found {
					t.Fatal("missing exit marker")
				}
				nonce = nonce[:strings.Index(nonce, ":")]
				if len(cmd.Env) != 0 {
					t.Fatal("discovery helper unexpectedly inherited model environment")
				}
				var request skillfs.Request
				if len(argv) < 6 || json.Unmarshal([]byte(argv[len(argv)-1]), &request) != nil || request.Path != "." {
					t.Fatal("bad guest protocol")
				}
				return []string{nonce}
			}}
			entries, partial, err := c.SkillDirectory(context.Background(), ".", 100)
			if malformed {
				if err == nil {
					t.Fatal("malformed protocol accepted")
				}
				return
			}
			if err != nil || partial || !reflect.DeepEqual(entries, []DiscoveryEntry{{Name: "repo", Kind: "directory"}}) {
				t.Fatalf("entries=%+v partial=%v err=%v", entries, partial, err)
			}
		})
	}
}
