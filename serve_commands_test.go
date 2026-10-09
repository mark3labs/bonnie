package bonnie

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deployed commands must use the agent defaults without starting its runtime.
func TestPackagedCommands(t *testing.T) {
	t.Parallel()
	journal := filepath.Join(t.TempDir(), "journal")
	a := New(WithJournal(journal), WithAddr(":9090"), WithInstructions("missing.md"))
	for _, args := range [][]string{{"chat", "--help"}, {"runs", "list", "--help"}, {"runs", "show", "--help"}, {"runs", "inspect", "--help"}, {"schedules", "list", "--help"}, {"schedules", "show", "--help"}, {"schedules", "history", "--help"}, {"schedules", "trigger", "--help"}, {"serve", "--help"}, {"version"}} {
		cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(t.Context()); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if out.Len() == 0 {
			t.Fatalf("%v: no output", args)
		}
		if args[0] == "version" && !strings.Contains(out.String(), "bonnie ") {
			t.Fatal(out.String())
		}
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("help opened journal: %v", err)
	}
	cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
	chat, _, _ := cmd.Find([]string{"chat"})
	if got := chat.Flags().Lookup("addr").DefValue; got != ":9090" {
		t.Fatal(got)
	}
	list, _, _ := cmd.Find([]string{"runs", "list"})
	if got := list.Flags().Lookup("journal").DefValue; got != journal {
		t.Fatal(got)
	}
	schedules, _, _ := cmd.Find([]string{"schedules", "list"})
	if got := schedules.Flags().Lookup("url").DefValue; got != "http://127.0.0.1:9090" {
		t.Fatal(got)
	}
}

// The explicit serve command must apply flags and use the command context.
func TestExplicitServe(t *testing.T) {
	t.Parallel()
	a := New(WithAgentFactory(stubFactory), WithContextFiles(""), WithJournal(t.TempDir()), Quiet())
	cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
	cmd.SetArgs([]string{"serve", "--addr", "127.0.0.1:0", "--schedule-clock=false"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if a.cfg.addr != "127.0.0.1:0" || !a.cfg.scheduleClockSet || a.cfg.scheduleClock {
		t.Fatal("serve did not apply flag overrides")
	}
}

// Code options remove commands but do not affect the default serve action.
func TestPackagedCommandsDisabled(t *testing.T) {
	t.Parallel()
	a := New(WithChatCommand(false), WithRunsCommand(false), WithSchedulesCommand(false), WithVersionCommand(false))
	cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
	commands := cmd.Commands()
	if len(commands) != 1 || commands[0].Name() != "serve" || cmd.RunE == nil {
		t.Fatalf("commands = %v", commands)
	}
}

// Local inspection works without agent instructions or model credentials.
func TestPackagedRunsUsesJournal(t *testing.T) {
	t.Parallel()
	journal := t.TempDir()
	a := New(WithJournal(journal), WithInstructions("missing.md"))
	cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"runs", "list", "--json"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(journal, "journal.db")); err != nil {
		t.Fatal(err)
	}
}
