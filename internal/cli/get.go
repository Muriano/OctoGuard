package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
)

// dialDaemon connects to the daemon, starting it if needed; a test hook.
var dialDaemon = daemon.Dial

// getResources maps the get subcommand argument to the resource and the kind
// of ref it expects.
var getResources = map[string]struct {
	what gh.Resource
	kind ref.Kind
}{
	"pr":       {gh.PR, ref.PR},
	"issue":    {gh.Issue, ref.Issue},
	"reviews":  {gh.Reviews, ref.PR},
	"comments": {gh.Comments, ref.PR},
	"checks":   {gh.Checks, ref.PR},
	"runs":     {gh.Runs, ref.Run},
}

func newGetCmd() *cobra.Command {
	var fresh bool
	var maxAge time.Duration
	cmd := &cobra.Command{
		Use:   "get pr|issue|reviews|comments|checks|runs <ref>",
		Short: "Read a GitHub resource through the daemon's cache",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, ok := getResources[args[0]]
			if !ok {
				return fmt.Errorf("%w: unknown resource %q: use pr, issue, reviews, comments, checks or runs", ref.ErrUsage, args[0])
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			r, err := ref.Parse(args[1], res.kind, cwd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			tok, err := auth.Resolve(ctx)
			if err != nil {
				return err
			}
			ga := daemon.GetArgs{Ref: r, What: res.what, Fresh: fresh, MaxAge: maxAge}
			result, err := get(ctx, cmd, tok, ga)
			if err != nil {
				return err
			}
			return printGet(cmd, r, result)
		},
	}
	cmd.Flags().BoolVar(&fresh, "fresh", false, "validate every part against GitHub, ignoring --max-age")
	cmd.Flags().DurationVar(&maxAge, "max-age", 30*time.Second, "serve cached parts younger than this without any request")
	return cmd
}

// get reads through the daemon or, when it is unavailable, directly.
func get(ctx context.Context, cmd *cobra.Command, tok auth.Token, a daemon.GetArgs) (gh.Result, error) {
	var result gh.Result
	args, err := json.Marshal(a)
	if err != nil {
		return result, err
	}
	data, ok, err := callDaemon(ctx, cmd, daemon.Request{Op: daemon.OpGet, Token: tok.Value, Args: args})
	if err != nil {
		return result, err
	}
	if ok {
		if err := json.Unmarshal(data, &result); err != nil {
			return result, fmt.Errorf("decode daemon response: %w", err)
		}
		return result, nil
	}

	apiURL, err := cmd.Flags().GetString("api-url")
	if err != nil {
		return result, err
	}
	ch, err := openCache()
	if err != nil {
		return result, err
	}
	defer func() { _ = ch.Close() }()
	c := gh.NewClient(apiURL, tok, ch, quota.New(time.Now), ch)
	return c.Get(ctx, a.Ref, a.What, gh.GetOpts{Fresh: a.Fresh, MaxAge: a.MaxAge})
}

// callDaemon sends req to the daemon. ok is false when the daemon cannot be
// reached: the notice is printed and the caller goes direct. A daemon that
// answers with an error is not a reason to fall back.
func callDaemon(ctx context.Context, cmd *cobra.Command, req daemon.Request) (data json.RawMessage, ok bool, err error) {
	conn, err := dialDaemon(ctx)
	if err == nil {
		defer func() { _ = conn.Close() }()
		var resp daemon.Response
		resp, err = conn.Call(ctx, req)
		if err == nil {
			if !resp.OK {
				if resp.Code == daemon.CodeUsage {
					return nil, false, fmt.Errorf("%w: %s", ref.ErrUsage, resp.Err)
				}
				return nil, false, errors.New(resp.Err)
			}
			return resp.Data, true, nil
		}
	}
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	_, werr := fmt.Fprintln(cmd.ErrOrStderr(), "octoguard: daemon unavailable, direct call")
	return nil, false, werr
}

// openCache opens the cache shared with the daemon.
func openCache() (*cache.Cache, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return cache.Open(filepath.Join(dir, "octoguard", "cache.db"))
}

func printGet(cmd *cobra.Command, r ref.Ref, res gh.Result) error {
	format, err := cmd.Flags().GetString("format")
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if format == "text" {
		_, err = fmt.Fprintf(out, "%s via=%s from_cache=%t not_modified=%t fetched_at=%s\n%s\n",
			r.Key(), res.Via, res.FromCache, res.NotModified, res.FetchedAt.Format(time.RFC3339), res.Data)
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"schema_version": 1,
		"ref":            r.Key(),
		"data":           res.Data,
		"meta": map[string]any{
			"from_cache":   res.FromCache,
			"not_modified": res.NotModified,
			"via":          res.Via,
			"fetched_at":   res.FetchedAt,
		},
	})
}
