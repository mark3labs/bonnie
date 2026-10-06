package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// isImageFile uses both signature and extension. A damaged image must reach
// Kit's validator rather than become binary text in the model's context.
func isImageFile(data []byte, path string) bool {
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// readSandboxImage delegates validation, budgets and resizing to Kit. Only
// bytes already read through the sandbox reach the host disk. The caller's
// path is never used as a host path, and private files are removed before return.
//
// TODO(kit): Replace the temporary file with a public byte-based image helper
// when https://github.com/mark3labs/kit/issues/162 is resolved. WithFileSystem
// cannot be used here: Kit routes images to the host disk, not that interface.
func readSandboxImage(ctx context.Context, data []byte, path string) (out kit.ToolOutput, err error) {
	if err := ctx.Err(); err != nil {
		return kit.ToolOutput{}, err
	}
	dir, err := os.MkdirTemp("", "bonnie-read-image-")
	if err != nil {
		return kit.ToolOutput{}, fmt.Errorf("bonnie: sandbox: create image directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			out = kit.ToolOutput{}
			err = fmt.Errorf("bonnie: sandbox: remove image directory: %w", cleanupErr)
		}
	}()
	// Keep only the extension for Kit's damaged-image detection. No user
	// directory or filename can redirect this write outside the private folder.
	staged := filepath.Join(dir, "image"+strings.ToLower(filepath.Ext(path)))
	if err := os.WriteFile(staged, data, 0o600); err != nil {
		return kit.ToolOutput{}, fmt.Errorf("bonnie: sandbox: stage image: %w", err)
	}
	input, err := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: staged})
	if err != nil {
		return kit.ToolOutput{}, fmt.Errorf("bonnie: sandbox: encode image read: %w", err)
	}
	res, err := kit.NewReadTool().Run(ctx, kit.LLMToolCall{Input: string(input)})
	if err != nil {
		return kit.ToolOutput{}, fmt.Errorf("bonnie: sandbox: read image: %w", err)
	}
	// Kit's summary must show the sandbox path, never the private host path.
	content := strings.ReplaceAll(res.Content, staged, path)
	if res.IsError {
		return kit.ErrorResult(content), nil
	}
	return kit.ImageResult(content, res.Data, res.MediaType), nil
}
