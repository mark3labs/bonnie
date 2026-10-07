package sandbox

import (
	"context"
	"fmt"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// maxToolOutput caps what a tool hands back to the model. A command that
// prints a megabyte would otherwise fill the context window and push the
// conversation into a compaction it did not need.
const maxToolOutput = 32 * 1024

// Opener returns the sandbox for the current turn.
//
// It exists so tools can be built before a sandbox is open. The run parks and
// resumes without holding compute, and the first tool call that needs the
// sandbox is what actually opens it.
type Opener func(ctx context.Context) (Sandbox, error)

// LazyOpener returns an [Opener] that opens the sandbox on first use and
// caches only a successful open. Failed opens can be retried. Concurrent calls
// open one sandbox at a time; a caller can cancel while it waits. A run that
// never calls a sandbox tool never starts a container.
func LazyOpener(p Provider, s *runtime.Session) Opener {
	open, _ := lazyOpener(p, s)
	return open
}

// lazyOpener also returns a cleanup hook for the per-turn agent. Cleanup only
// closes a handle that was actually opened; it never forces lazy startup.
func lazyOpener(p Provider, s *runtime.Session) (Opener, func() error) {
	var (
		sb       Sandbox
		recorded bool
	)
	// The gate protects both the cached handle and its journal record. Unlike
	// a mutex, it lets a waiting caller stop when its context is canceled.
	gate := make(chan struct{}, 1)
	runID := s.RunID()
	open := func(ctx context.Context) (Sandbox, error) {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if sb == nil {
			opened, err := p.Open(ctx, runID)
			if err != nil {
				return nil, err
			}
			sb = opened
		}

		// The record of the workspace's existence. Without it a resumed
		// run cannot tell a live workspace from a pruned one, and nothing
		// can find the sandboxes of finished runs to reclaim them.
		//
		// The record is retried on every call until the journal takes it,
		// and a call that could not land it fails: the model sees the
		// error, retries the tool, and the record lands when the journal
		// recovers. A bookkeeping failure must not be silent, and it must
		// not be permanent.
		if !recorded {
			if rerr := s.RecordSandboxOpen(ctx, p.Name(), sb.ID()); rerr != nil {
				return nil, fmt.Errorf("bonnie: sandbox: record open: %w", rerr)
			}
			recorded = true
		}
		return sb, nil
	}
	close := func() error {
		gate <- struct{}{}
		defer func() { <-gate }()
		if sb == nil {
			return nil
		}
		return sb.Close()
	}
	return open, close
}

// Tools returns the model-facing tools that work inside a sandbox: shell,
// read_file, write_file, and list_files.
//
// The tools run in the BONNIE process and proxy into the sandbox. The model
// never holds a sandbox handle and never sees a credential — it drives the
// work through tool calls and reads the results. Every sandbox call therefore
// travels the same journalling and approval path as any other tool.
func Tools(open Opener) []kit.Tool {
	return []kit.Tool{
		shellTool(open),
		readFileTool(open),
		writeFileTool(open),
		listFilesTool(open),
	}
}

// shellTool selects a shell inside the sandbox before it runs the command.
// Detection stays lazy and repeats on each call, because tools can install Bash.
func shellTool(open Opener) kit.Tool {
	type input struct {
		Command string `json:"command" description:"The shell command to run."`
		Dir     string `json:"dir,omitempty" description:"Working directory, relative to /workspace."`
	}
	return kit.NewTool("shell",
		"Run a shell command inside the isolated sandbox. Prefer Bash; if Bash is "+
			"not available, use sh (POSIX syntax). Each result reports the selected shell. "+
			"The working directory is /workspace. Returns stdout, stderr, and the exit code.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			sb, err := open(ctx)
			if err != nil {
				return unavailable(err), nil
			}
			// Probe in the sandbox, not on the host. Do not use a failed
			// command as the probe: retrying it could repeat an external effect.
			probe, err := sb.Exec(ctx, Command{Args: []string{"sh", "-c", "command -v bash >/dev/null 2>&1"}, Dir: in.Dir})
			if err != nil {
				return kit.ErrorResult(fmt.Sprintf("sandbox error: detect shell: %v", err)), nil
			}
			if probe.ExitCode != 0 && probe.ExitCode != 1 && probe.ExitCode != 127 {
				return kit.ErrorResult("sandbox error: detect shell: " + renderResult(probe)), nil
			}
			shell, label := "bash", "Shell: bash"
			if !probe.OK() {
				shell, label = "sh", "Shell: sh (Bash unavailable)"
			}
			cmd := Command{Args: []string{shell, "-lc", in.Command}, Dir: in.Dir}

			res, err := sb.Exec(ctx, cmd)
			if err != nil {
				// The command did not run. That is a fault in BONNIE or the
				// backend, not something the model did wrong, so say so
				// plainly rather than let it retry a broken command.
				return kit.ErrorResult(fmt.Sprintf("%s\nsandbox error: %v", label, err)), nil
			}
			return kit.ToolOutput{
				Content: label + "\n" + renderResult(res),
				// A non-zero exit is information for the model, not a
				// transport failure: it must see the error text to fix it.
				IsError: !res.OK(),
			}, nil
		})
}

// readFileTool reads a file out of the sandbox.
func readFileTool(open Opener) kit.Tool {
	type input struct {
		Path string `json:"path" description:"File path. Relative paths resolve from /workspace."`
	}
	return kit.NewTool("read_file",
		"Read a text file or image (PNG, JPEG, GIF, WebP) from the sandbox. "+
			"Images are returned as viewable images and resized to fit model limits.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			sb, err := open(ctx)
			if err != nil {
				return unavailable(err), nil
			}
			data, err := sb.ReadFile(ctx, in.Path)
			if err != nil {
				return kit.ErrorResult(err.Error()), nil
			}
			if isImageFile(data, in.Path) {
				return readSandboxImage(ctx, data, in.Path)
			}
			return kit.TextResult(truncate(string(data))), nil
		})
}

// writeFileTool writes a file into the sandbox.
func writeFileTool(open Opener) kit.Tool {
	type input struct {
		Path    string `json:"path" description:"File path. Relative paths resolve from /workspace."`
		Content string `json:"content" description:"The full file content."`
	}
	return kit.NewTool("write_file",
		"Write a text file into the sandbox, replacing it when it exists.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			sb, err := open(ctx)
			if err != nil {
				return unavailable(err), nil
			}
			if err := sb.WriteFile(ctx, in.Path, []byte(in.Content)); err != nil {
				return kit.ErrorResult(err.Error()), nil
			}
			return kit.TextResult(fmt.Sprintf("Wrote %d bytes to %s.",
				len(in.Content), Resolve(in.Path))), nil
		})
}

// listFilesTool lists a directory in the sandbox.
func listFilesTool(open Opener) kit.Tool {
	type input struct {
		Path string `json:"path,omitempty" description:"Directory to list. Defaults to /workspace."`
	}
	return kit.NewTool("list_files",
		"List the files in a sandbox directory.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			sb, err := open(ctx)
			if err != nil {
				return unavailable(err), nil
			}
			// Backends map Dir into their command namespace. They do not map
			// argv paths: /workspace in argv is not a host workspace path.
			// Keep the path out of shell text and list the mapped directory.
			res, err := sb.Exec(ctx, Command{
				Args: []string{"ls", "-la", "--", "."},
				Dir:  Resolve(in.Path),
			})
			if err != nil {
				return kit.ErrorResult(fmt.Sprintf("sandbox error: %v", err)), nil
			}
			if !res.OK() {
				return kit.ErrorResult(res.Output()), nil
			}
			return kit.TextResult(truncate(res.Stdout)), nil
		})
}

// unavailable turns a failure to open the sandbox into a model-readable error.
func unavailable(err error) kit.ToolOutput {
	return kit.ErrorResult(fmt.Sprintf(
		"the sandbox is not available, so this tool cannot run: %v", err))
}

// renderResult formats a finished command for the model. The exit code is
// stated only when it is non-zero, so a successful command reads as plain
// output.
func renderResult(res *Result) string {
	var b strings.Builder
	if out := strings.TrimRight(res.Stdout, "\n"); out != "" {
		b.WriteString(out)
	}
	if errOut := strings.TrimRight(res.Stderr, "\n"); errOut != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("stderr: ")
		b.WriteString(errOut)
	}
	if !res.OK() {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "(exit code %d)", res.ExitCode)
	}
	if b.Len() == 0 {
		return "(no output)"
	}
	return truncate(b.String())
}

// truncate caps tool output and says how much it dropped, so the model knows
// the text is partial instead of silently reasoning about a cut-off file.
func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + fmt.Sprintf("\n\n... truncated, %d bytes total", len(s))
}
