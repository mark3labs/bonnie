package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/treetest"
)

// commandMain adds commands through the public API. An invalid model and a
// channel that panics make accidental server startup fail without a live model.
const commandMain = `package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
)

func main() {
	bonnie.New(
		bonnie.WithModel("invalid-provider/invalid-model"),
		bonnie.WithChannel(func(*runtime.Runner) (bonnie.Channel, error) {
			panic("command started server resources")
		}),
		bonnie.WithCommand(func(root *cobra.Command, a *bonnie.Agent) {
			var label, tenant string
			root.Flags().StringVar(&label, "label", "default", "custom root label")
			root.PersistentFlags().StringVar(&tenant, "tenant", "default", "custom persistent tenant")
			root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "HOOK tenant=%s\n", tenant)
				return err
			}
			// Stop before Run, but prove that root flags reached the hook.
			root.PreRunE = func(*cobra.Command, []string) error {
				a.Configure(bonnie.WithName(label))
				return fmt.Errorf("ROOT label=%s tenant=%s", label, tenant)
			}
			child := &cobra.Command{
				Use: "inspect record", Short: "inspect a custom record", Args: cobra.ExactArgs(1),
				RunE: func(cmd *cobra.Command, args []string) error {
					format, err := cmd.Flags().GetString("format")
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "CHILD record=%s tenant=%s format=%s\n", args[0], tenant, format)
					return err
				},
			}
			child.Flags().String("format", "text", "custom child format")
			root.AddCommand(child)
		}),
	).Serve()
}
`

// TestBuildOutputKeepsCustomCommands proves that bonnie build keeps authored
// root flags, persistent flags, hooks, and subcommands. Help and the child
// command must work without a model, a journal, or an agent tree on disk.
func TestBuildOutputKeepsCustomCommands(t *testing.T) {
	t.Parallel()
	root := buildHermeticTree(t)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(commandMain), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "agent")
	if err := runBuild(root, buildOpts{output: bin, env: treetest.BuildEnv()}); err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	for _, tc := range []struct {
		name    string
		args    []string
		want    []string
		wantErr bool
	}{
		{
			name: "root help", args: []string{"--help"},
			want: []string{"inspect", "--label", "custom root label", "--tenant", "custom persistent tenant"},
		},
		{
			name: "child help", args: []string{"inspect", "--help"},
			want: []string{"inspect", "--format", "custom child format", "--tenant", "custom persistent tenant"},
		},
		{
			name: "root flags", args: []string{"-label=parsed", "-tenant", "root-tenant"},
			want: []string{"HOOK tenant=root-tenant", "ROOT label=parsed tenant=root-tenant"}, wantErr: true,
		},
		{
			name: "child flags", args: []string{"-tenant", "child-tenant", "inspect", "--format=json", "record"},
			want: []string{"HOOK tenant=child-tenant", "CHILD record=record tenant=child-tenant format=json"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			private := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, tc.args...)
			cmd.Dir = private
			// No provider keys, user configuration, Go, or BONNIE on PATH.
			cmd.Env = []string{"HOME=" + private, "PATH=" + private, "TERM=dumb", "NO_COLOR=1"}
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("command did not stop: %v\n%s", ctx.Err(), out)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("command error = %v, want error = %v\n%s", err, tc.wantErr, out)
			}
			for _, want := range tc.want {
				// Fang changes the first letter of help and error text.
				if !strings.Contains(strings.ToLower(string(out)), strings.ToLower(want)) {
					t.Errorf("output does not contain %q:\n%s", want, out)
				}
			}
			if strings.HasSuffix(tc.name, "help") && strings.Contains(string(out), "HOOK tenant=") {
				t.Errorf("help ran a pre-run hook:\n%s", out)
			}
			for _, unwanted := range []string{"serving on", "command started server resources", "panic:"} {
				if strings.Contains(string(out), unwanted) {
					t.Errorf("command started the server or panicked:\n%s", out)
				}
			}
			if _, err := os.Stat(filepath.Join(private, ".bonnie")); !os.IsNotExist(err) {
				t.Errorf("command opened the journal: %v", err)
			}
		})
	}
}
