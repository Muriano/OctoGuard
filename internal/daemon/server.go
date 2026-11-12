package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/wait"
)

// ErrAlreadyRunning is returned by Run when another daemon holds the lock.
var ErrAlreadyRunning = errors.New("daemon: another octoguard daemon is already running")

// maxLine bounds the size of one request line.
const maxLine = 1 << 20

// Config configures Run.
type Config struct {
	// IdleTimeout stops the daemon once no client has been connected for
	// this long; zero means 10 minutes.
	IdleTimeout time.Duration
	// APIURL is the GitHub API base URL; empty means https://api.github.com.
	APIURL string
	// Version is reported by OpStatus.
	Version string
}

// waitConfig configures the wait pool (the governor is set by Run); a test
// hook for shorter intervals and grace.
var waitConfig = wait.Config{Grace: wait.DefaultGrace}

// onListening is a test hook, called once the daemon holds the lock and
// listens on the socket.
var onListening = func() {}

type server struct {
	cfg     Config
	ctx     context.Context
	stop    context.CancelFunc
	cache   *cache.Cache
	gov     *quota.Governor
	log     *log.Logger
	sf      singleflight.Group
	pool    *wait.Pool
	started time.Time

	mu      sync.Mutex
	clients map[string]*gh.Client // by token ID
	conns   map[net.Conn]struct{}
	idle    *time.Timer
}

// Run serves the daemon until ctx is cancelled or the idle timeout expires.
// Only one daemon runs per user: a second one returns ErrAlreadyRunning.
func Run(ctx context.Context, cfg Config) error {
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	dir, err := stateDir()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()

	logf, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() { _ = logf.Close() }()
	logger := log.New(logf, "", log.LstdFlags)

	ch, err := cache.Open(filepath.Join(dir, "cache.db"))
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() { _ = ch.Close() }()

	ln, err := listen()
	if err != nil {
		return err
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	gov := quota.New(time.Now)
	wcfg := waitConfig
	wcfg.Gov = gov
	pool := wait.NewPool(wcfg)
	defer pool.Close()
	s := &server{
		cfg:     cfg,
		ctx:     ctx,
		stop:    stop,
		cache:   ch,
		gov:     gov,
		pool:    pool,
		log:     logger,
		started: time.Now(),
		clients: map[string]*gh.Client{},
		conns:   map[net.Conn]struct{}{},
	}
	s.idle = time.AfterFunc(cfg.IdleTimeout, s.onIdle)
	defer s.idle.Stop()
	logger.Printf("started pid=%d version=%s", os.Getpid(), cfg.Version)
	onListening()

	go func() {
		<-ctx.Done()
		_ = ln.Close() // also removes the socket file
	}()
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				logger.Printf("accept: %v", err)
				stop()
			}
			break
		}
		s.track(conn, true)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.track(conn, false)
			s.serveConn(conn)
		}()
	}
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	wg.Wait()
	logger.Printf("stopped")
	return nil
}

// lock takes the per-user daemon lock for the life of the process.
func lock(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(lockPath(dir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("daemon: lock: %w", err)
	}
	return func() { _ = f.Close() }, nil
}

// listen creates the socket (0600) in a 0700 directory, replacing a stale one
// left by a crashed daemon. The caller holds the lock, so no live daemon owns
// the old file.
func listen() (net.Listener, error) {
	path, err := socketPath()
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("daemon: remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return ln, nil
}

// track registers or forgets a connection and arms the idle timer when the
// last one goes away.
func (s *server) track(c net.Conn, open bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if open {
		s.conns[c] = struct{}{}
		s.idle.Stop()
		return
	}
	delete(s.conns, c)
	_ = c.Close()
	if len(s.conns) == 0 {
		s.idle.Reset(s.cfg.IdleTimeout)
	}
}

func (s *server) onIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) > 0 {
		return
	}
	s.log.Printf("idle for %s, shutting down", s.cfg.IdleTimeout)
	s.stop()
}

// serveConn answers the requests of one client, one at a time. Lines are read
// in the background so that a client hanging up is noticed during a long
// request: it cancels the connection's context, which releases its waits.
func (s *server) serveConn(c net.Conn) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	lines := make(chan []byte)
	go func() {
		defer cancel()
		defer close(lines)
		sc := bufio.NewScanner(c)
		sc.Buffer(make([]byte, 0, 64<<10), maxLine)
		for sc.Scan() {
			select {
			case lines <- append([]byte(nil), sc.Bytes()...):
			case <-ctx.Done():
				return
			}
		}
	}()
	enc := json.NewEncoder(c)
	for line := range lines {
		var req Request
		var resp Response
		if err := json.Unmarshal(line, &req); err != nil {
			resp = failure(CodeUsage, "malformed request")
		} else {
			resp = s.handle(ctx, req)
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *server) handle(ctx context.Context, req Request) Response {
	switch req.Op {
	case OpStatus:
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		pollers, waiters := s.pool.Stats()
		return success(Status{
			PID: os.Getpid(), Version: s.cfg.Version, StartedAt: s.started, Connections: n,
			Pollers: pollers, Waiters: waiters,
		})
	case OpGet:
		return s.get(req)
	case OpQuota:
		if req.Token == "" {
			return failure(CodeUsage, "missing token")
		}
		return success(s.gov.Snapshot(gh.Scope(ref.Host, auth.FromValue(req.Token))))
	case OpWait:
		return s.wait(ctx, req)
	case OpWrite, OpStats:
		return failure(CodeError, fmt.Sprintf("operation %q is not implemented yet", req.Op))
	default:
		return failure(CodeUsage, fmt.Sprintf("unknown operation %q", req.Op))
	}
}

// get reads through the token's client. Identical reads in flight are joined:
// only the first one reaches GitHub and the rest count as dedup.
func (s *server) get(req Request) Response {
	if req.Token == "" {
		return failure(CodeUsage, "missing token")
	}
	var a GetArgs
	if err := json.Unmarshal(req.Args, &a); err != nil {
		return failure(CodeUsage, "malformed get arguments")
	}
	if a.Ref.Host != ref.Host || a.Ref.Owner == "" || a.Ref.Repo == "" || a.Ref.Number <= 0 {
		return failure(CodeUsage, "invalid ref: v1 supports github.com owner/repo#number")
	}
	tok := auth.FromValue(req.Token)
	scope := gh.Scope(a.Ref.Host, tok)
	key := scope + "|" + a.Ref.Key() + "|" + string(a.What) + "|" + strconv.FormatBool(a.Fresh) + "|" + a.MaxAge.String()

	leader := false
	v, err, _ := s.sf.Do(key, func() (any, error) {
		leader = true
		return s.client(tok).Get(s.ctx, a.Ref, a.What, gh.GetOpts{Fresh: a.Fresh, MaxAge: a.MaxAge})
	})
	if !leader {
		// Best effort, like the client's own counters.
		_ = s.cache.IncCounter(scope, CounterDedup, 1, time.Now())
	}
	if err != nil {
		code := CodeError
		if errors.Is(err, ref.ErrUsage) {
			code = CodeUsage
		}
		return failure(code, err.Error())
	}
	return success(v.(gh.Result))
}

// wait blocks until the condition is met, can no longer be met or the
// timeout expires, or until the client hangs up (ctx).
func (s *server) wait(ctx context.Context, req Request) Response {
	if req.Token == "" {
		return failure(CodeUsage, "missing token")
	}
	var a WaitArgs
	if err := json.Unmarshal(req.Args, &a); err != nil {
		return failure(CodeUsage, "malformed wait arguments")
	}
	if a.Ref.Host != ref.Host || a.Ref.Owner == "" || a.Ref.Repo == "" || a.Ref.Number <= 0 {
		return failure(CodeUsage, "invalid ref: v1 supports github.com owner/repo#number")
	}
	if _, err := wait.ParseCondition(a.Ref.Kind, string(a.Condition)); err != nil {
		return failure(CodeUsage, err.Error())
	}
	if a.Timeout <= 0 {
		return failure(CodeUsage, "timeout must be positive")
	}
	tok := auth.FromValue(req.Token)
	c := s.client(tok)
	res, err := s.pool.Wait(ctx, wait.Request{
		Scope: gh.Scope(a.Ref.Host, tok), Ref: a.Ref, Cond: a.Condition, Timeout: a.Timeout,
		Fetch: func(ctx context.Context, r ref.Ref, what gh.Resource) (json.RawMessage, error) {
			res, err := c.Get(ctx, r, what, gh.GetOpts{})
			return res.Data, err
		},
	})
	if err != nil {
		code := CodeError
		if errors.Is(err, ref.ErrUsage) {
			code = CodeUsage
		}
		return failure(code, err.Error())
	}
	return success(res)
}

// client returns the GitHub client of tok, creating it on first use.
func (s *server) client(tok auth.Token) *gh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[tok.ID]
	if !ok {
		c = gh.NewClient(s.cfg.APIURL, tok, s.cache, s.gov, s.cache)
		s.clients[tok.ID] = c
	}
	return c
}

func success(v any) Response {
	data, err := json.Marshal(v)
	if err != nil {
		return failure(CodeError, "encode response: "+err.Error())
	}
	return Response{OK: true, Data: data, Code: CodeOK}
}

func failure(code int, msg string) Response {
	return Response{OK: false, Err: msg, Code: code}
}
