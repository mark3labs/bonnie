package bonnie

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/mark3labs/bonnie/internal/agentcmd"
	"github.com/mark3labs/bonnie/sandbox"
)

// serve keeps process ownership separate from the library's Run method.
func (a *Agent) serve() {
	cmd := a.serveCommand(flag.CommandLine)
	cmd.SetArgs(serveArgs(os.Args[1:], flag.CommandLine, cmd))
	if err := fang.Execute(context.Background(), cmd,
		fang.WithNotifySignal(os.Interrupt, syscall.SIGTERM),
		fang.WithoutVersion(),
		fang.WithoutCompletions(),
		fang.WithoutManpage(),
	); err != nil {
		os.Exit(1)
	}
}

// serveCommand uses the same Go flag values the host registered. Help does not
// open a journal, read agent files, or check backend availability.
func (a *Agent) serveCommand(fs *flag.FlagSet) *cobra.Command {
	registerServeFlags(fs)
	cmd := &cobra.Command{
		Use:           filepath.Base(os.Args[0]),
		Short:         "Serve a durable BONNIE agent over HTTP",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if addr := fs.Lookup("addr").Value.String(); addr != "" {
				WithAddr(addr)(a.cfg)
			}
			if model := fs.Lookup("model").Value.String(); model != "" {
				WithModel(model)(a.cfg)
			}
			if cmd.Flags().Changed("schedule-clock") || fs.Lookup("schedule-clock").Value.String() != "true" {
				WithScheduleClock(fs.Lookup("schedule-clock").Value.String() == "true")(a.cfg)
			}
			if cmd.Flags().Changed("web") || fs.Lookup("web").Value.String() == "true" {
				WithWebUI(fs.Lookup("web").Value.String() == "true")(a.cfg)
			}
			if name := fs.Lookup("sandbox").Value.String(); name != "" {
				a.cfg.sandboxName = name
			}
			return a.Run(cmd.Context())
		},
	}
	cmd.Flags().AddGoFlagSet(fs)
	// Keep a host's help text. For the framework flag, show the permitted
	// providers without touching their runtime resources.
	if fs.Lookup("sandbox").Usage == sandboxFlagUsage {
		cmd.Flags().Lookup("sandbox").Usage = a.cfg.sandboxHelp()
	}
	// Explicit serve has the same flags and action as the default command.
	serve := &cobra.Command{
		Use: "serve", Short: "Serve the agent over HTTP", Args: cobra.NoArgs,
		RunE: cmd.RunE,
	}
	serve.Flags().AddGoFlagSet(fs)
	serve.Flags().Lookup("sandbox").Usage = cmd.Flags().Lookup("sandbox").Usage
	cmd.AddCommand(serve)
	if !a.cfg.noChatCommand {
		cmd.AddCommand(agentcmd.NewChatCommand(a.cfg.addr))
	}
	if !a.cfg.noRunsCommand {
		cmd.AddCommand(agentcmd.NewRunsCommand(a.cfg.journal))
	}
	if !a.cfg.noSchedulesCommand {
		cmd.AddCommand(agentcmd.NewSchedulesCommand(a.cfg.addr))
	}
	if !a.cfg.noVersionCommand {
		cmd.AddCommand(agentcmd.NewVersionCommand())
	}
	for _, fn := range a.cfg.commands {
		fn(cmd, a)
	}
	return cmd
}

const sandboxFlagUsage = "sandbox backend to use from the agent's permitted providers (default: first provider, or landlock)"

// registerServeFlags preserves flags that the host has already registered.
func registerServeFlags(fs *flag.FlagSet) {
	if fs.Lookup("web") == nil {
		fs.Bool("web", false, "serve the browser interface at /web")
	}
	if fs.Lookup("sandbox") == nil {
		fs.String("sandbox", "", sandboxFlagUsage)
	}
	if fs.Lookup("addr") == nil {
		fs.String("addr", "", "address to listen on")
	}
	if fs.Lookup("model") == nil {
		fs.String("model", "", "model to use, for example anthropic/claude-sonnet-4-5")
	}
	if fs.Lookup("schedule-clock") == nil {
		fs.Bool("schedule-clock", true, "run schedule clock")
	}
}

// sandboxHelp describes the list without validating availability. Invalid
// configuration is still reported by Run, not hidden by the help command.
func (c *config) sandboxHelp() string {
	providers := c.sandboxes
	if !c.sandboxesSet {
		if c.sandboxSet {
			providers = []sandbox.Provider{c.sandbox}
		} else {
			return "sandbox backend: landlock (default: landlock)"
		}
	}
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		if !nilProvider(provider) && provider.Name() != "" {
			names = append(names, provider.Name())
		}
	}
	if len(names) == 0 {
		return sandboxFlagUsage
	}
	return fmt.Sprintf("sandbox backend: %s (default: %s)", strings.Join(names, ", "), names[0])
}

// serveArgs retains Go flag's single-dash long names, including the -addr
// used by bonnie dev. An optional command adds custom root Cobra flags.
// Values and arguments after -- must stay unchanged. Subcommand-local flags
// use normal Cobra syntax.
func serveArgs(args []string, fs *flag.FlagSet, commands ...*cobra.Command) []string {
	result := append([]string(nil), args...)
	for i := 0; i < len(result); i++ {
		arg := result[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		// Cobra values take precedence when a callback changes a root flag.
		known, takesValue := false, false
		if len(commands) != 0 {
			f := commands[0].Flags().Lookup(name)
			if f == nil {
				f = commands[0].PersistentFlags().Lookup(name)
			}
			if f != nil {
				known, takesValue = true, f.NoOptDefVal == ""
			}
		}
		if !known {
			if f := fs.Lookup(name); f != nil {
				boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
				known, takesValue = true, !ok || !boolFlag.IsBoolFlag()
			}
		}
		if !known {
			// Preserve a custom shorthand and its value without turning it
			// into a long flag. Exact long names keep Go flag precedence.
			if len(commands) != 0 && len(name) == 1 && !strings.HasPrefix(arg, "--") {
				f := commands[0].Flags().ShorthandLookup(name)
				if f == nil {
					f = commands[0].PersistentFlags().ShorthandLookup(name)
				}
				if f != nil && f.NoOptDefVal == "" && !hasValue {
					i++
				}
			}
			continue
		}
		if !strings.HasPrefix(arg, "--") {
			result[i] = "-" + arg
		}
		if !hasValue && takesValue {
			i++
		}
	}
	return result
}
