package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/write"
)

// ErrMergeRejected is returned by `pr merge` when GitHub refuses the merge
// (not mergeable, or the head moved); the process exits with code 4.
var ErrMergeRejected = errors.New("merge rejected by GitHub; see \"reason\" and \"hint\" in the output")

const writesNote = "Writes are spaced (1/s, 60/min per token, queued, never refused) and invalidate the cache of what they touch."

func newPRCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pr", Short: "Write to pull requests: create, comment, edit, merge", Long: writesNote}
	cmd.AddCommand(newPRCreateCmd(), newCommentCmd(write.PRComment, ref.PR), newPREditCmd(), newPRMergeCmd())
	return cmd
}

func newIssueCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "issue", Short: "Write to issues: comment, create", Long: writesNote}
	cmd.AddCommand(newCommentCmd(write.IssueComment, ref.Issue), newIssueCreateCmd())
	return cmd
}

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Write to Projects (v2) by node ID: item-add, item-edit", Long: writesNote}
	cmd.AddCommand(newItemAddCmd(), newItemEditCmd())
	return cmd
}

// bodyFlags registers --body and --body-file ("-" reads stdin).
type bodyFlags struct{ body, file string }

func (b *bodyFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&b.body, "body", "b", "", "body text")
	cmd.Flags().StringVarP(&b.file, "body-file", "F", "", "read the body from a file (\"-\" for stdin)")
	cmd.MarkFlagsMutuallyExclusive("body", "body-file")
}

func (b *bodyFlags) read(cmd *cobra.Command) (string, error) {
	if b.file == "" {
		return b.body, nil
	}
	var data []byte
	var err error
	if b.file == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(b.file)
	}
	if err != nil {
		return "", fmt.Errorf("read --body-file: %w", err)
	}
	return string(data), nil
}

// parseRef parses a <ref> argument of kind.
func parseRef(s string, kind ref.Kind) (ref.Ref, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return ref.Ref{}, err
	}
	return ref.Parse(s, kind, cwd)
}

// repoRef resolves --repo owner/repo, or the origin remote of the current
// directory, to a ref without a number.
func repoRef(repo string, kind ref.Kind) (ref.Ref, error) {
	s := "1" // a placeholder number, resolved against the origin remote
	if repo != "" {
		s = repo + "#1"
	}
	r, err := parseRef(s, kind)
	if err != nil {
		return ref.Ref{}, err
	}
	r.Number = 0
	return r, nil
}

func newPRCreateCmd() *cobra.Command {
	var b bodyFlags
	var repo, title, base, head string
	var labels, reviewers, assignees []string
	cmd := &cobra.Command{
		Use:   "create --title T --head BRANCH --base BRANCH",
		Short: "Create a pull request",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := repoRef(repo, ref.PR)
			if err != nil {
				return err
			}
			body, err := b.read(cmd)
			if err != nil {
				return err
			}
			return runWrite(cmd, write.Request{Op: write.PRCreate, Ref: r, Title: title, Body: body, Base: base, Head: head,
				Labels: labels, Reviewers: reviewers, Assignees: assignees})
		},
	}
	b.register(cmd)
	cmd.Flags().StringVarP(&repo, "repo", "R", "", "owner/repo (default: the origin remote of the current directory)")
	cmd.Flags().StringVarP(&title, "title", "t", "", "title")
	cmd.Flags().StringVarP(&base, "base", "B", "", "branch to merge into")
	cmd.Flags().StringVarP(&head, "head", "H", "", "branch with the changes")
	cmd.Flags().StringSliceVarP(&labels, "label", "l", nil, "add a label (repeatable)")
	cmd.Flags().StringSliceVarP(&reviewers, "reviewer", "r", nil, "request a review from a login or org/team (repeatable; \"@me\" is you)")
	cmd.Flags().StringSliceVarP(&assignees, "assignee", "a", nil, "assign a login (repeatable; \"@me\" is you)")
	return cmd
}

func newIssueCreateCmd() *cobra.Command {
	var b bodyFlags
	var repo, title string
	var labels, assignees []string
	cmd := &cobra.Command{
		Use:   "create --title T",
		Short: "Create an issue",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := repoRef(repo, ref.Issue)
			if err != nil {
				return err
			}
			body, err := b.read(cmd)
			if err != nil {
				return err
			}
			return runWrite(cmd, write.Request{Op: write.IssueCreate, Ref: r, Title: title, Body: body, Labels: labels, Assignees: assignees})
		},
	}
	b.register(cmd)
	cmd.Flags().StringVarP(&repo, "repo", "R", "", "owner/repo (default: the origin remote of the current directory)")
	cmd.Flags().StringVarP(&title, "title", "t", "", "title")
	cmd.Flags().StringSliceVarP(&labels, "label", "l", nil, "add a label (repeatable)")
	cmd.Flags().StringSliceVarP(&assignees, "assignee", "a", nil, "assign a login (repeatable; \"@me\" is you)")
	return cmd
}

func newCommentCmd(op write.Op, kind ref.Kind) *cobra.Command {
	var b bodyFlags
	cmd := &cobra.Command{
		Use:   "comment <ref> --body TEXT",
		Short: fmt.Sprintf("Comment on a %s", kind),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRef(args[0], kind)
			if err != nil {
				return err
			}
			body, err := b.read(cmd)
			if err != nil {
				return err
			}
			return runWrite(cmd, write.Request{Op: op, Ref: r, Body: body})
		},
	}
	b.register(cmd)
	return cmd
}

func newPREditCmd() *cobra.Command {
	var b bodyFlags
	var title, base string
	var labels, reviewers, assignees []string
	cmd := &cobra.Command{
		Use:   "edit <ref>",
		Short: "Edit a pull request's title, body or base, or add labels, reviewers or assignees",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRef(args[0], ref.PR)
			if err != nil {
				return err
			}
			body, err := b.read(cmd)
			if err != nil {
				return err
			}
			return runWrite(cmd, write.Request{Op: write.PREdit, Ref: r, Title: title, Body: body, Base: base,
				Labels: labels, Reviewers: reviewers, Assignees: assignees})
		},
	}
	b.register(cmd)
	cmd.Flags().StringVarP(&title, "title", "t", "", "new title")
	cmd.Flags().StringVarP(&base, "base", "B", "", "new base branch")
	cmd.Flags().StringSliceVar(&labels, "add-label", nil, "add a label (repeatable)")
	cmd.Flags().StringSliceVar(&reviewers, "add-reviewer", nil, "request a review from a login or org/team (repeatable; \"@me\" is you)")
	cmd.Flags().StringSliceVar(&assignees, "add-assignee", nil, "assign a login (repeatable; \"@me\" is you)")
	return cmd
}

func newPRMergeCmd() *cobra.Command {
	var merge, squash, rebase bool
	cmd := &cobra.Command{
		Use:   "merge <ref> [--merge|--squash|--rebase]",
		Short: "Merge a pull request; GitHub decides whether it can",
		Long: "Merge the head SHA read fresh. octoguard applies no criteria of its own: decide first\n" +
			"(e.g. after `wait --until checks-green` or `--until changed`). If GitHub refuses, exit 4 with\n" +
			"{\"status\":\"rejected\",\"reason\":...,\"hint\":\"octoguard wait pr <ref> --until changed\"}.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRef(args[0], ref.PR)
			if err != nil {
				return err
			}
			method := "merge"
			if squash {
				method = "squash"
			} else if rebase {
				method = "rebase"
			}
			return runWrite(cmd, write.Request{Op: write.PRMerge, Ref: r, Method: method})
		},
	}
	cmd.Flags().BoolVarP(&merge, "merge", "m", false, "merge commit (default)")
	cmd.Flags().BoolVarP(&squash, "squash", "s", false, "squash and merge")
	cmd.Flags().BoolVarP(&rebase, "rebase", "r", false, "rebase and merge")
	cmd.MarkFlagsMutuallyExclusive("merge", "squash", "rebase")
	return cmd
}

func newItemAddCmd() *cobra.Command {
	var project, content, pr, issue string
	cmd := &cobra.Command{
		Use:   "item-add --project OWNER/NUMBER (--pr|--issue) <ref>  |  --project PVT_... --content-id NODE_ID",
		Short: "Add an issue or pull request to a project",
		Long: "With --project OWNER/NUMBER (e.g. my-org/1) and --pr or --issue the item is added by REST,\n" +
			"which spends core quota, not GraphQL. With a PVT_ node ID and --content-id it uses GraphQL.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.HasPrefix(project, "PVT_") || (pr == "" && issue == "") {
				return runWrite(cmd, write.Request{Op: write.ProjectItemAdd, Project: project, Content: content})
			}
			owner, num, okSep := strings.Cut(project, "/")
			if !okSep {
				owner, num, okSep = strings.Cut(project, "#")
			}
			n, err := strconv.Atoi(num)
			if !okSep || owner == "" || err != nil || n <= 0 {
				return fmt.Errorf("%w: --project must be OWNER/NUMBER or a PVT_ node ID", ref.ErrUsage)
			}
			s, kind := pr, ref.PR
			if issue != "" {
				s, kind = issue, ref.Issue
			}
			r, err := parseRef(s, kind)
			if err != nil {
				return err
			}
			return runWrite(cmd, write.Request{Op: write.ProjectItemAdd, Ref: r, ProjectOwner: owner, ProjectNumber: n})
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "OWNER/NUMBER of the project, or its node ID (PVT_...)")
	cmd.Flags().StringVar(&pr, "pr", "", "pull request to add (REST)")
	cmd.Flags().StringVar(&issue, "issue", "", "issue to add (REST)")
	cmd.Flags().StringVar(&content, "content-id", "", "node ID of the issue or pull request, with a PVT_ project (\"node_id\" in `octoguard get`)")
	cmd.MarkFlagsMutuallyExclusive("pr", "issue", "content-id")
	return cmd
}

func newItemEditCmd() *cobra.Command {
	var project, item, field, value, typ string
	cmd := &cobra.Command{
		Use:   "item-edit --project PVT_... --id PVTI_... --field PVTF_... --value V",
		Short: "Set a field of a project item",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWrite(cmd, write.Request{Op: write.ProjectItemEdit, Project: project, Item: item, Field: field, Value: value, ValueType: typ})
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project node ID (PVT_...)")
	cmd.Flags().StringVar(&item, "id", "", "item node ID (PVTI_...)")
	cmd.Flags().StringVar(&field, "field", "", "field node ID (PVTF_...)")
	cmd.Flags().StringVar(&value, "value", "", "new value (for single-select: the option ID)")
	cmd.Flags().StringVar(&typ, "value-type", "text", "text|number|date|single-select")
	return cmd
}

// runWrite performs req and prints the result.
func runWrite(cmd *cobra.Command, req write.Request) error {
	ctx := cmd.Context()
	tok, err := auth.Resolve(ctx)
	if err != nil {
		return err
	}
	res, err := doWrite(ctx, cmd, tok, req)
	if err != nil {
		return err
	}
	if err := printWrite(cmd, req.Op, res); err != nil {
		return err
	}
	if res.Status == write.StatusRejected {
		return ErrMergeRejected
	}
	return nil
}

// doWrite writes through the daemon, which spaces the writes of every CLI on
// the machine, or directly when it is unavailable.
func doWrite(ctx context.Context, cmd *cobra.Command, tok auth.Token, req write.Request) (write.Result, error) {
	var res write.Result
	args, err := json.Marshal(req)
	if err != nil {
		return res, err
	}
	data, ok, err := callDaemon(ctx, cmd, daemon.Request{Op: daemon.OpWrite, Token: tok.Value, Args: args})
	if err != nil {
		return res, err
	}
	if ok {
		if err := json.Unmarshal(data, &res); err != nil {
			return res, fmt.Errorf("decode daemon response: %w", err)
		}
		return res, nil
	}

	apiURL, err := cmd.Flags().GetString("api-url")
	if err != nil {
		return res, err
	}
	ch, err := openCache()
	if err != nil {
		return res, err
	}
	defer func() { _ = ch.Close() }()
	w := &write.Writer{
		Client: gh.NewClient(apiURL, tok, ch, quota.New(time.Now), ch),
		Cache:  ch,
		Spacer: write.NewSpacer(nil, nil),
		Scope:  gh.Scope(ref.Host, tok),
	}
	return w.Execute(ctx, req)
}

func printWrite(cmd *cobra.Command, op write.Op, res write.Result) error {
	format, err := cmd.Flags().GetString("format")
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if format == "text" {
		line := fmt.Sprintf("%s %s", op, res.Status)
		if res.Reason != "" {
			line += ": " + res.Reason + "\nhint: " + res.Hint
		}
		_, err = fmt.Fprintln(out, line)
		return err
	}
	v := map[string]any{"schema_version": 1, "op": op, "status": res.Status}
	if res.Status == write.StatusRejected {
		v["reason"], v["hint"] = res.Reason, res.Hint
	} else {
		v["data"] = res.Data
	}
	return json.NewEncoder(out).Encode(v)
}
