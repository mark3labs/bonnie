package bonnie

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"

	"github.com/mark3labs/bonnie/sandbox"
)

// Help must describe the permitted providers without checking their resources
// or opening the journal. Even invalid configuration must allow help.
func TestServeCommandHelp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts func(*selectionProvider, *selectionProvider) []Option
		want string
	}{
		{"default", func(_, _ *selectionProvider) []Option { return nil }, "sandbox backend: landlock (default: landlock)"},
		{"single", func(first, _ *selectionProvider) []Option { return []Option{WithSandbox(first)} }, "sandbox backend: first (default: first)"},
		{"list", func(first, second *selectionProvider) []Option { return []Option{WithSandboxes(first, second)} }, "sandbox backend: first, second (default: first)"},
		{"empty", func(_, _ *selectionProvider) []Option { return []Option{WithSandboxes()} }, sandboxFlagUsage},
		{"nil", func(_, _ *selectionProvider) []Option {
			var absent *selectionProvider
			return []Option{WithSandboxes(nil, absent)}
		}, sandboxFlagUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := &selectionProvider{t: t, name: "first", availableErr: sandbox.ErrUnavailable}
			second := &selectionProvider{t: t, name: "second", availableErr: sandbox.ErrUnavailable}
			root := t.TempDir()
			journal := filepath.Join(root, "journal")
			opts := append(tc.opts(first, second), WithJournal(journal), WithInstructions(filepath.Join(root, "missing.md")))
			a := New(opts...)
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			cmd := a.serveCommand(fs)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"--help"})
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{tc.want, "--addr", "--model", "--sandbox"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("help does not contain %q: %s", want, &output)
				}
			}
			if first.availableCalls != 0 || second.availableCalls != 0 {
				t.Fatal("help checked backend availability")
			}
			if _, err := os.Stat(journal); !os.IsNotExist(err) {
				t.Fatalf("help journal stat = %v, want not exist", err)
			}
		})
	}
}

// The command must call Run with its context. A custom factory requires no
// instructions or workspace, and cancellation must stop the HTTP server.
func TestServeCommandRun(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	journal := filepath.Join(root, "journal")
	a := New(WithAgentFactory(stubFactory), WithInstructions(filepath.Join(root, "missing.md")),
		WithWorkspace(""), WithJournal(journal), WithAddr("invalid-address"), Quiet())
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	cmd := a.serveCommand(fs)
	cmd.SetArgs([]string{"--addr", "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if a.cfg.addr != "127.0.0.1:0" {
		t.Fatalf("address = %q, want flag override", a.cfg.addr)
	}
	if _, err := os.Stat(filepath.Join(journal, "journal.db")); err != nil {
		t.Fatalf("Run did not create the journal: %v", err)
	}
}

// Flag overrides must reach Run's validation before a model is constructed.
// Empty flags must leave the matching Go options unchanged.
func TestServeCommandOverrides(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, model, selected, want string
		args                        []string
	}{
		{"model", "flag/model", "", "WithModel", []string{"--addr=127.0.0.1:0", "--model=flag/model"}},
		{"sandbox", "", "unknown", "WithSandbox", []string{"--addr=127.0.0.1:0", "--sandbox=unknown"}},
		{"empty", "option/model", "", "WithModel", []string{"--addr=", "--model="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			opts := []Option{WithAgentFactory(stubFactory), WithInstructions(filepath.Join(root, "missing.md")),
				WithWorkspace(""), WithJournal(root), WithAddr("option-address"), Quiet()}
			if tc.name == "empty" {
				opts = append(opts, WithModel("option/model"))
			}
			a := New(opts...)
			cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
			cmd.SetArgs(tc.args)
			err := cmd.ExecuteContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), "WithAgentFactory") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want factory conflict with %s", err, tc.want)
			}
			wantAddr := "127.0.0.1:0"
			if tc.name == "empty" {
				wantAddr = "option-address"
			}
			if a.cfg.addr != wantAddr || a.cfg.model != tc.model || a.cfg.sandboxName != tc.selected {
				t.Fatalf("configuration = addr %q, model %q, sandbox %q", a.cfg.addr, a.cfg.model, a.cfg.sandboxName)
			}
		})
	}
}

// Cobra must use the host's flag values, defaults, and help text. Parsing a
// bool or int must update the host's variable, not a separate copy.
func TestServeCommandHostFlags(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	addr := fs.String("addr", "host-address", "host address help")
	model := fs.String("model", "host-model", "host model help")
	selected := fs.String("sandbox", "host-sandbox", "host sandbox help")
	verbose := fs.Bool("verbose", false, "host bool help")
	count := fs.Int("count", 3, "host int help")
	original := make(map[string]*flag.Flag)
	fs.VisitAll(func(f *flag.Flag) { original[f.Name] = f })
	cmd := New().serveCommand(fs)
	for name, f := range original {
		got := cmd.Flags().Lookup(name)
		if fs.Lookup(name) != f || got.Value.String() != f.Value.String() || got.DefValue != f.DefValue || got.Usage != f.Usage {
			t.Fatalf("host flag %q was replaced or changed", name)
		}
	}
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--addr=parsed-address", "--model=parsed-model", "--sandbox=parsed-sandbox", "--verbose", "--count", "7", "--help"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *addr != "parsed-address" || *model != "parsed-model" || *selected != "parsed-sandbox" || !*verbose || *count != 7 {
		t.Fatalf("host values = %q, %q, %q, %v, %d", *addr, *model, *selected, *verbose, *count)
	}
	if !strings.Contains(output.String(), "host sandbox help") {
		t.Fatalf("host sandbox help was lost: %s", &output)
	}
}

// Single-dash long names are a Go flag compatibility contract. Values and
// arguments after -- must not be rewritten, and the input must stay intact.
func TestServeArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		args, want []string
	}{
		{"empty", nil, nil},
		{"long names", []string{"-addr", ":0", "-model=provider/model", "-sandbox", "local"}, []string{"--addr", ":0", "--model=provider/model", "--sandbox", "local"}},
		{"double dash", []string{"--addr", "-model", "--sandbox=local"}, []string{"--addr", "-model", "--sandbox=local"}},
		{"single dash value", []string{"-model", "-addr", "-sandbox=-model"}, []string{"--model", "-addr", "--sandbox=-model"}},
		{"bool and int", []string{"-verbose", "-count", "-7", "-verbose=false", "-addr=:0"}, []string{"--verbose", "--count", "-7", "--verbose=false", "--addr=:0"}},
		{"short and unknown", []string{"-x", "-unknown=value", "plain"}, []string{"--x", "-unknown=value", "plain"}},
		{"boundary", []string{"-addr=:0", "--", "-model", "-sandbox=local"}, []string{"--addr=:0", "--", "-model", "-sandbox=local"}},
		{"boundary as value", []string{"-model", "--", "-addr=:0"}, []string{"--model", "--", "--addr=:0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			registerServeFlags(fs)
			fs.Bool("verbose", false, "verbose")
			fs.Bool("x", false, "short flag")
			fs.Int("count", 0, "count")
			before := slices.Clone(tc.args)
			got := serveArgs(tc.args, fs)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("serveArgs = %q, want %q", got, tc.want)
			}
			if !slices.Equal(tc.args, before) {
				t.Fatal("serveArgs changed its input")
			}
		})
	}
}

// Fang must write help and errors to the command's writers. Disable its extra
// commands and version flag, as Serve does, without taking process ownership.
func TestServeFangExecute(t *testing.T) {
	t.Parallel()
	for _, help := range []bool{true, false} {
		name := "error"
		if help {
			name = "help"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &selectionProvider{t: t, name: "custom", availableErr: sandbox.ErrUnavailable}
			a := New(WithSandbox(p))
			cmd := a.serveCommand(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			args := []string{"--unknown"}
			if help {
				args = []string{"--help"}
			}
			cmd.SetArgs(args)
			err := fang.Execute(t.Context(), cmd, fang.WithoutVersion(), fang.WithoutCompletions(), fang.WithoutManpage())
			if help {
				if err != nil || stderr.Len() != 0 || !strings.Contains(strings.ToLower(stdout.String()), "sandbox backend: custom (default: custom)") {
					t.Fatalf("help = %q, stderr = %q, error = %v", &stdout, &stderr, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "unknown flag") || !strings.Contains(strings.ToLower(stderr.String()), "unknown flag") || stdout.Len() != 0 {
				t.Fatalf("stdout = %q, stderr = %q, error = %v", &stdout, &stderr, err)
			}
			if cmd.Version != "" || cmd.Flags().Lookup("version") != nil || len(cmd.Commands()) != 0 {
				t.Fatal("Fang added a disabled command or version flag")
			}
			if p.availableCalls != 0 {
				t.Fatal("Fang help or parse error checked backend availability")
			}
		})
	}
}
