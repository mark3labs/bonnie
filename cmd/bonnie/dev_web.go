package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mark3labs/bonnie/client"
)

// launchDevWeb opens the browser only after the HTTP health route is ready.
// A browser failure does not stop the dev loop: print the URL for manual use.
func launchDevWeb(ctx context.Context, base string, output io.Writer, open func(context.Context, string) error) error {
	if err := waitForHealth(ctx, base); err != nil {
		return err
	}
	url := strings.TrimRight(base, "/") + "/web/"
	if err := open(ctx, url); err != nil {
		if _, writeErr := fmt.Fprintf(output, "bonnie: dev: cannot open browser: %v\nOpen %s\n", err, url); writeErr != nil {
			return fmt.Errorf("bonnie: dev: print browser URL: %w", writeErr)
		}
	}
	return nil
}

// waitForHealth checks HTTP readiness, not just an open TCP listener.
func waitForHealth(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c := client.New(base, client.WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}))
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		health, err := c.Health(ctx)
		if err == nil && health.OK {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("bonnie: dev: wait for health at %s: %w", base, ctx.Err())
		case <-ticker.C:
		}
	}
}

// openBrowser uses the operating system's URL opener. The context cancels it
// when the dev loop stops.
func openBrowser(ctx context.Context, url string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{url}
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		command, args = "xdg-open", []string{url}
	}
	if err := exec.CommandContext(ctx, command, args...).Run(); err != nil {
		return fmt.Errorf("bonnie: dev: browser: %w", err)
	}
	return nil
}
