package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"
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

// LazyOpener returns an [Opener] that opens the sandbox once, on first use,
// and returns the same one afterwards. A run that never calls a sandbox tool
// never starts a container.
func LazyOpener(p Provider, runID string) Opener {
	var (
		once sync.Once
		sb   Sandbox
		err  error
	)
	return func(ctx context.Context) (Sandbox, error) {
		once.Do(func() { sb, err = p.Open(ctx, runID) })
		if err != nil {
			return nil, err
		}
		return sb, nil
	}
}

// Tools returns the model-facing tools that work inside a sandbox: bash,
// read_file, write_file, and list_files.
//
// The tools run in the BONNIE process and proxy into the sandbox. The model
// never holds a sandbox handle and never sees a credential — it drives the
// work through tool calls and reads the results. Every sandbox call therefore
// travels the same journalling and approval path as any other tool.
func Tools(open Opener) []kit.Tool {
	return []kit.Tool{
		bashTool(open),
		readFileTool(open),
		writeFileTool(open),
		listFilesTool(open),
	}
}

// bashTool runs a shell command in the sandbox.
func bashTool(open Opener) kit.Tool {
	type input struct {
		Command string `json:"command" description:"The shell command to run."`
		Dir     string `json:"dir,omitempty" description:"Working directory, relative to /workspace."`
	}
	return kit.NewTool("bash",
		"Run a shell command inside the isolated sandbox. The working directory is "+
			"/workspace. Returns stdout, stderr, and the exit code.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			sb, err := open(ctx)
			if err != nil {
				return unavailable(err), nil
			}
			cmd := Shell(in.Command)
			cmd.Dir = in.Dir

			res, err := sb.Exec(ctx, cmd)
			if err != nil {
				// The command did not run. That is a fault in BONNIE or the
				// backend, not something the model did wrong, so say so
				// plainly rather than let it retry a broken command.
				return kit.ErrorResult(fmt.Sprintf("sandbox error: %v", err)), nil
			}
			return kit.ToolOutput{
				Content: renderResult(res),
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
		"Read a text file from the sandbox.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			sb, err := open(ctx)
			if err != nil {
				return unavailable(err), nil
			}
			data, err := sb.ReadFile(ctx, in.Path)
			if err != nil {
				return kit.ErrorResult(err.Error()), nil
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
			dir := Resolve(in.Path)
			res, err := sb.Exec(ctx, Command{Args: []string{"ls", "-la", "--", dir}})
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
