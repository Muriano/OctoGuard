package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/skills"
)

func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Install or update the octoguard skill for coding agents",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		newSkillsSubCmd("install", "Install the octoguard SKILL.md where each agent looks for skills", skills.Install),
		newSkillsSubCmd("update", "Refresh installed octoguard skills; files without the octoguard marker are left alone", skills.Update),
	)
	return cmd
}

func newHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hook claude",
		Short: "Claude Code PreToolUse hook: block the gh commands octoguard replaces",
		Long: "Reads the PreToolUse input on stdin. For a Bash command running a gh command that\n" +
			"octoguard replaces (pr view/checks/merge/comment/create/edit, issue view/comment/create,\n" +
			"run view/watch, gh api on pulls/issues/checks/runs/rate_limit) it prints a deny decision\n" +
			"naming the octoguard command to use; otherwise it prints nothing. Installed by\n" +
			"`octoguard skills install --agent claude --enforce`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "claude" {
				return fmt.Errorf("%w: unknown hook %q: only claude", ref.ErrUsage, args[0])
			}
			in, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			out, err := skills.HookResponse(in)
			if err != nil || out == nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return err
		},
	}
}

func newSkillsSubCmd(name, short string, op func(skills.Options) ([]skills.Result, error)) *cobra.Command {
	var agent, scope string
	var enforce bool
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := skills.Options{Scope: scope}
			switch {
			case agent == "all":
				opts.Agents = skills.Agents
			case slices.Contains(skills.Agents, agent):
				opts.Agents = []string{agent}
			default:
				return fmt.Errorf("%w: invalid --agent %q: must be %s or all", ref.ErrUsage, agent, strings.Join(skills.Agents, ", "))
			}
			switch scope {
			case skills.ScopeUser:
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				opts.Home = home
			case skills.ScopeProject:
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				opts.Root = skills.ProjectRoot(wd)
			default:
				return fmt.Errorf("%w: invalid --scope %q: must be user or project", ref.ErrUsage, scope)
			}
			if enforce && !slices.Contains(opts.Agents, "claude") {
				return fmt.Errorf("%w: --enforce applies to claude only", ref.ErrUsage)
			}
			results, err := op(opts)
			if err != nil {
				return err
			}
			if enforce {
				more, err := skills.Enforce(opts)
				if err != nil {
					return err
				}
				results = append(results, more...)
			}
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if format == "text" {
				for _, r := range results {
					if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", r.Agent, r.Action, r.Path); err != nil {
						return err
					}
				}
				return nil
			}
			return json.NewEncoder(out).Encode(map[string]any{"schema_version": 1, "results": results})
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "all", "agent: claude|codex|gemini|copilot|all")
	cmd.Flags().StringVar(&scope, "scope", skills.ScopeUser, "scope: user (home directory) or project (repository root)")
	if name == "install" {
		cmd.Flags().BoolVar(&enforce, "enforce", false,
			"claude: also add a CLAUDE.md block and a PreToolUse hook that blocks the gh commands octoguard replaces")
	}
	return cmd
}
