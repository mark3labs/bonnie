package sandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type shellToolSandbox struct {
	Sandbox
	exec     func(Command) (*Result, error)
	commands []Command
}

func (s *shellToolSandbox) Exec(_ context.Context, cmd Command) (*Result, error) {
	s.commands = append(s.commands, cmd)
	return s.exec(cmd)
}

func TestShellTool(t *testing.T) {
	t.Parallel()

	t.Run("creation is lazy", func(t *testing.T) {
		t.Parallel()
		called := false
		Tools(func(context.Context) (Sandbox, error) {
			called = true
			return nil, nil
		})
		if called {
			t.Fatal("opener called while creating tools")
		}
	})

	tests := []struct {
		name       string
		probe      *Result
		probeErr   error
		commandErr error
		exitCode   int
		calls      int
		wantShell  string
		wantError  bool
	}{
		{name: "bash preferred", probe: &Result{}, calls: 2, wantShell: "bash"},
		{name: "sh fallback exit 1", probe: &Result{ExitCode: 1}, calls: 2, wantShell: "sh"},
		{name: "sh fallback exit 127", probe: &Result{ExitCode: 127}, calls: 2, wantShell: "sh"},
		{name: "probe error blocks command", probeErr: errors.New("probe failed"), calls: 1, wantError: true},
		{name: "unexpected probe exit blocks command", probe: &Result{ExitCode: 2}, calls: 1, wantError: true},
		{name: "nonzero bash is not retried", probe: &Result{}, exitCode: 127, calls: 2, wantShell: "bash", wantError: true},
		{name: "failed bash is not retried", probe: &Result{}, commandErr: errors.New("bash failed"), calls: 2, wantShell: "bash", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			count := 0
			sb := &shellToolSandbox{exec: func(cmd Command) (*Result, error) {
				count++
				if count == 1 {
					return tc.probe, tc.probeErr
				}
				return &Result{Stdout: "ran", ExitCode: tc.exitCode}, tc.commandErr
			}}
			tool := Tools(func(context.Context) (Sandbox, error) { return sb, nil })[0]
			got, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"command":"echo hi","dir":"sub dir"}`})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.IsError != tc.wantError {
				t.Fatalf("IsError = %v, want %v; output %q", got.IsError, tc.wantError, got.Content)
			}
			if count != tc.calls {
				t.Fatalf("Exec calls = %d, want %d", count, tc.calls)
			}
			if tc.calls == 2 {
				if tc.commandErr == nil {
					label := "Shell: bash"
					if tc.wantShell == "sh" {
						label = "Shell: sh (Bash unavailable)"
					}
					if !strings.HasPrefix(got.Content, label+"\n") {
						t.Fatalf("missing shell label: %q", got.Content)
					}
				}
				if !reflect.DeepEqual(sb.commands[0], Command{Args: []string{"sh", "-c", "command -v bash >/dev/null 2>&1"}, Dir: "sub dir"}) {
					t.Fatalf("probe command = %+v", sb.commands[0])
				}
				want := Command{Args: []string{tc.wantShell, "-lc", "echo hi"}, Dir: "sub dir"}
				if !reflect.DeepEqual(sb.commands[1], want) {
					t.Fatalf("execution command = %+v, want %+v", sb.commands[1], want)
				}
			}
		})
	}

	t.Run("shell detection repeats on next invocation", func(t *testing.T) {
		t.Parallel()
		probes := []*Result{{}, {ExitCode: 1}}
		count := 0
		sb := &shellToolSandbox{exec: func(Command) (*Result, error) {
			count++
			if count%2 == 1 {
				return probes[(count-1)/2], nil
			}
			return &Result{}, nil
		}}
		tool := Tools(func(context.Context) (Sandbox, error) { return sb, nil })[0]
		for range 2 {
			if _, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"command":"true"}`}); err != nil {
				t.Fatal(err)
			}
		}
		if count != 4 || sb.commands[1].Args[0] != "bash" || sb.commands[3].Args[0] != "sh" {
			t.Fatalf("calls/selected shells = %d, %v, want 4, bash then sh", count, []string{sb.commands[1].Args[0], sb.commands[3].Args[0]})
		}
	})
}
