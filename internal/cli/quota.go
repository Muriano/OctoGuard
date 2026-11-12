package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/internal/quota"
)

// quotaResources are the buckets reported by `octoguard quota`.
var quotaResources = []string{"core", "graphql", "search"}

func newQuotaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "quota",
		Short: "Show the remaining GitHub API quota (core, graphql, search)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			tok, err := auth.Resolve(ctx)
			if err != nil {
				return err
			}
			// Without the daemon this process has seen no response yet, so
			// every bucket is unknown and gets probed below.
			snap := map[string]quota.Bucket{}
			data, ok, err := callDaemon(ctx, cmd, daemon.Request{Op: daemon.OpQuota, Token: tok.Value})
			if err != nil {
				return err
			}
			if ok {
				if err := json.Unmarshal(data, &snap); err != nil {
					return fmt.Errorf("decode daemon response: %w", err)
				}
			}
			apiURL, err := cmd.Flags().GetString("api-url")
			if err != nil {
				return err
			}
			buckets, probed, err := completeQuota(ctx, apiURL, tok, snap)
			if err != nil {
				return err
			}
			return printQuota(cmd, buckets, probed)
		},
	}
}

// completeQuota fills the buckets of snap that are not Known with a single
// call to /rate_limit, which does not consume quota. probed tells whether
// that call was made.
func completeQuota(ctx context.Context, apiURL string, tok auth.Token, snap map[string]quota.Bucket) (buckets map[string]quota.Bucket, probed bool, err error) {
	buckets = make(map[string]quota.Bucket, len(quotaResources))
	for _, name := range quotaResources {
		if b := snap[name]; b.Known {
			buckets[name] = b
		} else {
			probed = true
		}
	}
	if !probed {
		return buckets, false, nil
	}
	live, err := probeRateLimit(ctx, apiURL, tok)
	if err != nil {
		return nil, true, err
	}
	for _, name := range quotaResources {
		if _, ok := buckets[name]; !ok {
			buckets[name] = live[name] // zero (not Known) if GitHub omits it
		}
	}
	return buckets, true, nil
}

// probeRateLimit reads GET /rate_limit.
func probeRateLimit(ctx context.Context, apiURL string, tok auth.Token) (map[string]quota.Bucket, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(apiURL, "/")+"/rate_limit", nil)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "octoguard")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("github: read /rate_limit: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var msg struct{ Message string }
		_ = json.Unmarshal(body, &msg)
		return nil, fmt.Errorf("github: /rate_limit: %d %s", resp.StatusCode, msg.Message)
	}
	var rl struct {
		Resources map[string]struct {
			Limit     int   `json:"limit"`
			Remaining int   `json:"remaining"`
			Reset     int64 `json:"reset"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &rl); err != nil {
		return nil, fmt.Errorf("github: decode /rate_limit: %w", err)
	}
	out := make(map[string]quota.Bucket, len(rl.Resources))
	for name, r := range rl.Resources {
		out[name] = quota.Bucket{Remaining: r.Remaining, Limit: r.Limit, Reset: time.Unix(r.Reset, 0), Known: true}
	}
	return out, nil
}

func printQuota(cmd *cobra.Command, buckets map[string]quota.Bucket, probed bool) error {
	format, err := cmd.Flags().GetString("format")
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if format == "text" {
		for _, name := range quotaResources {
			b := buckets[name]
			if !b.Known {
				_, err = fmt.Fprintf(out, "%-8s unknown\n", name)
			} else {
				_, err = fmt.Fprintf(out, "%-8s %d/%d reset=%s\n", name, b.Remaining, b.Limit, b.Reset.Format(time.RFC3339))
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	v := map[string]any{"schema_version": 1, "meta": map[string]any{"probed": probed}}
	for _, name := range quotaResources {
		b := buckets[name]
		v[name] = map[string]any{"remaining": b.Remaining, "limit": b.Limit, "reset": b.Reset, "known": b.Known}
	}
	return json.NewEncoder(out).Encode(v)
}
