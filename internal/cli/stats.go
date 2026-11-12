package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/stats"
)

// statsInterval is the --live refresh period (a variable for tests).
var statsInterval = 2 * time.Second

var statsWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

func newStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show quota use and the GitHub requests octoguard saved",
		Long: "Show quota use and the GitHub requests octoguard saved, as a panel for a person.\n" +
			"It only reads local data (the cache database and a running daemon): it never calls GitHub\n" +
			"and does not start the daemon. Pass --format json for scripts and agents.",
		Args: cobra.NoArgs,
		RunE: runStats,
	}
	cmd.Flags().String("since", "24h", "window to report: 1h|24h|7d|30d")
	cmd.Flags().Bool("live", false, "refresh every 2s from local data only (no GitHub requests)")
	return cmd
}

// statsOutput is the --format json document.
type statsOutput struct {
	SchemaVersion int    `json:"schema_version"`
	Since         string `json:"since"`
	Baseline      string `json:"baseline"`
	stats.Report
}

func runStats(cmd *cobra.Command, _ []string) error {
	flags := cmd.Flags()
	sinceFlag, err := flags.GetString("since")
	if err != nil {
		return err
	}
	since, ok := statsWindows[sinceFlag]
	if !ok {
		return fmt.Errorf("invalid --since %q: must be 1h, 24h, 7d or 30d", sinceFlag)
	}
	live, err := flags.GetBool("live")
	if err != nil {
		return err
	}
	format, err := flags.GetString("format")
	if err != nil {
		return err
	}
	// stats is meant to be looked at: the panel is the default and JSON is
	// only for an explicit --format json, unlike the global JSON default.
	asJSON := flags.Changed("format") && format == "json"

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	tok, err := auth.Resolve(ctx)
	if err != nil {
		return err
	}
	scope := gh.Scope(ref.Host, tok)

	out := cmd.OutOrStdout()
	tty := isTerminal(out)
	color := tty && os.Getenv("NO_COLOR") == ""

	show := func() error {
		buckets, err := readCounters(scope, time.Now().Add(-since))
		if err != nil {
			return err
		}
		r := stats.Compute(buckets, quotaSnapshot(ctx, tok), since)
		if asJSON {
			return json.NewEncoder(out).Encode(statsOutput{
				SchemaVersion: 1, Since: sinceFlag, Baseline: r.Baseline.String(), Report: r,
			})
		}
		if live && tty {
			_, _ = io.WriteString(out, "\x1b[H\x1b[2J") // home and clear screen
		}
		stats.Render(out, r, termWidth(out), color)
		return nil
	}

	if !live {
		return show()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	tick := time.NewTicker(statsInterval)
	defer tick.Stop()
	for {
		if err := show(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// readCounters reads the hourly counters of scope from the cache database.
// With no database yet there is nothing to report, and none is created.
func readCounters(scope string, from time.Time) ([]cache.Bucket, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "octoguard", "cache.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	c, err := cache.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return c.Counters(scope, from)
}

// quotaSnapshot asks a running daemon for the quota it has seen. It never
// starts the daemon: with none running the quota is reported as unknown.
func quotaSnapshot(ctx context.Context, tok auth.Token) map[string]quota.Bucket {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := daemon.DialRunning(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = conn.Close() }()
	resp, err := conn.Call(ctx, daemon.Request{Op: daemon.OpQuota, Token: tok.Value})
	if err != nil || !resp.OK {
		return nil
	}
	var snap map[string]quota.Bucket
	if json.Unmarshal(resp.Data, &snap) != nil {
		return nil
	}
	return snap
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// termWidth is the terminal width, from COLUMNS or the terminal itself, and
// 80 when unknown.
func termWidth(w io.Writer) int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	if f, ok := w.(*os.File); ok {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
			return int(ws.Col)
		}
	}
	return 80
}
