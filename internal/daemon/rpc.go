// Package daemon is OctoGuard's local daemon: it owns the cache, deduplicates
// in-flight reads across every CLI on the machine and talks to them over a
// Unix socket with line-delimited JSON.
package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/wait"
)

// Operations understood by the daemon.
const (
	OpGet    = "get"
	OpWait   = "wait"
	OpWrite  = "write"
	OpQuota  = "quota"
	OpStats  = "stats"
	OpStatus = "status"
)

// Response codes, matching the CLI exit codes.
const (
	CodeOK    = 0
	CodeError = 1
	CodeUsage = 2
)

// CounterDedup counts reads answered by joining an identical read in flight.
const CounterDedup = "dedup"

// Request is one line sent by a client. Token is the raw GitHub token: it only
// lives in the daemon's memory and is never written anywhere.
type Request struct {
	Op    string          `json:"op"`
	Token string          `json:"token,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"`
}

// Response is the daemon's answer to one Request.
type Response struct {
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data,omitempty"`
	Err  string          `json:"err,omitempty"`
	Code int             `json:"code"`
}

// GetArgs are the arguments of OpGet.
type GetArgs struct {
	Ref    ref.Ref       `json:"ref"`
	What   gh.Resource   `json:"what"`
	Fresh  bool          `json:"fresh,omitempty"`
	MaxAge time.Duration `json:"max_age,omitempty"`
}

// WaitArgs are the arguments of OpWait. The data of the answer is a
// wait.Result.
type WaitArgs struct {
	Ref       ref.Ref        `json:"ref"`
	Condition wait.Condition `json:"condition"`
	Timeout   time.Duration  `json:"timeout"`
}

// Status is the data of OpStatus.
type Status struct {
	PID         int       `json:"pid"`
	Version     string    `json:"version"`
	StartedAt   time.Time `json:"started_at"`
	Connections int       `json:"connections"`
	Pollers     int       `json:"pollers"`
	Waiters     int       `json:"waiters"`
}

// stateDir is where the cache, the lock and the log live (and the socket when
// there is no XDG_RUNTIME_DIR).
func stateDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "octoguard"), nil
}

// socketPath is $XDG_RUNTIME_DIR/octoguard.sock, or the state directory when
// XDG_RUNTIME_DIR is unset, missing or not private to the user (a shared
// directory would let another user plant a socket and receive the token).
func socketPath() (string, error) {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && privateDir(d) {
		return filepath.Join(d, "octoguard.sock"), nil
	}
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "octoguard.sock"), nil
}

// privateDir reports whether dir is a directory owned by the current user and
// not writable by group or others.
func privateDir(dir string) bool {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o022 != 0 {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid()
}

func lockPath(dir string) string { return filepath.Join(dir, "octoguard.lock") }
