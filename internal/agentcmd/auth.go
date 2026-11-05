package agentcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// maxAPIKeyBytes bounds a key read from standard input. A real provider key is
// far smaller; the bound stops a mistaken pipe from filling the credential file.
const maxAPIKeyBytes = 64 << 10

// errNoKeyInput is returned when there is no terminal for the hidden prompt and
// --api-key-stdin is not set. The command never waits on a pipe it was not told
// to read.
var errNoKeyInput = errors.New("standard input is not a terminal; pipe the key with --api-key-stdin")

// NewAuthCommand manages stored model-provider credentials through Kit's public
// SDK. OpenAI login prints the browser URL; it does not start an agent or open
// a browser. Other providers store an API key from a hidden prompt or from
// standard input. The CLI and each compiled agent's Serve use this command.
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
		remove:   kit.RemoveProviderCredentials,
		provider: kit.GetProviderInfo,
		setKey: func(provider, key, region string) error {
			// Without --region, keep the region stored with the previous key.
			if region == "" {
				return kit.SetProviderAPIKey(provider, key)
			}
			cm, err := kit.NewCredentialManager()
			if err != nil {
				return err
			}
			return cm.SetProviderAPIKeyWithRegion(provider, key, region)
		},
		prompt: promptAPIKey,
	})
}

// authActions keeps command tests independent of live OAuth, the terminal,
// and user credentials.
type authActions struct {
	login    func(context.Context, string, kit.LoginOptions) error
	load     func() (*kit.CredentialStore, error)
	remove   func(string) error
	provider func(string) *kit.ProviderInfo
	setKey   func(provider, key, region string) error
	// prompt reads a key without echo. It returns errNoKeyInput when there is
	// no terminal.
	prompt func(cmd *cobra.Command, title string) (string, error)
}

// promptAPIKey reads a key from the process terminal without echo. It reads
// os.Stdin, not cmd.InOrStdin, because only a real terminal can hide input.
func promptAPIKey(cmd *cobra.Command, title string) (string, error) {
	fd := os.Stdin.Fd()
	if !term.IsTerminal(fd) {
		return "", errNoKeyInput
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s: ", title); err != nil {
		return "", err
	}
	key, err := term.ReadPassword(fd)
	// End the prompt line: the terminal did not echo the newline.
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	return string(key), nil
}

func newAuthCommand(actions authActions) *cobra.Command {
	cmd := &cobra.Command{
		Use: "auth", Short: "Manage stored model-provider credentials", Args: cobra.NoArgs,
		Long: "Manage Kit's stored model-provider credentials, not HTTP or MCP authentication.\n" +
			"Credentials are shared with Kit. These commands do not start an agent.\n" +
			"Status reports stored credentials only, not environment keys or token validity.",
	}
	var keyFromStdin bool
	var region string
	login := &cobra.Command{
		Use: "login provider", Short: "Log in to OpenAI with OAuth, or store a provider API key", Args: cobra.ExactArgs(1),
		Long: "Store credentials for a model provider in Kit's shared store.\n\n" +
			"openai: log in with ChatGPT/Codex subscription OAuth, not general OpenAI API OAuth.\n" +
			"Open the printed URL in your browser. The browser must reach localhost:1455 on\n" +
			"this host. For a remote host, forward loopback port 1455 before login. There is\n" +
			"no manual code-entry fallback. Login ends after at most five minutes.\n" +
			"With --api-key-stdin, openai stores an API key instead.\n\n" +
			"Other providers (anthropic, google, groq, amazon-bedrock, ...): store an API key.\n" +
			"The key is read from a hidden prompt, or from standard input with --api-key-stdin.\n" +
			"A stored key takes precedence over the provider's environment variable.\n" +
			"GitHub Copilot uses OAuth only; use 'kit auth login copilot'.\n\n" +
			"Restart running agents after login.",
		Example: "  bonnie auth login openai\n" +
			"  bonnie auth login anthropic\n" +
			"  printenv GROQ_API_KEY | bonnie auth login groq --api-key-stdin\n" +
			"  bonnie auth login amazon-bedrock --region eu-central-1",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("bonnie: auth login: %w", err)
			}
			name := strings.ToLower(strings.TrimSpace(args[0]))
			if name == "openai" && !keyFromStdin {
				if region != "" {
					return fmt.Errorf("bonnie: auth login: --region applies to amazon-bedrock only")
				}
				return loginOAuth(cmd, actions, name)
			}
			return loginAPIKey(cmd, actions, name, region, keyFromStdin)
		},
	}
	login.Flags().BoolVar(&keyFromStdin, "api-key-stdin", false, "read the API key from standard input")
	login.Flags().StringVar(&region, "region", "", "AWS region of the key (amazon-bedrock only)")
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

// loginOAuth runs Kit's provider OAuth. Kit supports only openai.
func loginOAuth(cmd *cobra.Command, actions authActions, provider string) error {
	err := actions.login(cmd.Context(), provider, kit.LoginOptions{
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
}

// loginAPIKey stores an API key for a provider that Kit's model database knows.
// The key comes from standard input or a hidden prompt, never from an argument,
// so it does not reach shell history or the process list.
func loginAPIKey(cmd *cobra.Command, actions authActions, name, region string, fromStdin bool) error {
	info := actions.provider(name)
	if info == nil {
		return fmt.Errorf("bonnie: auth login: unknown provider %q (see 'bonnie update models')", name)
	}
	provider := info.ID
	if provider == "" {
		provider = name
	}
	if provider == "github-copilot" {
		return fmt.Errorf("bonnie: auth login: GitHub Copilot uses OAuth only; use 'kit auth login copilot'")
	}
	region = strings.TrimSpace(region)
	if region != "" && provider != "amazon-bedrock" {
		return fmt.Errorf("bonnie: auth login: --region applies to amazon-bedrock only")
	}
	display := info.Name
	if display == "" {
		display = provider
	}
	var key string
	if fromStdin {
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxAPIKeyBytes+1))
		if err != nil {
			return fmt.Errorf("bonnie: auth login: read API key: %w", err)
		}
		if len(data) > maxAPIKeyBytes {
			return fmt.Errorf("bonnie: auth login: API key is longer than %d bytes", maxAPIKeyBytes)
		}
		key = string(data)
	} else {
		var err error
		if key, err = actions.prompt(cmd, display+" API key"); err != nil {
			return fmt.Errorf("bonnie: auth login: read API key: %w", err)
		}
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("bonnie: auth login: API key is empty")
	}
	if err := cmd.Context().Err(); err != nil {
		return fmt.Errorf("bonnie: auth login: %w", err)
	}
	if err := actions.setKey(provider, key, region); err != nil {
		return fmt.Errorf("bonnie: auth login: store API key: %w", err)
	}
	msg := fmt.Sprintf("%s API key saved", display)
	if region != "" {
		msg += " for region " + region
	}
	msg += ". It takes precedence over environment keys. Restart running agents to use it."
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), msg); err != nil {
		return fmt.Errorf("bonnie: auth login output: %w", err)
	}
	return nil
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
