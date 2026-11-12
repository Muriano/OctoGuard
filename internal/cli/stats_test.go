package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

const statsTestToken = "stats-test-token"

// statsEnv isolates the cache, the socket and the token, and seeds the
// counters of the test token. It returns the runtime directory.
func statsEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	runtimeDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("GH_TOKEN", statsTestToken)
	t.Setenv("COLUMNS", "")

	dir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(filepath.Join(dir, "octoguard", "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	scope := gh.Scope(ref.Host, auth.FromValue(statsTestToken))
	if err := c.IncCounter(scope, gh.CounterCacheHit, 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.IncCounter(scope, gh.CounterRESTCall, 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	return runtimeDir
}

func runStatsCmd(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"stats"}, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func TestStats_DefaultIsText(t *testing.T) {
	statsEnv(t)
	out, err := runStatsCmd(t, context.Background())
	if err != nil {
		t.Fatalf("stats: %v\n%s", err, out)
	}
	if json.Valid([]byte(out)) {
		t.Fatalf("default output is JSON, want the text panel:\n%s", out)
	}
	for _, want := range []string{"OctoGuard stats · last 24h", "Saved  5 of 7 calls", "core      unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("non-terminal output has ANSI codes:\n%q", out)
	}
}

func TestStats_FormatJSONExplicit(t *testing.T) {
	statsEnv(t)
	out, err := runStatsCmd(t, context.Background(), "--format", "json", "--since", "7d")
	if err != nil {
		t.Fatalf("stats: %v\n%s", err, out)
	}
	var doc struct {
		SchemaVersion int    `json:"schema_version"`
		Since         string `json:"since"`
		Baseline      string `json:"baseline"`
		Saved         struct {
			Cache      int64 `json:"cache"`
			TotalCalls int64 `json:"total_calls"`
		} `json:"saved"`
		Hourly []json.RawMessage `json:"hourly"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if doc.SchemaVersion != 1 || doc.Since != "7d" || doc.Baseline != "30s" {
		t.Errorf("header = %+v", doc)
	}
	if doc.Saved.Cache != 5 || doc.Saved.TotalCalls != 7 || len(doc.Hourly) != 168 {
		t.Errorf("saved = %+v, hours = %d", doc.Saved, len(doc.Hourly))
	}
}

func TestStats_InvalidSince(t *testing.T) {
	statsEnv(t)
	if _, err := runStatsCmd(t, context.Background(), "--since", "2h"); err == nil {
		t.Fatal("expected an error for --since 2h")
	}
}

func TestStatsLive_NoNetwork(t *testing.T) {
	runtimeDir := statsEnv(t)
	fake := fakegh.New(t)
	prev := statsInterval
	statsInterval = 20 * time.Millisecond
	t.Cleanup(func() { statsInterval = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out, err := runStatsCmd(t, ctx, "--live", "--api-url", fake.URL)
	if err != nil {
		t.Fatalf("stats --live: %v\n%s", err, out)
	}
	if n := strings.Count(out, "OctoGuard stats"); n < 2 {
		t.Errorf("--live rendered %d times, want at least 2", n)
	}
	for _, path := range []string{"/", "/rate_limit", "/user"} {
		if n := fake.Calls(path); n != 0 {
			t.Errorf("fakegh Calls(%q) = %d, want 0", path, n)
		}
	}
	if n := fake.GraphQLCalls(); n != 0 {
		t.Errorf("fakegh GraphQLCalls = %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "octoguard.sock")); err == nil {
		t.Error("stats started the daemon; it must only read local data")
	}
}
