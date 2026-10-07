package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// PATH changes cannot run in parallel. Existing binaries must never be updated,
// even if the file is an old release or automatic download is disabled.
func TestMicrosandboxInstallExisting(t *testing.T) {
	pathDir, root := t.TempDir(), t.TempDir()
	t.Setenv("PATH", pathDir)
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "msb"))
	write(filepath.Join(pathDir, "msb"))
	p := Microsandbox()
	if err := p.EnsureInstalled(context.Background(), root, false); err != nil {
		t.Fatal(err)
	}
	if p.bin != "msb" {
		t.Fatalf("PATH must win: %s", p.bin)
	}
	if err := os.Remove(filepath.Join(pathDir, "msb")); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureInstalled(context.Background(), root, false); err != nil {
		t.Fatal(err)
	}
	if p.bin != filepath.Join(root, "msb") {
		t.Fatalf("local binary not selected: %s", p.bin)
	}
	p = Microsandbox(WithMicrosandboxBinary("custom-msb"))
	if err := p.EnsureInstalled(context.Background(), root, true); err != nil {
		t.Fatal(err)
	}
	if p.bin != "custom-msb" {
		t.Fatal("custom binary changed")
	}
	p = Microsandbox()
	if err := p.EnsureInstalled(context.Background(), t.TempDir(), false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func msbBundle(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		data := []byte("runtime fixture")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestInstallMsb(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		members []string
		badHash bool
		wantErr bool
	}{
		{"valid", []string{"msb", "libkrunfw.so.5.6.1"}, false, false},
		{"checksum", []string{"msb", "libkrunfw.so.5.6.1"}, true, true},
		{"traversal", []string{"../msb", "libkrunfw.so.5.6.1"}, false, true},
		{"missing-library", []string{"msb"}, false, true},
		{"duplicate", []string{"msb", "msb", "libkrunfw.so.5.6.1"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := msbBundle(t, tc.members...)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write(data); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			hash := fmt.Sprintf("%x", sha256.Sum256(data))
			if tc.badHash {
				hash = "wrong"
			}
			root := t.TempDir()
			path, err := installMsb(context.Background(), root, srv.URL, hash)
			if (err != nil) != tc.wantErr {
				t.Fatalf("got %v", err)
			}
			if tc.wantErr {
				entries, err := os.ReadDir(root)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("partial install: %v", entries)
				}
				return
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&0o111 == 0 {
				t.Fatal("msb is not executable")
			}
			if _, err := os.Stat(filepath.Join(root, "microsandbox", "lib", "libkrunfw.so.5")); err != nil {
				t.Fatal(err)
			}
			// A second publication must reuse, not replace, the installed binary.
			if _, err := installMsb(context.Background(), root, srv.URL, hash); err != nil {
				t.Fatal(err)
			}
			got, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(info, got) {
				t.Fatal("existing install was replaced")
			}
		})
	}
}

func TestInstallMsbCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := installMsb(ctx, t.TempDir(), "https://example.invalid/msb", "unused")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
