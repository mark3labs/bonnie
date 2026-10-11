package agentcmd

import (
	"context"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// NewAuthCommand manages stored model-provider credentials through Kit's public
// SDK. Login prints the browser URL; it does not start an agent or open a browser.
func NewAuthCommand() *cobra.Command {
	return newAuthCommand(authActions{
		login: kit.LoginProvider,
		load: func() (*kit.CredentialStore, error) {
			cm, err := kit.NewCredentialManager()
			if err != nil {
				return nil, err
			}
			return cm.LoadCredentials()
		},
		remove: kit.RemoveProviderCredentials,
	})
}

// authActions keeps command tests independent of live OAuth and user credentials.
type authActions struct {
	login  func(context.Context, string, kit.LoginOptions) error
	load   func() (*kit.CredentialStore, error)
	remove func(string) error
}

func newAuthCommand(actions authActions) *cobra.Command {
	cmd := &cobra.Command{
		Use: "auth", Short: "Manage stored model-provider credentials", Args: cobra.NoArgs,
		Long: "Manage Kit's stored model-provider credentials, not HTTP or MCP authentication.\n" +
			"Credentials are shared with Kit. These commands do not start an agent.\n" +
			"Status reports stored credentials only, not environment keys or token validity.",
	}
	login := &cobra.Command{
		Use: "login openai", Short: "Log in to OpenAI with ChatGPT/Codex OAuth", Args: cobra.ExactArgs(1),
		Long: "Log in to OpenAI with ChatGPT/Codex subscription OAuth, not general OpenAI API OAuth.\n" +
			"Only openai login is supported. Open the printed URL in your browser.\n" +
			"The browser must reach localhost:1455 on this host. For a remote host,\n" +
			"forward loopback port 1455 before login. There is no manual code-entry fallback.\n" +
			"Login ends after at most five minutes. Restart running agents after login.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("bonnie: auth login: %w", err)
			}
			if args[0] != "openai" {
				return fmt.Errorf("bonnie: auth login: only openai is supported")
			}
			err := actions.login(cmd.Context(), args[0], kit.LoginOptions{
				OnAuthorization: func(ctx context.Context, instructions kit.LoginInstructions) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					_, err := fmt.Fprintf(cmd.OutOrStdout(), "Open this URL in your browser:\n%s\n\nWaiting for authorization at %s\n", instructions.URL, instructions.RedirectURI)
					return err
				},
			})
			if err != nil {
				return fmt.Errorf("bonnie: auth login: %w", err)
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "OpenAI credentials saved. Restart running agents to use them."); err != nil {
				return fmt.Errorf("bonnie: auth login output: %w", err)
			}
			return nil
		},
	}
	status := &cobra.Command{
		Use: "status", Short: "List stored providers and credential types, without secrets", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("bonnie: auth status: %w", err)
			}
			store, err := actions.load()
			if err != nil {
				return fmt.Errorf("bonnie: auth status: %w", err)
			}
			// Project only provider names and types. Never serialize the store.
			providers := storedAuthTypes(store)
			if len(providers) == 0 {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), "No stored provider credentials."); err != nil {
					return fmt.Errorf("bonnie: auth status output: %w", err)
				}
				return nil
			}
			names := make([]string, 0, len(providers))
			for name := range providers {
				names = append(names, name)
			}
			slices.Sort(names)
			for _, name := range names {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", name, providers[name]); err != nil {
					return fmt.Errorf("bonnie: auth status output: %w", err)
				}
			}
			return nil
		},
	}
	logout := &cobra.Command{
		Use: "logout provider", Short: "Remove a provider's stored credentials from Kit's shared store", Args: cobra.ExactArgs(1),
		Long: "Remove stored credentials only. Environment keys and running agents are not changed.\n" +
			"This also removes the credentials used by Kit and other agents with this store.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("bonnie: auth logout: %w", err)
			}
			if err := actions.remove(args[0]); err != nil {
				return fmt.Errorf("bonnie: auth logout: %w", err)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed stored credentials for %s. Environment keys and running agents are unchanged.\n", args[0]); err != nil {
				return fmt.Errorf("bonnie: auth logout output: %w", err)
			}
			return nil
		},
	}
	cmd.AddCommand(login, status, logout)
	return cmd
}

func storedAuthTypes(store *kit.CredentialStore) map[string]string {
	providers := make(map[string]string)
	if store == nil {
		return providers
	}
	for name, credentials := range store.Providers {
		if credentials != nil {
			providers[name] = credentials.Type
		}
	}
	if store.Anthropic != nil {
		providers["anthropic"] = store.Anthropic.Type
	}
	if store.OpenAI != nil {
		providers["openai"] = store.OpenAI.Type
	}
	if store.Copilot != nil {
		providers["copilot"] = store.Copilot.Type
	}
	return providers
}
