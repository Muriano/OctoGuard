package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muriano/OctoGuard/internal/daemon"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run or inspect the local octoguard daemon",
	}
	cmd.AddCommand(newDaemonRunCmd(), newDaemonStatusCmd(), newDaemonStopCmd())
	return cmd
}

func newDaemonRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground (the CLI starts it on demand)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiURL, err := cmd.Flags().GetString("api-url")
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			err = daemon.Run(ctx, daemon.Config{IdleTimeout: 10 * time.Minute, APIURL: apiURL, Version: Version})
			if errors.Is(err, daemon.ErrAlreadyRunning) {
				_, err = fmt.Fprintln(cmd.ErrOrStderr(), "octoguard: daemon already running")
				return err
			}
			return err
		},
	}
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the daemon is running (does not start it)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, running, err := daemonStatus(cmd.Context())
			if err != nil {
				return err
			}
			return printDaemon(cmd, running, st)
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon if it is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			st, running, err := daemonStatus(ctx)
			if err != nil {
				return err
			}
			if running {
				if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil {
					return fmt.Errorf("stop daemon (pid %d): %w", st.PID, err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, still, _ := daemonStatus(ctx); !still {
						break
					}
					if time.Now().After(deadline) {
						return fmt.Errorf("daemon (pid %d) did not stop within 5s", st.PID)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			return printDaemon(cmd, false, daemon.Status{})
		},
	}
}

// daemonStatus asks a running daemon for its status; running is false when no
// daemon answers.
func daemonStatus(ctx context.Context) (st daemon.Status, running bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := daemon.DialRunning(ctx)
	if err != nil {
		return daemon.Status{}, false, nil
	}
	defer func() { _ = conn.Close() }()
	resp, err := conn.Call(ctx, daemon.Request{Op: daemon.OpStatus})
	if err != nil {
		return daemon.Status{}, false, err
	}
	if !resp.OK {
		return daemon.Status{}, false, errors.New(resp.Err)
	}
	if err := json.Unmarshal(resp.Data, &st); err != nil {
		return daemon.Status{}, false, fmt.Errorf("decode daemon status: %w", err)
	}
	return st, true, nil
}

func printDaemon(cmd *cobra.Command, running bool, st daemon.Status) error {
	format, err := cmd.Flags().GetString("format")
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if format == "text" {
		if !running {
			_, err = fmt.Fprintln(out, "daemon: not running")
			return err
		}
		_, err = fmt.Fprintf(out, "daemon: running pid=%d version=%s since=%s connections=%d\n",
			st.PID, st.Version, st.StartedAt.Format(time.RFC3339), st.Connections)
		return err
	}
	v := map[string]any{"schema_version": 1, "running": running}
	if running {
		v["pid"], v["version"], v["started_at"], v["connections"] = st.PID, st.Version, st.StartedAt, st.Connections
	}
	return json.NewEncoder(out).Encode(v)
}
