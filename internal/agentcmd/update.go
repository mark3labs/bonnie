package agentcmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// NewUpdateCommand refreshes Kit's shared model database without an agent runtime.
func NewUpdateCommand() *cobra.Command {
	return newUpdateCommand(kit.UpdateModels)
}

func newUpdateCommand(update func(context.Context, kit.UpdateModelsOptions) (kit.UpdateModelsResult, error)) *cobra.Command {
	parent := &cobra.Command{
		Use: "update", Short: "Update shared agent data", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	models := &cobra.Command{
		Use: "models [source]", Short: "Refresh Kit's shared model database",
		Args: cobra.MaximumNArgs(1),
		Long: `Refresh Kit's shared model database without a server, provider key, or Kit executable.

With no source, use KIT_MODELS_URL or https://models.dev/api.json?type=all.
A source can be an HTTP(S) URL, a local JSON file, or embedded to reset the cache.
HTTP requests end after at most 30 seconds, or earlier on caller cancellation.
On Linux the shared store is $XDG_DATA_HOME/kit/providers.json, defaulting to
~/.local/share/kit/providers.json. Kit and compatible agents use this same store.
Use trusted sources: this is a host operation, outside the agent sandbox.
Restart running agents to use the model database. This does not update binaries.
If output fails after a successful update, the database change is not undone.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("bonnie: update models: %w", err)
			}
			source := ""
			if len(args) != 0 {
				source = args[0]
			}
			result, err := update(cmd.Context(), kit.UpdateModelsOptions{Source: source})
			if err != nil {
				return fmt.Errorf("bonnie: update models: %w", err)
			}
			var message string
			switch result.Status {
			case kit.UpdateModelsStatusUpdated:
				message = fmt.Sprintf("Model database updated: %d providers, %d models.", result.ProviderCount, result.ModelCount)
			case kit.UpdateModelsStatusNotModified:
				message = "Model database is already up to date."
			case kit.UpdateModelsStatusReset:
				message = "Model database reset to embedded version."
			default:
				return fmt.Errorf("bonnie: update models: unexpected result status %q", result.Status)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\nRestart running agents to use the model database.\n", message); err != nil {
				return fmt.Errorf("bonnie: update models output: %w", err)
			}
			return nil
		},
	}
	// Resolve the path at help time, after a host attaches this command group.
	models.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		cmd.Example = fmt.Sprintf("  %s\n  %s https://example.com/models.json\n  %s ./models.json\n  %s embedded", cmd.CommandPath(), cmd.CommandPath(), cmd.CommandPath(), cmd.CommandPath())
		cmd.SetHelpFunc(nil)
		cmd.HelpFunc()(cmd, nil)
	})
	parent.AddCommand(models)
	return parent
}
