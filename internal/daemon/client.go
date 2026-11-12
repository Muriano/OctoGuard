package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// autostartWait is how long Dial waits for an autostarted daemon.
const autostartWait = 3 * time.Second

// daemonCommand builds the command that starts a daemon; a test hook.
var daemonCommand = func() (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(exe, "daemon", "run"), nil
}

// Conn is a connection to the daemon. Calls on one Conn are serialized.
type Conn struct {
	mu sync.Mutex
	c  net.Conn
	r  *bufio.Reader
}

// Dial connects to the daemon, starting it in the background if it is not
// running, and gives up after 3 seconds (or when ctx ends).
func Dial(ctx context.Context) (*Conn, error) {
	if c, err := DialRunning(ctx); err == nil {
		return c, nil
	}
	if err := ensureStarted(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, autostartWait)
	defer cancel()
	backoff := 10 * time.Millisecond
	for {
		c, err := DialRunning(ctx)
		if err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("daemon: not reachable after autostart: %w", err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 200*time.Millisecond)
	}
}

// DialRunning connects to a running daemon without starting one.
func DialRunning(ctx context.Context) (*Conn, error) {
	path, err := socketPath()
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return &Conn{c: c, r: bufio.NewReader(c)}, nil
}

// ensureStarted launches a detached daemon unless one holds the lock (running
// or starting). If several clients race, the extra daemons fail to take the
// lock and exit, leaving exactly one.
func ensureStarted() error {
	dir, err := stateDir()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	unlock, err := lock(dir)
	if errors.Is(err, ErrAlreadyRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	unlock()

	cmd, err := daemonCommand()
	if err != nil {
		return fmt.Errorf("daemon: autostart: %w", err)
	}
	// A new session detaches the daemon from the CLI's terminal and signals;
	// stdio stays on /dev/null.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("daemon: autostart: %w", err)
	}
	go func() { _ = cmd.Wait() }() // reap it if it exits while we live
	return nil
}

// Call sends req and returns the daemon's response.
func (c *Conn) Call(ctx context.Context, req Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline, _ := ctx.Deadline()
	if err := c.c.SetDeadline(deadline); err != nil {
		return Response{}, fmt.Errorf("daemon: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.c.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	line, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("daemon: %w", err)
	}
	if _, err := c.c.Write(append(line, '\n')); err != nil {
		return Response{}, fmt.Errorf("daemon: %w", err)
	}
	reply, err := c.r.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("daemon: %w", ctx.Err())
		}
		return Response{}, fmt.Errorf("daemon: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(reply, &resp); err != nil {
		return Response{}, fmt.Errorf("daemon: decode response: %w", err)
	}
	return resp, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }
