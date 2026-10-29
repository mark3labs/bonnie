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
//
// treetest.BuildEnv sets GOFLAGS=-ldflags=-w. The binary must also have no
// DWARF: the -I that bonnie build adds must not replace the GOFLAGS -ldflags.
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
	f, err := elf.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if f.Section(".debug_info") != nil {
		t.Error("binary has DWARF: the -ldflags=-w in GOFLAGS was lost")
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

// TestLoaderArgs proves the target comes from the build environment, not
// from the host, that the GOFLAGS linker flags are kept, and that a -I the
// caller sets wins.
func TestLoaderArgs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := slices.DeleteFunc(staticBuildEnv(nil), func(item string) bool {
		return strings.HasPrefix(item, "GOFLAGS=")
	})
	for _, tc := range []struct {
		name string
		env  []string
		want []string
	}{
		{"arm64 target", []string{"GOOS=linux", "GOARCH=arm64"},
			[]string{"-ldflags=-I /lib/ld-linux-aarch64.so.1"}},
		{"darwin target", []string{"GOOS=darwin", "GOARCH=arm64"}, nil},
		{"GOFLAGS ldflags kept", []string{"GOOS=linux", "GOARCH=arm64", "GOFLAGS=-mod=mod '-ldflags=-s -w'"},
			[]string{"-ldflags=-s -w -I /lib/ld-linux-aarch64.so.1"}},
		{"caller -I wins", []string{"GOOS=linux", "GOARCH=arm64", "GOFLAGS=-ldflags=-I=/opt/ld.so"}, nil},
	} {
		got, err := loaderArgs(dir, append(slices.Clone(base), tc.env...))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: loaderArgs = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGoflagsLdflags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ goflags, want string }{
		{"", ""},
		{"-mod=mod", ""},
		{"-ldflags=-w", "-w"},
		{"--ldflags=-w", "-w"},
		{"-ldflags=all=-w", "-w"},
		{`-mod=mod "-ldflags=-s -w"`, "-s -w"},
		{"-ldflags=-w -ldflags=-s", "-s"},
	} {
		if got := goflagsLdflags([]string{"GOFLAGS=" + tc.goflags}); got != tc.want {
			t.Errorf("goflagsLdflags(%q) = %q, want %q", tc.goflags, got, tc.want)
		}
	}
}
