package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files into a fresh temporary directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadAcceptsAllThreeFormats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file string
		body string
	}{
		{"agent.yaml", `
apiVersion: bonnie.dev/v0alpha
title: deploy-bot
model: anthropic/claude-sonnet-4-5
instructions: prompts/system.md
sandbox:
  kind: docker
  image: python:3.12-slim
  network:
    mode: allow-list
    allow: ["api.example.com", "*.github.com"]
channels:
  http:
    addr: ":9090"
workspace: seed/
`},
		{"agent.toml", `
apiVersion = "bonnie.dev/v0alpha"
title = "deploy-bot"
model = "anthropic/claude-sonnet-4-5"
instructions = "prompts/system.md"
workspace = "seed/"

[sandbox]
kind = "docker"
image = "python:3.12-slim"

[sandbox.network]
mode = "allow-list"
allow = ["api.example.com", "*.github.com"]

[channels.http]
addr = ":9090"
`},
		{"agent.json", `{
  "apiVersion": "bonnie.dev/v0alpha",
  "title": "deploy-bot",
  "model": "anthropic/claude-sonnet-4-5",
  "instructions": "prompts/system.md",
  "sandbox": {
    "kind": "docker",
    "image": "python:3.12-slim",
    "network": {
      "mode": "allow-list",
      "allow": ["api.example.com", "*.github.com"]
    }
  },
  "channels": {"http": {"addr": ":9090"}},
  "workspace": "seed/"
}`},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			t.Parallel()
			dir := writeTree(t, map[string]string{c.file: c.body})
			m, path, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if filepath.Base(path) != c.file {
				t.Fatalf("discovered %q, want %q", path, c.file)
			}
			if m.Title != "deploy-bot" || m.Model != "anthropic/claude-sonnet-4-5" ||
				m.Instructions != "prompts/system.md" || m.Workspace != "seed/" {
				t.Fatalf("scalars did not survive: %+v", m)
			}
			if m.Sandbox == nil || m.Sandbox.Kind != "docker" || m.Sandbox.Image != "python:3.12-slim" {
				t.Fatalf("sandbox did not survive: %+v", m.Sandbox)
			}
			if m.Sandbox.Network == nil || m.Sandbox.Network.Mode != "allow-list" ||
				len(m.Sandbox.Network.Allow) != 2 || m.Sandbox.Network.Allow[1] != "*.github.com" {
				t.Fatalf("network did not survive: %+v", m.Sandbox.Network)
			}
			if m.Channels == nil || m.Channels.HTTP == nil || m.Channels.HTTP.Addr != ":9090" {
				t.Fatalf("channels did not survive: %+v", m.Channels)
			}
		})
	}
}

// The strictness must be one code path: the same typo produces the same
// class of error in every format, and the error names the key.
func TestUnknownKeyIsNamed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file string
		body string
		want string // the key the error must name
	}{
		{"agent.yaml", "apiVersion: bonnie.dev/v0alpha\ntitle: x\ntitles: y\n", "titles"},
		{"agent.toml", "apiVersion = \"bonnie.dev/v0alpha\"\nmodelname = \"m\"\n", "modelname"},
		{"agent.json", "{\"apiVersion\":\"bonnie.dev/v0alpha\",\"instruktions\":\"a.md\"}", "instruktions"},
		{"agent.yaml", "apiVersion: bonnie.dev/v0alpha\nsandbox:\n  kind: docker\n  netwrok:\n    mode: deny-all\n", "sandbox.netwrok"},
	}
	for _, c := range cases {
		t.Run(c.file+" "+c.want, func(t *testing.T) {
			t.Parallel()
			dir := writeTree(t, map[string]string{c.file: c.body})
			_, _, err := Load(dir)
			if err == nil {
				t.Fatal("want an error for an unknown key")
			}
			if !errors.Is(err, ErrUnknownKey) {
				t.Fatalf("err = %v, want ErrUnknownKey", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error does not name the key %q: %v", c.want, err)
			}
			if !strings.Contains(err.Error(), c.file) {
				t.Fatalf("error does not name the file: %v", err)
			}
		})
	}
}

func TestAPIVersionIsChecked(t *testing.T) {
	t.Parallel()
	t.Run("unknown version", func(t *testing.T) {
		t.Parallel()
		dir := writeTree(t, map[string]string{"agent.yaml": "apiVersion: bonnie.dev/v9\n"})
		_, _, err := Load(dir)
		if !errors.Is(err, ErrUnknownAPIVersion) {
			t.Fatalf("err = %v, want ErrUnknownAPIVersion", err)
		}
		if !strings.Contains(err.Error(), APIVersion) {
			t.Fatalf("error does not name the supported version: %v", err)
		}
	})
	t.Run("missing version", func(t *testing.T) {
		t.Parallel()
		dir := writeTree(t, map[string]string{"agent.yaml": "title: x\n"})
		if _, _, err := Load(dir); !errors.Is(err, ErrUnknownAPIVersion) {
			t.Fatalf("err = %v, want ErrUnknownAPIVersion", err)
		}
	})
}

func TestTwoManifestsInOneRootIsRefused(t *testing.T) {
	t.Parallel()
	dir := writeTree(t, map[string]string{
		"agent.yaml": "apiVersion: bonnie.dev/v0alpha\n",
		"agent.toml": "apiVersion = \"bonnie.dev/v0alpha\"\n",
	})
	_, _, err := Load(dir)
	if !errors.Is(err, ErrAmbiguousManifest) {
		t.Fatalf("err = %v, want ErrAmbiguousManifest", err)
	}
	// The refusal names both files, so the operator can delete the right one.
	for _, name := range []string{"agent.yaml", "agent.toml"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error does not name %s: %v", name, err)
		}
	}

	// --config is the escape hatch: it names one file and skips discovery.
	m, err := LoadFile(filepath.Join(dir, "agent.toml"))
	if err != nil {
		t.Fatalf("LoadFile with --config semantics: %v", err)
	}
	if m.APIVersion != APIVersion {
		t.Fatalf("apiVersion = %q", m.APIVersion)
	}
}

func TestNoManifestIsASentinel(t *testing.T) {
	t.Parallel()
	if _, _, err := Load(t.TempDir()); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("err = %v, want ErrNoManifest", err)
	}
}

// Reserved keys are refused, not accepted-and-ignored. A key that promises
// something nothing performs is the failure SECURITY.md exists to prevent.
func TestReservedKeysAreRefused(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"mcp":    "apiVersion: bonnie.dev/v0alpha\nmcp:\n  server: http://x\n",
		"skills": "apiVersion: bonnie.dev/v0alpha\nskills: skills/\n",
	}
	for key, body := range cases {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			dir := writeTree(t, map[string]string{"agent.yaml": body})
			_, _, err := Load(dir)
			if !errors.Is(err, ErrReservedKey) {
				t.Fatalf("err = %v, want ErrReservedKey", err)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("error does not name the reserved key: %v", err)
			}
		})
	}
}

func TestNetworkValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		network string // the YAML body of sandbox.network
		wantErr string // text the error must mention
	}{
		{
			name:    "allow-list needs at least one host",
			network: "    mode: allow-list\n",
			wantErr: "allow-list",
		},
		{
			name:    "allow without mode",
			network: "    allow: [\"api.example.com\"]\n",
			wantErr: "allow-list",
		},
		{
			name:    "unknown mode",
			network: "    mode: mostly-open\n",
			wantErr: "mostly-open",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			body := "apiVersion: bonnie.dev/v0alpha\nsandbox:\n  kind: docker\n  network:\n" + c.network
			dir := writeTree(t, map[string]string{"agent.yaml": body})
			_, _, err := Load(dir)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error does not mention %q: %v", c.wantErr, err)
			}
		})
	}
}

func TestUnknownSandboxKindIsRefused(t *testing.T) {
	t.Parallel()
	dir := writeTree(t, map[string]string{
		"agent.yaml": "apiVersion: bonnie.dev/v0alpha\nsandbox:\n  kind: firecracker\n",
	})
	_, _, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "firecracker") {
		t.Fatalf("err = %v, want the kind named", err)
	}
}

func TestTreeEscapingPathsAreRefused(t *testing.T) {
	t.Parallel()
	cases := []string{"instructions", "workspace"}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			for _, val := range []string{"/etc/passwd", "../../secrets.md"} {
				dir := writeTree(t, map[string]string{
					"agent.yaml": "apiVersion: bonnie.dev/v0alpha\n" + key + ": " + val + "\n",
				})
				if _, _, err := Load(dir); err == nil {
					t.Fatalf("want an error for %s: %s", key, val)
				}
			}
		})
	}
}

func TestLoadFileRejectsUnknownExtension(t *testing.T) {
	t.Parallel()
	dir := writeTree(t, map[string]string{"agent.ini": "apiVersion = bonnie.dev/v0alpha\n"})
	if _, err := LoadFile(filepath.Join(dir, "agent.ini")); err == nil {
		t.Fatal("want an error for an unsupported extension")
	}
}

func TestDefaultsAreEmptyNotFilled(t *testing.T) {
	t.Parallel()
	// The loader reports what the file says. Filling defaults is the
	// host's job, so precedence (flag over manifest over default) has one
	// home.
	dir := writeTree(t, map[string]string{"agent.yaml": "apiVersion: bonnie.dev/v0alpha\n"})
	m, path, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Instructions != "" || m.Sandbox != nil || m.Channels != nil || m.Workspace != "" {
		t.Fatalf("loader filled defaults: %+v", m)
	}
	if filepath.Base(path) != "agent.yaml" {
		t.Fatalf("path = %q", path)
	}
}
