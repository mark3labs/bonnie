package agentcmd

import (
	"context"
	"io"
	"testing"

	"github.com/mark3labs/bonnie/internal/tui"
)

func TestChatCommandDefaultAndEmptyRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		args      []string
		wantError bool
	}{
		{name: "default"},
		{name: "empty", args: []string{"--run", ""}, wantError: true},
		{name: "blank", args: []string{"--run", " "}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			cmd := newChatCommand(":8080", func(_ context.Context, _ tui.Client, target string, byID bool) error {
				called = true
				if target != "tui-default" || byID {
					t.Fatalf("default target=%q byID=%v", target, byID)
				}
				return nil
			})
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if (err != nil) != tc.wantError || called == tc.wantError {
				t.Fatalf("error=%v called=%v", err, called)
			}
		})
	}
}
