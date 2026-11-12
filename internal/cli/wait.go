package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/stats"
	"github.com/Muriano/OctoGuard/internal/wait"
)

// ErrPending is returned by `wait` when the timeout expires before the
// condition is met; the process exits with code 3.
var ErrPending = errors.New("wait: timed out with the condition still pending; run the same command again to keep waiting")

// fallbackMinInterval is the slowest pace of a wait polled in-process when
// the daemon is unavailable.
const fallbackMinInterval = 60 * time.Second

var waitKinds = map[string]ref.Kind{"pr": ref.PR, "issue": ref.Issue, "run": ref.Run}

func newWaitCmd() *cobra.Command {
	var until string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait pr|issue|run <ref>",
		Short: "Block until a condition holds on a PR, issue or workflow run",
		Long: "Block until a condition holds or --timeout expires.\n" +
			"  pr:    --until checks-done|checks-green|review|comment|merged|closed|changed\n" +
			"  issue: --until comment|closed|labeled|changed\n" +
			"  run:   --until done (default)\n" +
			"Exit codes: 0 met or failed (see \"status\"), 3 pending (timed out: call again).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, ok := waitKinds[args[0]]
			if !ok {
				return fmt.Errorf("%w: unknown kind %q: use pr, issue or run", ref.ErrUsage, args[0])
			}
			if until == "" {
				if kind != ref.Run {
					return fmt.Errorf("%w: --until is required for a %s", ref.ErrUsage, kind)
				}
				until = string(wait.Done)
			}
			cond, err := wait.ParseCondition(kind, until)
			if err != nil {
				return err
			}
			if timeout <= 0 {
				return fmt.Errorf("%w: --timeout must be positive", ref.ErrUsage)
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			r, err := ref.Parse(args[1], kind, cwd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			tok, err := auth.Resolve(ctx)
			if err != nil {
				return err
			}
			res, err := runWait(ctx, cmd, tok, daemon.WaitArgs{Ref: r, Condition: cond, Timeout: timeout})
			if err != nil {
				return err
			}
			if err := printWait(cmd, r, res); err != nil {
				return err
			}
			if res.Status == wait.StatusPending {
				return ErrPending
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&until, "until", "", "condition to wait for (see above)")
	cmd.Flags().DurationVar(&timeout, "timeout", 8*time.Minute, "give up and report pending after this long")
	return cmd
}

// runWait waits through the daemon or, when it is unavailable, polls
// in-process at a conservative pace. Either way it records wait_saved.
func runWait(ctx context.Context, cmd *cobra.Command, tok auth.Token, a daemon.WaitArgs) (wait.Result, error) {
	var res wait.Result
	args, err := json.Marshal(a)
	if err != nil {
		return res, err
	}
	// The daemon answers by the timeout; the margin covers a slow last poll.
	dctx, cancel := context.WithTimeout(ctx, a.Timeout+time.Minute)
	defer cancel()
	data, ok, err := callDaemon(dctx, cmd, daemon.Request{Op: daemon.OpWait, Token: tok.Value, Args: args})
	if err != nil {
		return res, err
	}
	ch, cerr := openCache()
	if ok {
		if err := json.Unmarshal(data, &res); err != nil {
			return res, fmt.Errorf("decode daemon response: %w", err)
		}
	} else {
		if cerr != nil {
			return res, cerr
		}
		if res, err = waitInProcess(ctx, cmd, tok, ch, a); err != nil {
			_ = ch.Close()
			return res, err
		}
	}
	if cerr == nil {
		// Best effort, like every counter.
		if n := wait.Saved(res.Duration, stats.Baseline(), res.Polls); n > 0 {
			_ = ch.IncCounter(gh.Scope(a.Ref.Host, tok), stats.CounterWaitSaved, n, time.Now())
		}
		_ = ch.Close()
	}
	return res, nil
}

func waitInProcess(ctx context.Context, cmd *cobra.Command, tok auth.Token, ch *cache.Cache, a daemon.WaitArgs) (wait.Result, error) {
	apiURL, err := cmd.Flags().GetString("api-url")
	if err != nil {
		return wait.Result{}, err
	}
	gov := quota.New(time.Now)
	c := gh.NewClient(apiURL, tok, ch, gov, ch)
	pool := wait.NewPool(wait.Config{Gov: gov, Counters: ch, MinInterval: fallbackMinInterval})
	defer pool.Close()
	return pool.Wait(ctx, wait.Request{
		Scope: gh.Scope(a.Ref.Host, tok), Ref: a.Ref, Cond: a.Condition, Timeout: a.Timeout,
		Fetch: func(ctx context.Context, r ref.Ref, what gh.Resource) (json.RawMessage, error) {
			res, err := c.Get(ctx, r, what, gh.GetOpts{})
			return res.Data, err
		},
	})
}

func printWait(cmd *cobra.Command, r ref.Ref, res wait.Result) error {
	format, err := cmd.Flags().GetString("format")
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if format == "text" {
		_, err = fmt.Fprintf(out, "%s %s %s changes=%d polls=%d waited=%s\n",
			r.Key(), res.Condition, res.Status, len(res.Changes), res.Polls, res.Duration.Round(time.Second))
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"schema_version": 1,
		"ref":            r.Key(),
		"status":         res.Status,
		"condition":      res.Condition,
		"changes":        res.Changes,
		"state":          res.State,
	})
}
