package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// MicrosandboxVersion is the release BONNIE installs when msb is absent.
// Existing installations are neither upgraded nor checked against this version.
const MicrosandboxVersion = "v0.7.7"

type msbRelease struct {
	asset string
	hash  string
}

// These hashes pin the release archives, not a checksum file fetched at startup.
var msbReleases = map[string]msbRelease{
	"linux/amd64":  {"microsandbox-linux-x86_64.tar.gz", "b3cc4a5e3f52dfdd938a6f67ac4a9a959ddfe304bab56de4964044b8613f01bb"},
	"linux/arm64":  {"microsandbox-linux-aarch64.tar.gz", "8997b1ea76de58689fb6d0fa7b32af6fbe8cbc24612da40b168a5b433c7d8318"},
	"darwin/arm64": {"microsandbox-darwin-aarch64.tar.gz", "eed5faa16217ad375ad9e4ab5e819656baeab8ccde0d3ab7e79c4af49319d403"},
}

// EnsureInstalled uses msb from PATH, then dir/msb or dir/microsandbox/bin/msb.
// If none exists and download is true, it installs the tracked release in dir.
// Call this at startup, before Available or Open. A custom binary is left alone.
// Existing binaries are not replaced, even when download is false.
func (p *MicrosandboxProvider) EnsureInstalled(ctx context.Context, dir string, download bool) error {
	if p.bin != "msb" {
		return nil
	}
	if _, err := exec.LookPath("msb"); err == nil {
		return nil
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("bonnie: microsandbox install path: %w", err)
	}
	for _, path := range []string{filepath.Join(root, "msb"), filepath.Join(root, "microsandbox", "bin", "msb")} {
		if _, err := exec.LookPath(path); err == nil {
			p.bin = path
			return nil
		}
	}
	if !download {
		return fmt.Errorf("%w: msb is absent and automatic download is disabled", ErrUnavailable)
	}
	release, ok := msbReleases[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return fmt.Errorf("%w: no tracked msb release for %s/%s", ErrUnavailable, runtime.GOOS, runtime.GOARCH)
	}
	url := "https://github.com/superradcompany/microsandbox/releases/download/" + MicrosandboxVersion + "/" + release.asset
	path, err := installMsb(ctx, root, url, release.hash)
	if err != nil {
		return fmt.Errorf("bonnie: install microsandbox %s: %w", MicrosandboxVersion, err)
	}
	p.bin = path
	return nil
}

func installMsb(ctx context.Context, root, url, hash string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("bonnie: create install directory: %w", err)
	}
	stage, err := os.MkdirTemp(root, ".msb-install-")
	if err != nil {
		return "", fmt.Errorf("bonnie: create install stage: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := downloadMsb(ctx, filepath.Join(stage, "bundle"), url, hash); err != nil {
		return "", err
	}
	if err := extractMsb(filepath.Join(stage, "bundle"), stage); err != nil {
		return "", err
	}
	if err := os.Remove(filepath.Join(stage, "bundle")); err != nil {
		return "", fmt.Errorf("bonnie: remove install archive: %w", err)
	}
	target := filepath.Join(root, "microsandbox")
	if err := os.Rename(stage, target); err != nil {
		// A concurrent startup can finish first. Never replace its install.
		if _, check := exec.LookPath(filepath.Join(target, "bin", "msb")); check != nil {
			return "", fmt.Errorf("bonnie: publish microsandbox install: %w", err)
		}
	}
	return filepath.Join(target, "bin", "msb"), nil
}

func downloadMsb(ctx context.Context, path, url, hash string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("bonnie: create release request: %w", err)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("bonnie: download microsandbox: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bonnie: download microsandbox: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("bonnie: create release archive: %w", err)
	}
	h := sha256.New()
	const limit = 256 << 20
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("bonnie: save release archive: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("bonnie: close release archive: %w", closeErr)
	}
	if n > limit || fmt.Sprintf("%x", h.Sum(nil)) != hash {
		return fmt.Errorf("bonnie: release archive exceeds size limit or SHA-256 does not match")
	}
	return nil
}

func extractMsb(archive, stage string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("bonnie: open release archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("bonnie: read release gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var binary, library bool
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("bonnie: read release tar: %w", err)
		}
		// The tracked bundles have two regular files at the archive root.
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != h.Name || strings.ContainsAny(h.Name, "\\/") || h.Size < 0 || h.Size > 256<<20 {
			return fmt.Errorf("bonnie: unsafe release archive member %q", h.Name)
		}
		dir, mode := "lib", os.FileMode(0o644)
		switch {
		case h.Name == "msb" && !binary:
			binary, dir, mode = true, "bin", 0o755
		case strings.HasPrefix(h.Name, "libkrunfw.") && !library:
			library = true
		default:
			return fmt.Errorf("bonnie: unexpected release archive member %q", h.Name)
		}
		if err := os.MkdirAll(filepath.Join(stage, dir), 0o700); err != nil {
			return fmt.Errorf("bonnie: create runtime directory: %w", err)
		}
		out, err := os.OpenFile(filepath.Join(stage, dir, h.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return fmt.Errorf("bonnie: create runtime file: %w", err)
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("bonnie: extract runtime file: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("bonnie: close runtime file: %w", closeErr)
		}
		if dir == "lib" {
			alias := "libkrunfw.dylib"
			if strings.Contains(h.Name, ".so.") {
				_, version, _ := strings.Cut(h.Name, ".so.")
				abi, _, _ := strings.Cut(version, ".")
				if err := os.Symlink(h.Name, filepath.Join(stage, dir, "libkrunfw.so."+abi)); err != nil {
					return fmt.Errorf("bonnie: link runtime ABI: %w", err)
				}
				alias = "libkrunfw.so"
			}
			if err := os.Symlink(h.Name, filepath.Join(stage, dir, alias)); err != nil {
				return fmt.Errorf("bonnie: link runtime library: %w", err)
			}
		}
	}
	if !binary || !library {
		return fmt.Errorf("bonnie: release archive is missing msb or libkrunfw")
	}
	return nil
}
