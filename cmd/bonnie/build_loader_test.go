package main

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/treetest"
)

// dynlinkSource makes a tree dynamically linked with CGO disabled, the way
// purego does (examples/self-contained-agent loads libffi through it). The
// binary is never run.
const dynlinkSource = `package main

import _ "unsafe"

//go:cgo_import_dynamic libc_getpid getpid "libc.so.6"
//go:cgo_import_dynamic _ _ "libc.so.6"
`

// interpreter returns the ELF interpreter of the binary at path.
func interpreter(t *testing.T, path string) string {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			b := make([]byte, p.Filesz)
			if _, err := p.ReadAt(b, 0); err != nil {
				t.Fatal(err)
			}
			return strings.TrimRight(string(b), "\x00")
		}
	}
	t.Fatalf("%s has no interpreter: it is not dynamically linked", path)
	return ""
}

// dynlinkTree is a hermetic tree that links dynamically.
func dynlinkTree(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the ELF interpreter applies to Linux only")
	}
	if _, ok := linuxLoaders[runtime.GOARCH]; !ok {
		t.Skipf("no standard loader for GOARCH %s", runtime.GOARCH)
	}
	root := buildHermeticTree(t)
	if err := os.WriteFile(filepath.Join(root, "dynlink.go"), []byte(dynlinkSource), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestBuildUsesStandardLoader is the guard for a defect: the Go toolchain from
// nixpkgs names a loader in /nix/store, and an agent built with it stopped on
// a Debian host with "cannot execute: required file not found". The binary
// must name the loader of the target's glibc (or musl) system, whatever the
// toolchain default is.
func TestBuildUsesStandardLoader(t *testing.T) {
	t.Parallel()
	root := dynlinkTree(t)
	bin := filepath.Join(root, "agent")
	if err := runBuild(root, buildOpts{output: bin, env: treetest.BuildEnv()}); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	want := linuxLoader("linux", runtime.GOARCH, fileExists)
	if got := interpreter(t, bin); got != want {
		t.Errorf("interpreter = %q, want %q", got, want)
	}
}

// TestBuildNativeLoaderKeepsToolchainDefault proves --native-loader gives the
// interpreter of a plain go build, for a NixOS host without nix-ld.
func TestBuildNativeLoaderKeepsToolchainDefault(t *testing.T) {
	t.Parallel()
	root := dynlinkTree(t)
	bin := filepath.Join(root, "agent")
	env := treetest.BuildEnv()
	if err := runBuild(root, buildOpts{output: bin, nativeLoader: true, env: env}); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	plain := filepath.Join(root, "plain")
	cmd := exec.Command("go", "build", "-o", plain, ".")
	cmd.Dir = root
	cmd.Env = staticBuildEnv(env)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if got, want := interpreter(t, bin), interpreter(t, plain); got != want {
		t.Errorf("interpreter = %q, want the toolchain default %q", got, want)
	}
}

func TestLinuxLoader(t *testing.T) {
	t.Parallel()
	none := func(string) bool { return false }
	muslOnly := func(p string) bool { return strings.Contains(p, "musl") }
	for _, tc := range []struct {
		goos, goarch string
		exists       func(string) bool
		want         string
	}{
		{"linux", "amd64", none, "/lib64/ld-linux-x86-64.so.2"},
		{"linux", "arm64", none, "/lib/ld-linux-aarch64.so.1"},
		{"linux", "amd64", muslOnly, "/lib/ld-musl-x86_64.so.1"},
		{"linux", "mips", muslOnly, "/lib/ld.so.1"},
		{"linux", "wasm", none, ""},
		{"android", "arm64", none, ""},
		{"darwin", "arm64", none, ""},
	} {
		if got := linuxLoader(tc.goos, tc.goarch, tc.exists); got != tc.want {
			t.Errorf("linuxLoader(%s, %s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// TestLoaderEnv proves the target comes from the build environment, not from
// the host, and that a GO_LDSO the caller sets wins.
func TestLoaderEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := staticBuildEnv(nil)

	env, err := loaderEnv(dir, append(slices.Clone(base), "GOOS=linux", "GOARCH=arm64"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "GO_LDSO=/lib/ld-linux-aarch64.so.1") {
		t.Errorf("arm64 target: GO_LDSO not set to the aarch64 loader: %q", env[len(env)-1])
	}

	env, err = loaderEnv(dir, append(slices.Clone(base), "GOOS=darwin", "GOARCH=arm64"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range env {
		if strings.HasPrefix(item, "GO_LDSO=") {
			t.Errorf("darwin target: unexpected %s", item)
		}
	}

	own := append(slices.Clone(base), "GOOS=linux", "GOARCH=amd64", "GO_LDSO=/opt/ld.so")
	env, err = loaderEnv(dir, own)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(env, own) {
		t.Errorf("caller GO_LDSO changed the environment: %q", env)
	}
}
