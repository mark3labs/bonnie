//go:build linux

package sandbox

import (
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/bonnie/internal/discoveryio"
)

// Release binaries carry this fixed-operation entry point. It runs before
// application main, does not load configuration, and never executes scripts.
func init() {
	if len(os.Args) < 2 || os.Args[1] != "--bonnie-discovery-helper-v1" {
		return
	}
	if err := discoveryio.Serve(os.Args[2:], os.Stdout); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

type guestDiscovery struct {
	mu     sync.Mutex
	helper string
}

func (c *cliSandbox) installDiscovery(ctx context.Context) (string, error) {
	c.discovery.mu.Lock()
	defer c.discovery.mu.Unlock()
	if c.discovery.helper != "" {
		return c.discovery.helper, nil
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return "", fmt.Errorf("bonnie: unsupported discovery helper platform")
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Release archives include a small architecture-matched helper. A static
	// custom agent can use its own fixed-operation entry point instead.
	packaged := filepath.Join(filepath.Dir(executable), "bonnie-discovery-"+runtime.GOARCH)
	if info, err := os.Lstat(packaged); err == nil && info.Mode().IsRegular() {
		executable = packaged
	}
	binary, err := elf.Open(executable)
	if err != nil {
		return "", fmt.Errorf("bonnie: discovery helper requires a static Linux binary: %w", err)
	}
	dynamic := false
	for _, program := range binary.Progs {
		if program.Type == elf.PT_INTERP {
			dynamic = true
		}
	}
	_ = binary.Close()
	if dynamic {
		return "", fmt.Errorf("bonnie: remote discovery requires a CGO_ENABLED=0 static BONNIE/agent binary")
	}
	file, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 256<<20+1))
	if err != nil {
		return "", err
	}
	if len(data) > 256<<20 {
		return "", fmt.Errorf("bonnie: discovery helper binary exceeds install limit")
	}
	// A fresh private directory cannot have a pre-existing symlink leaf. No
	// repository input is interpreted by the installation command.
	result, err := c.Exec(ctx, Command{Args: []string{"mktemp", "-d", "/tmp/.bonnie-discovery-XXXXXXXXXXXX"}, Dir: "/tmp"})
	if err != nil {
		return "", err
	}
	if !result.OK() {
		return "", fmt.Errorf("bonnie: create private helper directory")
	}
	directory := strings.TrimSpace(result.Stdout)
	if !strings.HasPrefix(directory, "/tmp/.bonnie-discovery-") || strings.ContainsAny(strings.TrimPrefix(directory, "/tmp/"), "/\x00\r\n") {
		return "", fmt.Errorf("bonnie: invalid helper installation response")
	}
	helper := directory + "/helper"
	if err := c.WriteFile(ctx, helper, data); err != nil {
		return "", fmt.Errorf("bonnie: install discovery helper: %w", err)
	}
	result, err = c.Exec(ctx, Command{Args: []string{"chmod", "500", helper}, Dir: "/tmp"})
	if err != nil {
		return "", err
	}
	if !result.OK() {
		return "", fmt.Errorf("bonnie: protect discovery helper")
	}
	c.discovery.helper = helper
	return helper, nil
}
func (c *cliSandbox) discoveryCall(ctx context.Context, operation, p string, limit int64) (discoveryio.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	helper, err := c.installDiscovery(ctx)
	if err != nil {
		return discoveryio.Response{}, err
	}
	result, err := c.Exec(ctx, Command{Args: []string{helper, "--bonnie-discovery-helper-v1", "1", WorkDir, operation, p, strconv.FormatInt(limit, 10)}, Dir: WorkDir, outputLimit: discoveryio.MaxProtocolBytes + 4096})
	if err != nil {
		return discoveryio.Response{}, err
	}
	if !result.OK() {
		return discoveryio.Response{}, fmt.Errorf("bonnie: unsupported guest helper platform or execution failure")
	}
	if len(result.Stdout) > discoveryio.MaxProtocolBytes || len(result.Stderr) > 4096 {
		return discoveryio.Response{}, fmt.Errorf("bonnie: discovery protocol output exceeds bound")
	}
	var response discoveryio.Response
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return response, fmt.Errorf("bonnie: malformed discovery protocol: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return response, fmt.Errorf("bonnie: trailing discovery protocol data")
	}
	if response.Version != 1 || response.Error != "" {
		return response, fmt.Errorf("bonnie: discovery helper failed (%s)", response.Error)
	}
	return response, nil
}

// ListDiscovery implements secure enumeration through the trusted guest helper.
func (c *cliSandbox) ListDiscovery(ctx context.Context, p string, n int) ([]DiscoveryEntry, bool, error) {
	if n < 1 || n > 10000 {
		return nil, false, fmt.Errorf("bonnie: invalid discovery entry bound")
	}
	r, err := c.discoveryCall(ctx, "list", p, int64(n))
	if len(r.Entries) > n {
		return nil, false, fmt.Errorf("bonnie: helper exceeded entry bound")
	}
	for _, e := range r.Entries {
		if !validEntry(DirectoryEntry{Name: e.Name, Kind: e.Kind}) {
			return nil, false, fmt.Errorf("bonnie: invalid helper entry")
		}
	}
	return r.Entries, r.More, err
}

// ReadDiscovery implements bounded no-follow reads through the guest helper.
func (c *cliSandbox) ReadDiscovery(ctx context.Context, p string, n int64) (DiscoveryFile, error) {
	if n < 1 || n > 64<<20 {
		return DiscoveryFile{}, fmt.Errorf("bonnie: invalid discovery byte bound")
	}
	r, err := c.discoveryCall(ctx, "read", p, n)
	if err != nil {
		return DiscoveryFile{}, err
	}
	if r.File == nil || int64(len(r.File.Data)) > n {
		return DiscoveryFile{}, fmt.Errorf("bonnie: invalid helper file response")
	}
	return *r.File, nil
}
