package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Muriano/OctoGuard/internal/guides"
	"github.com/Muriano/OctoGuard/internal/ref"
)

func newGuideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guide [topic]",
		Short: "Show an embedded usage guide, or list the topics",
		Long: "Show an embedded Markdown guide for this octoguard version. Without a topic,\n" +
			"list the topics (" + strings.Join(guides.Topics(), ", ") + "). Start with `octoguard guide quickstart`.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				text, err := guides.Render(args[0], Version)
				if err != nil {
					return fmt.Errorf("%w: %v", ref.ErrUsage, err)
				}
				_, err = io.WriteString(out, text)
				return err
			}
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			if format == "text" {
				_, err = fmt.Fprintln(out, strings.Join(guides.Topics(), "\n"))
				return err
			}
			return json.NewEncoder(out).Encode(map[string]any{"schema_version": 1, "topics": guides.Topics()})
		},
	}
}
