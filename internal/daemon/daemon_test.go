package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

const (
	helperEnv  = "OCTOGUARD_TEST_DAEMON"
	pidFileEnv = "OCTOGUARD_TEST_PIDFILE"
)

// TestMain doubles as the daemon binary for the autostart test.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runHelperDaemon())
	}
	os.Exit(m.Run())
}

func runHelperDaemon() int {
	onListening = func() {
		f, err := os.OpenFile(os.Getenv(pidFileEnv), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = fmt.Fprintln(f, os.Getpid())
			_ = f.Close()
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err := Run(ctx, Config{IdleTimeout: 30 * time.Second})
	if err != nil && !errors.Is(err, ErrAlreadyRunning) {
		return 1
	}
	return 0
}

// isolate points the socket and the state directory at short temporary
// directories (macOS caps Unix socket paths at 104 bytes).
func isolate(t *testing.T) (runtimeDir, state string) {
	t.Helper()
	mk := func() string {
		d, err := os.MkdirTemp("/tmp", "og")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		return d
	}
	runtimeDir, home := mk(), mk()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	state, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	return runtimeDir, state
}

// startDaemon runs a daemon in-process and waits until it accepts clients.
// The returned function stops it and waits for Run to return.
func startDaemon(t *testing.T, cfg Config) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	waitReachable(t)
	var once sync.Once
	var runErr error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(5 * time.Second):
				runErr = errors.New("daemon did not stop")
			}
		})
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func waitReachable(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := DialRunning(context.Background())
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon not reachable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func call(t *testing.T, req Request) Response {
	t.Helper()
	c, err := DialRunning(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Call(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestDaemon_ConcurrentAutostartSingleInstance(t *testing.T) {
	isolate(t)
	pidFile := filepath.Join(t.TempDir(), "pids")
	daemonCommand = func() (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), helperEnv+"=1", pidFileEnv+"="+pidFile)
		return cmd, nil
	}
	t.Cleanup(func() {
		daemonCommand = defaultDaemonCommand
		for _, pid := range readPIDs(t, pidFile) {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			c, err := DialRunning(context.Background())
			if err != nil {
				return
			}
			_ = c.Close()
			time.Sleep(20 * time.Millisecond)
		}
	})

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := Dial(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = c.Close() }()
			resp, err := c.Call(ctx, Request{Op: OpStatus})
			if err == nil && !resp.OK {
				err = errors.New(resp.Err)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("client: %v", err)
	}
	// Give any extra daemon spawned by the race time to lose the lock.
	time.Sleep(500 * time.Millisecond)
	if pids := readPIDs(t, pidFile); len(pids) != 1 {
		t.Errorf("daemons that took the lock = %v, want exactly 1", pids)
	}
}

var defaultDaemonCommand = daemonCommand

func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	return pids
}

const prGraphQL = `{"data":{"rateLimit":{"cost":1},"repository":{"pullRequest":{
	"number":2,"state":"OPEN","merged":false,"mergeable":"MERGEABLE","headRefOid":"abc","baseRefName":"main",
	"reviews":{"nodes":[]},
	"commits":{"nodes":[{"commit":{"oid":"abc","statusCheckRollup":null}}]}}}}}`

var prRef = ref.Ref{Host: ref.Host, Owner: "o", Repo: "r", Number: 2, Kind: ref.PR}

func getArgs(t *testing.T, a GetArgs) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDaemon_SingleFlightDedup(t *testing.T) {
	_, state := isolate(t)
	srv := fakegh.New(t)
	srv.SetResource("/graphql", []byte(prGraphQL), "")
	// Slow GitHub down so the five reads overlap.
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(slow.Close)
	stop := startDaemon(t, Config{IdleTimeout: time.Minute, APIURL: slow.URL})

	const token = "dedup-test-token"
	args := getArgs(t, GetArgs{Ref: prRef, What: gh.PR})
	const n = 5
	var wg sync.WaitGroup
	start := make(chan struct{})
	resps := make([]Response, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resps[i] = call(t, Request{Op: OpGet, Token: token, Args: args})
		}()
	}
	close(start)
	wg.Wait()
	for i, r := range resps {
		if !r.OK {
			t.Fatalf("get %d: %s", i, r.Err)
		}
	}
	if got := srv.GraphQLCalls(); got != 1 {
		t.Errorf("GitHub calls = %d, want 1", got)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	ch, err := cache.Open(filepath.Join(state, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ch.Close() }()
	buckets, err := ch.Counters(gh.Scope(ref.Host, auth.FromValue(token)), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var dedup int64
	for _, b := range buckets {
		if b.Name == CounterDedup {
			dedup += b.N
		}
	}
	if dedup != n-1 {
		t.Errorf("dedup = %d, want %d", dedup, n-1)
	}
}

func TestDaemon_IdleShutdown(t *testing.T) {
	runtimeDir, _ := isolate(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Config{IdleTimeout: 200 * time.Millisecond}) }()
	waitReachable(t)
	// A connected client keeps the daemon alive past the timeout.
	c, err := DialRunning(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := c.Call(context.Background(), Request{Op: OpStatus}); err != nil {
		t.Fatalf("daemon stopped with a client connected: %v", err)
	}
	_ = c.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after the idle timeout")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "octoguard.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still present after shutdown: %v", err)
	}
}

func TestDaemon_SocketPerms0600(t *testing.T) {
	_, state := isolate(t)
	// Without XDG_RUNTIME_DIR the socket lives in the state directory.
	t.Setenv("XDG_RUNTIME_DIR", "")
	startDaemon(t, Config{IdleTimeout: time.Minute})

	sock := filepath.Join(state, "octoguard.sock")
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want socket 0600", st.Mode())
	}
	dst, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if dst.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", dst.Mode().Perm())
	}
}

func TestDaemon_TokenNotPersisted(t *testing.T) {
	_, state := isolate(t)
	srv := fakegh.New(t)
	srv.SetResource("/repos/o/r/issues/1", []byte(`{"number":1,"title":"bug"}`), `"v1"`)
	srv.FailNext("/repos/o/r/issues/9", http.StatusUnauthorized, 0)
	stop := startDaemon(t, Config{IdleTimeout: time.Minute, APIURL: srv.URL})

	const token = "ghp_SuperSecretTokenValue123"
	issue := ref.Ref{Host: ref.Host, Owner: "o", Repo: "r", Number: 1, Kind: ref.Issue}
	for _, r := range []Request{
		{Op: OpGet, Token: token, Args: getArgs(t, GetArgs{Ref: issue, What: gh.Issue})},
		{Op: OpGet, Token: token, Args: getArgs(t, GetArgs{Ref: issue, What: gh.Issue, Fresh: true})},
		{Op: OpGet, Token: token, Args: getArgs(t, GetArgs{Ref: ref.Ref{Host: ref.Host, Owner: "o", Repo: "r", Number: 9, Kind: ref.Issue}, What: gh.Issue})},
		{Op: OpQuota, Token: token},
		{Op: "bogus", Token: token},
	} {
		resp := call(t, r)
		if strings.Contains(resp.Err, token) || bytes.Contains(resp.Data, []byte(token)) {
			t.Errorf("response to %q leaks the token", r.Op)
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	var checked []string
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSocket != 0 {
			continue
		}
		f, err := os.Open(filepath.Join(state, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		checked = append(checked, e.Name())
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("%s contains the token", e.Name())
		}
	}
	for _, want := range []string{"cache.db", "daemon.log"} {
		if !contains(checked, want) {
			t.Errorf("%s was not checked (files: %v)", want, checked)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
