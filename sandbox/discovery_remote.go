package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/mark3labs/bonnie/internal/skillfs"
)

// remoteDiscovery serializes helper setup with requests. The helper is kept
// outside the workspace and never executes repository code.
type remoteDiscovery struct {
	once   sync.Once
	gate   chan struct{}
	helper string
}

// SkillDirectory enumerates through the guest no-follow filesystem helper.
func (c *cliSandbox) SkillDirectory(ctx context.Context, p string, n int) ([]DiscoveryEntry, bool, error) {
	response, err := c.skillRequest(ctx, skillfs.Request{Version: 1, Operation: "list", Path: p, Limit: int64(n)})
	return response.Entries, response.Partial, err
}

// SkillFile reads bounded bytes through the guest filesystem helper.
func (c *cliSandbox) SkillFile(ctx context.Context, p string, n int64) (DiscoveryFile, error) {
	response, err := c.skillRequest(ctx, skillfs.Request{Version: 1, Operation: "read", Path: p, Limit: n})
	return response.File, err
}
func (c *cliSandbox) skillRequest(ctx context.Context, req skillfs.Request) (skillfs.Response, error) {
	c.discovery.once.Do(func() { c.discovery.gate = make(chan struct{}, 1) })
	select {
	case c.discovery.gate <- struct{}{}:
		defer func() { <-c.discovery.gate }()
	case <-ctx.Done():
		return skillfs.Response{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return skillfs.Response{}, err
	}
	var response skillfs.Response
	if len(req.Path) > 4096 || req.Limit < 1 || req.Limit > 16<<20 {
		return response, fmt.Errorf("bonnie: invalid discovery request")
	}
	if c.discovery.helper == "" {
		return response, fmt.Errorf("%w: workspace discovery requires backend-controlled helper storage; guest-writable installation is disabled", ErrUnavailable)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return response, err
	}
	// The protocol is argv data: microsandbox does not promise stdin forwarding.
	result, err := c.Exec(ctx, Command{Args: []string{c.discovery.helper, string(data)}, outputLimit: 24 << 20})
	if err != nil {
		return response, err
	}
	if !result.OK() || len(result.Stdout) > 24<<20 || len(result.Stderr) > 4096 {
		return response, fmt.Errorf("bonnie: invalid discovery helper response")
	}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.Version != 1 {
		return response, fmt.Errorf("bonnie: invalid discovery helper protocol")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return response, fmt.Errorf("bonnie: trailing discovery protocol data")
	}
	if response.Error != "" {
		return response, fmt.Errorf("bonnie: guest discovery I/O rejected")
	}
	if int64(len(response.File.Data)) > req.Limit || len(response.Entries) > 10000 || (req.Operation == "list" && int64(len(response.Entries)) > req.Limit) {
		return response, fmt.Errorf("bonnie: discovery helper exceeded bound")
	}
	return response, nil
}
