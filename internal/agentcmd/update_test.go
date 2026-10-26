package agentcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestUpdateSources(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"", "https://example.com/a?token=secret", "a path/models.json", "embedded"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			calls := 0
			cmd := newUpdateCommand(func(got context.Context, opts kit.UpdateModelsOptions) (kit.UpdateModelsResult, error) {
				calls++
				if got != ctx || opts.Source != source {
					t.Fatalf("context/source not forwarded: %v", opts)
				}
				return kit.UpdateModelsResult{Status: kit.UpdateModelsStatusUpdated, ProviderCount: 2, ModelCount: 3}, nil
			})
			var out bytes.Buffer
			cmd.SetOut(&out)
			args := []string{"models"}
			if source != "" {
				args = append(args, source)
			}
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || out.String() != "Model database updated: 2 providers, 3 models.\nRestart running agents to use the model database.\n" {
				t.Fatalf("calls=%d output=%q", calls, out.String())
			}
		})
	}
}

func TestUpdateResults(t *testing.T) {
	t.Parallel()
	failure := errors.New("failure")
	for _, tc := range []struct {
		name      string
		result    kit.UpdateModelsResult
		actionErr error
		writer    io.Writer
		cancel    bool
		want      string
		err       error
	}{
		{name: "not modified", result: kit.UpdateModelsResult{Status: kit.UpdateModelsStatusNotModified}, want: "Model database is already up to date."},
		{name: "reset", result: kit.UpdateModelsResult{Status: kit.UpdateModelsStatusReset}, want: "Model database reset to embedded version."},
		{name: "unknown", want: "unexpected result status"},
		{name: "SDK", actionErr: failure, err: failure},
		{name: "cancelled SDK", actionErr: context.Canceled, err: context.Canceled},
		{name: "deadline SDK", actionErr: context.DeadlineExceeded, err: context.DeadlineExceeded},
		{name: "pre cancelled", cancel: true, err: context.Canceled},
		{name: "writer", result: kit.UpdateModelsResult{Status: kit.UpdateModelsStatusReset}, writer: updateFailWriter{failure}, err: failure, want: "output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			cmd := newUpdateCommand(func(context.Context, kit.UpdateModelsOptions) (kit.UpdateModelsResult, error) {
				calls++
				return tc.result, tc.actionErr
			})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			if tc.writer != nil {
				cmd.SetOut(tc.writer)
			}
			cmd.SetArgs([]string{"models"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			err := cmd.ExecuteContext(ctx)
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("error = %v", err)
			}
			if tc.err != nil || tc.name == "unknown" {
				if err == nil || !strings.Contains(err.Error(), "bonnie: update models") || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) {
					t.Fatalf("error = %v", err)
				}
				if out.Len() != 0 {
					t.Fatal(out.String())
				}
			} else if err != nil || out.String() != tc.want+"\nRestart running agents to use the model database.\n" {
				t.Fatalf("%v: %q", err, out.String())
			}
			wantCalls := 1
			if tc.cancel {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("calls = %d", calls)
			}
		})
	}
}

type updateFailWriter struct{ err error }

func (w updateFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestUpdateNoAction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		fail bool
	}{
		{nil, false}, {[]string{"--help"}, false}, {[]string{"update"}, false},
		{[]string{"update", "--help"}, false}, {[]string{"update", "models", "--help"}, false},
		{[]string{"update", "models", "one", "two"}, true},
		{[]string{"update", "models", "--unknown"}, true}, {[]string{"update", "unknown"}, true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			root := &cobra.Command{Use: "my-agent", SilenceErrors: true, SilenceUsage: true}
			root.AddCommand(newUpdateCommand(func(context.Context, kit.UpdateModelsOptions) (kit.UpdateModelsResult, error) {
				t.Fatal("updater called")
				return kit.UpdateModelsResult{}, nil
			}))
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetArgs(tc.args)
			if err := root.ExecuteContext(t.Context()); (err != nil) != tc.fail {
				t.Fatalf("error = %v", err)
			}
			if len(tc.args) == 3 && tc.args[2] == "--help" && !strings.Contains(out.String(), "my-agent update models embedded") {
				t.Fatal(out.String())
			}
		})
	}
}
