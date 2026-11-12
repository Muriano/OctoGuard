package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

const issueBody = `{"number":1,"title":"bug"}`

// isolate points the daemon socket, the state directory and the token at
// temporary values (short paths: macOS caps Unix socket paths at 104 bytes).
func isolate(t *testing.T) {
	t.Helper()
	mk := func() string {
		d, err := os.MkdirTemp("/tmp", "og")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		return d
	}
	home := mk()
	t.Setenv("XDG_RUNTIME_DIR", mk())
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("GH_TOKEN", "test-token")
}

// setDial replaces the daemon dialer for the test.
func setDial(t *testing.T, f func(context.Context) (*daemon.Conn, error)) {
	t.Helper()
	prev := dialDaemon
	dialDaemon = f
	t.Cleanup(func() { dialDaemon = prev })
}

func noDaemon(context.Context) (*daemon.Conn, error) {
	return nil, errors.New("daemon: test: unavailable")
}

// startDaemon runs a daemon in-process against apiURL until the test ends.
func startDaemon(t *testing.T, apiURL string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, daemon.Config{IdleTimeout: time.Minute, APIURL: apiURL}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := daemon.DialRunning(context.Background())
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

// run executes octoguard with args and returns stdout, stderr and the error.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewRoot()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

type getOutput struct {
	SchemaVersion int             `json:"schema_version"`
	Ref           string          `json:"ref"`
	Data          json.RawMessage `json:"data"`
	Meta          struct {
		FromCache   bool      `json:"from_cache"`
		NotModified bool      `json:"not_modified"`
		Via         string    `json:"via"`
		FetchedAt   time.Time `json:"fetched_at"`
	} `json:"meta"`
}

func decodeGet(t *testing.T, stdout string) getOutput {
	t.Helper()
	var g getOutput
	if err := json.Unmarshal([]byte(stdout), &g); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	return g
}

func TestGetCLI_UsesDaemon(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResource("/repos/o/r/issues/1", []byte(issueBody), `"v1"`)
	startDaemon(t, srv.URL)
	setDial(t, daemon.DialRunning)

	// The direct path points at a dead URL: only the daemon can answer.
	stdout, stderr, err := run(t, "--api-url", "http://127.0.0.1:1", "get", "issue", "o/r#1")
	if err != nil {
		t.Fatalf("get: %v (stderr %q)", err, stderr)
	}
	if strings.Contains(stderr, "daemon unavailable") {
		t.Fatalf("fell back with a running daemon: %q", stderr)
	}
	g := decodeGet(t, stdout)
	if g.SchemaVersion != 1 || g.Ref != "github.com/o/r/issue/1" || string(g.Data) != issueBody {
		t.Fatalf("unexpected output: %s", stdout)
	}
	if g.Meta.Via != "rest" || g.Meta.FromCache || g.Meta.FetchedAt.IsZero() {
		t.Fatalf("unexpected meta: %+v", g.Meta)
	}
	if n := srv.Calls("/repos/o/r/issues/1"); n != 1 {
		t.Fatalf("fakegh calls = %d, want 1", n)
	}

	// The daemon's cache answers a read within --max-age without a request.
	stdout, _, err = run(t, "--api-url", "http://127.0.0.1:1", "get", "issue", "o/r#1", "--max-age", "1m")
	if err != nil {
		t.Fatal(err)
	}
	if g := decodeGet(t, stdout); !g.Meta.FromCache {
		t.Fatalf("second read not from cache: %+v", g.Meta)
	}
	if n := srv.Calls("/repos/o/r/issues/1"); n != 1 {
		t.Fatalf("fakegh calls = %d, want 1", n)
	}
}

func TestGetCLI_FallbackWhenDaemonUnavailable(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResource("/repos/o/r/issues/1", []byte(issueBody), `"v1"`)
	setDial(t, noDaemon)

	stdout, stderr, err := run(t, "--api-url", srv.URL, "get", "issue", "o/r#1")
	if err != nil {
		t.Fatalf("get: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "octoguard: daemon unavailable, direct call") {
		t.Fatalf("stderr %q lacks the fallback notice", stderr)
	}
	g := decodeGet(t, stdout)
	if g.SchemaVersion != 1 || string(g.Data) != issueBody || g.Meta.Via != "rest" {
		t.Fatalf("unexpected output: %s", stdout)
	}
	if n := srv.Calls("/repos/o/r/issues/1"); n != 1 {
		t.Fatalf("fakegh calls = %d, want 1", n)
	}
	if strings.Contains(stdout+stderr, "test-token") {
		t.Fatal("token leaked to the output")
	}
}

func TestGetCLI_UsageErrors(t *testing.T) {
	isolate(t)
	setDial(t, noDaemon)
	for _, args := range [][]string{
		{"get", "nope", "o/r#1"},
		{"get", "pr"},
		{"get", "pr", "https://ghe.example.com/o/r/pull/1"},
	} {
		if _, _, err := run(t, args...); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}
