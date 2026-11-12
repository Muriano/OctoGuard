// Package cli wires the octoguard command tree.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is injected at build time with
// -ldflags "-X github.com/Muriano/OctoGuard/internal/cli.Version=<version>".
var Version = "dev"

// NewRoot builds the root octoguard command with its global flags.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "octoguard",
		Short:         "GitHub API quota guardian for AI agents",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			if format != "json" && format != "text" {
				return fmt.Errorf("invalid --format %q: must be json or text", format)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	pf := root.PersistentFlags()
	pf.String("format", "json", "output format: json|text")
	pf.String("api-url", "https://api.github.com", "GitHub API base URL (for tests)")
	_ = pf.MarkHidden("api-url")
	root.AddCommand(newDaemonCmd(), newGetCmd(), newQuotaCmd(), newStatsCmd(), newWaitCmd(),
		newPRCmd(), newIssueCmd(), newProjectCmd(), newGuideCmd(), newAgentContextCmd(), newSkillsCmd())
	return root
}
