package agentcmd

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// NewVersionCommand reports the executable and BONNIE module versions.
// It does not open resources or start the agent.
func NewVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print agent and BONNIE build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			agentVersion, bonnieVersion := "dev", "unknown"
			var revision string
			if info, ok := debug.ReadBuildInfo(); ok {
				if info.Main.Version != "" {
					agentVersion = info.Main.Version
				}
				if info.Main.Path == "github.com/mark3labs/bonnie" {
					bonnieVersion = agentVersion
				}
				for _, dep := range info.Deps {
					if dep.Path == "github.com/mark3labs/bonnie" {
						bonnieVersion = dep.Version
						if dep.Replace != nil {
							bonnieVersion += " (replaced)"
						}
					}
				}
				for _, setting := range info.Settings {
					if setting.Key == "vcs.revision" {
						revision = setting.Value
					}
					if setting.Key == "vcs.modified" && setting.Value == "true" {
						agentVersion += " (modified)"
					}
				}
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "agent %s\nbonnie %s\n", agentVersion, bonnieVersion); err != nil {
				return fmt.Errorf("bonnie: version: %w", err)
			}
			if revision != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "revision %s\n", revision); err != nil {
					return fmt.Errorf("bonnie: version: %w", err)
				}
			}
			return nil
		},
	}
}
