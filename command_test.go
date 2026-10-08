package bonnie

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// Registration runs once per command, in option order, after all built-in and
// host flags exist. A later callback can use what an earlier callback added.
func TestCommandRegistrationOrder(t *testing.T) {
	t.Parallel()
	var order []int
	var agent *Agent
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	fs.String("host", "default", "host flag")
	a := New(WithCommand(func(cmd *cobra.Command, a *Agent) {
		agent = a
		order = append(order, 1)
		for _, name := range []string{"addr", "model", "sandbox", "web", "schedule-clock", "host"} {
			if cmd.Flags().Lookup(name) == nil {
				t.Errorf("callback cannot see flag %q", name)
			}
		}
		cmd.Flags().String("custom", "first", "custom flag")
	}))
	a.Configure(WithCommand(func(cmd *cobra.Command, got *Agent) {
		order = append(order, 2)
		if got != agent || cmd.Flags().Lookup("custom") == nil {
			t.Fatal("callback lost the agent or earlier flag")
		}
	}))
	cmd := a.serveCommand(fs)
	if agent != a || !slices.Equal(order, []int{1, 2}) {
		t.Fatalf("agent = %p, order = %v", agent, order)
	}
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []int{1, 2}) {
		t.Fatalf("execution repeated registration: %v", order)
	}
}

// Parsed custom flags reach Configure through a pre-run hook. The root RunE
// remains intact, built-in flags still win, and Run uses the command context.
func TestCommandParsedConfiguration(t *testing.T) {
	t.Parallel()
	journal := filepath.Join(t.TempDir(), "journal")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var prehook bool
	a := New(WithAgentFactory(stubFactory), WithContextFiles(""),
		WithJournal("unused"), WithAddr("invalid-address"), Quiet(),
		WithCommand(func(cmd *cobra.Command, a *Agent) {
			var parsedJournal, name string
			cmd.Flags().StringVar(&parsedJournal, "journal", "", "journal directory")
			cmd.Flags().StringVar(&name, "name", "", "agent name")
			cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
				prehook = true
				if cmd.Context() != ctx || parsedJournal != journal || name != "parsed" {
					t.Fatal("pre-run hook did not receive parsed flags and context")
				}
				a.Configure(WithJournal(parsedJournal), WithName(name), WithAddr("hook-address"), WithScheduleClock(false))
				return nil
			}
		}))
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	cmd := a.serveCommand(fs)
	cmd.SetArgs(serveArgs([]string{"-journal", journal, "-name=parsed", "-addr=127.0.0.1:0"}, fs, cmd))
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !prehook || a.cfg.name != "parsed" || a.cfg.addr != "127.0.0.1:0" || a.cfg.scheduleClock {
		t.Fatalf("prehook = %v, name = %q, addr = %q", prehook, a.cfg.name, a.cfg.addr)
	}
	if _, err := os.Stat(filepath.Join(journal, "journal.db")); err != nil {
		t.Fatalf("Run did not use the parsed journal: %v", err)
	}
}

// Custom subcommands use Cobra's context, arguments, persistent flags and
// hooks. They do not enter the default server RunE.
func TestCommandSubcommand(t *testing.T) {
	t.Parallel()
	var order []string
	var verbose bool
	journal := filepath.Join(t.TempDir(), "journal")
	a := New(WithJournal(journal), WithInstructions("missing.md"),
		WithCommand(func(root *cobra.Command, _ *Agent) {
			root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")
			root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
				if cmd.Context() != t.Context() || !verbose {
					t.Fatal("persistent hook lost the context or flag")
				}
				order = append(order, "pre")
				return nil
			}
			child := &cobra.Command{
				Use: "inspect", Args: cobra.ExactArgs(1),
				RunE: func(cmd *cobra.Command, args []string) error {
					value, err := cmd.Flags().GetString("format")
					if err != nil {
						return err
					}
					if value != "json" || args[0] != "record" || cmd.Context() != t.Context() {
						t.Fatal("subcommand lost flags, arguments, or context")
					}
					order = append(order, "run")
					return nil
				},
			}
			child.Flags().String("format", "text", "output format")
			root.AddCommand(child)
		}))
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	cmd := a.serveCommand(fs)
	cmd.SetArgs(serveArgs([]string{"inspect", "-verbose", "--format=json", "record"}, fs, cmd))
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"pre", "run"}) {
		t.Fatalf("hook order = %v", order)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("subcommand opened server resources: %v", err)
	}
}

// Root and child help must include registered commands and flags without
// running hooks, constructing channels, checking providers, or opening files.
func TestCommandHelpNoResources(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--help"}, {"inspect", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			journal := filepath.Join(t.TempDir(), "journal")
			p := &selectionProvider{t: t, name: "unavailable", availableErr: sandbox.ErrUnavailable}
			a := New(WithSandbox(p), WithJournal(journal), WithInstructions("missing.md"),
				WithChannel(func(*runtime.Runner) (Channel, error) {
					t.Fatal("help constructed a channel")
					return nil, nil
				}),
				WithCommand(func(cmd *cobra.Command, _ *Agent) {
					cmd.PersistentFlags().String("custom", "", "custom help")
					cmd.PersistentPreRunE = func(*cobra.Command, []string) error {
						t.Fatal("help ran a pre-run hook")
						return nil
					}
					cmd.AddCommand(&cobra.Command{Use: "inspect", Short: "inspect help", RunE: func(*cobra.Command, []string) error {
						t.Fatal("help ran the subcommand")
						return nil
					}})
				}))
			cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"inspect", "--custom", "custom help"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("help does not contain %q: %s", want, &output)
				}
			}
			if p.availableCalls != 0 {
				t.Fatal("help checked backend availability")
			}
			if _, err := os.Stat(journal); !os.IsNotExist(err) {
				t.Fatalf("help opened the journal: %v", err)
			}
		})
	}
}

// A pre-run error must stop Run and preserve the error for the host.
func TestCommandPrehookError(t *testing.T) {
	t.Parallel()
	want := errors.New("invalid parsed configuration")
	journal := filepath.Join(t.TempDir(), "journal")
	a := New(WithJournal(journal), WithCommand(func(cmd *cobra.Command, _ *Agent) {
		cmd.PreRunE = func(*cobra.Command, []string) error { return want }
	}))
	cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(t.Context()); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("failed pre-run hook opened journal: %v", err)
	}
}

// Configure applies the existing option rules, in order, without rebuilding
// defaults. Replacement values win; collections keep their prior entries.
func TestConfigureOverrides(t *testing.T) {
	t.Parallel()
	a := New(WithName("original"), WithAddr("original-address"),
		WithSandboxEnv(map[string]string{"keep": "value", "replace": "old"}),
		WithChannel(func(*runtime.Runner) (Channel, error) { return nil, nil }))
	a.Configure()
	a.Configure(WithName("first"), WithName("last"), WithAddr("parsed-address"),
		WithJournal("parsed-journal"), WithModel("parsed/model"), WithWebUI(true),
		WithShutdownTimeout(time.Second), WithScheduleClock(false),
		WithSandboxEnv(map[string]string{"replace": "new", "add": "value"}),
		WithChannel(func(*runtime.Runner) (Channel, error) { return nil, nil }))
	if a.cfg.name != "last" || a.cfg.addr != "parsed-address" || a.cfg.journal != "parsed-journal" ||
		a.cfg.model != "parsed/model" || !a.cfg.webUI || a.cfg.shutdown != time.Second ||
		a.cfg.scheduleClock || !a.cfg.scheduleClockSet {
		t.Fatalf("parsed options were not applied: %+v", a.cfg)
	}
	if a.cfg.instrPath != DefaultInstructions || len(a.cfg.channels) != 2 ||
		a.cfg.sandboxEnv["keep"] != "value" || a.cfg.sandboxEnv["replace"] != "new" || a.cfg.sandboxEnv["add"] != "value" {
		t.Fatal("Configure lost defaults or additive options")
	}
}

// Configure must retain validation rules for options that can be set only once.
func TestConfigureValidation(t *testing.T) {
	t.Parallel()
	a := New(WithSandboxes(sandbox.Local()), WithInstructions(""), WithSkills(""), WithContextFiles(""))
	a.Configure(WithSandboxes(sandbox.Local()))
	if !a.cfg.sandboxesDuplicate {
		t.Fatal("Configure bypassed duplicate provider validation")
	}
	if err := a.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "WithSandboxes") {
		t.Fatalf("error = %v, want duplicate WithSandboxes", err)
	}
}

// Custom root Cobra flags have the same single-dash compatibility as Go
// flags. Value tokens, shorthand flags, unknown flags, and -- stay unchanged.
func TestCommandServeArgs(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	cmd := New(WithCommand(func(cmd *cobra.Command, _ *Agent) {
		cmd.Flags().StringP("custom", "c", "", "custom")
		cmd.Flags().Int("count", 0, "count")
		cmd.PersistentFlags().BoolP("verbose", "v", false, "verbose")
	})).serveCommand(fs)
	args := []string{"-custom", "-addr", "--custom", "-model", "-count", "-7", "-verbose", "-verbose=false", "-v", "-c", "-addr", "-unknown", "-addr=:0", "--", "-custom=x"}
	before := slices.Clone(args)
	want := []string{"--custom", "-addr", "--custom", "-model", "--count", "-7", "--verbose", "--verbose=false", "-v", "-c", "-addr", "-unknown", "--addr=:0", "--", "-custom=x"}
	if got := serveArgs(args, fs, cmd); !slices.Equal(got, want) {
		t.Fatalf("serveArgs = %q, want %q", got, want)
	}
	if !slices.Equal(args, before) {
		t.Fatal("serveArgs changed its input")
	}
}
