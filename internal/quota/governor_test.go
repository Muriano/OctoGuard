package quota

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

const tok = "github.com:0123456789abcdef"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newGov() (*Governor, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	return New(c.now), c
}

func headers(resource string, remaining, limit int, reset time.Time) http.Header {
	h := http.Header{}
	if resource != "" {
		h.Set("X-RateLimit-Resource", resource)
	}
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Used", strconv.Itoa(limit-remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

func TestPollInterval_Thresholds(t *testing.T) {
	const base = 10 * time.Second
	cases := []struct {
		name      string
		remaining int
		want      time.Duration
	}{
		{"full", 5000, base},
		{"just above 50%", 2501, base},
		{"exactly 50%", 2500, 2 * base},
		{"just above 20%", 1001, 2 * base},
		{"exactly 20%", 1000, 4 * base},
		{"exactly 5%", 250, 4 * base},
		// Below 5%: max(base×4, secondsToReset/remaining) = max(40s, 3600s/200).
		{"below 5%, still base×4", 200, 4 * base},
		// 3600s/60 = 60s > 40s.
		{"below 5%, paced to reset", 60, 60 * time.Second},
		{"exhausted waits for reset", 0, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, c := newGov()
			g.Observe(tok, headers("core", tc.remaining, 5000, c.now().Add(time.Hour)))
			if got := g.PollInterval(tok, "core", base); got != tc.want {
				t.Fatalf("PollInterval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllow_Thresholds(t *testing.T) {
	cases := []struct {
		name      string
		remaining int
		cost      int
		want      time.Duration
	}{
		{"above 50%", 4000, 1, 0},
		{"between 50% and 20%", 1500, 1, 0},
		{"exactly 5%", 250, 1, 0},
		{"cost above remaining waits for reset", 5, 10, time.Hour},
		{"critical, uncached read waits for reset", 100, 1, time.Hour},
		{"critical, cached revalidation passes", 100, 0, 0},
		{"exhausted, cached revalidation waits for reset", 0, 0, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, c := newGov()
			g.Observe(tok, headers("core", tc.remaining, 5000, c.now().Add(time.Hour)))
			if got := g.Allow(tok, "core", tc.cost); got != tc.want {
				t.Fatalf("Allow = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllow_CriticalPacesRevalidations(t *testing.T) {
	g, c := newGov()
	// 100 left for 1000s: one revalidation every 10s.
	g.Observe(tok, headers("core", 100, 5000, c.now().Add(1000*time.Second)))

	if got := g.Allow(tok, "core", 0); got != 0 {
		t.Fatalf("first Allow = %v, want 0", got)
	}
	if got := g.Allow(tok, "core", 0); got != 10*time.Second {
		t.Fatalf("second Allow = %v, want 10s", got)
	}
	// The pace follows the time left: 996s/100 = 9.96s since the last grant.
	c.advance(4 * time.Second)
	if got := g.Allow(tok, "core", 0); got != 5960*time.Millisecond {
		t.Fatalf("Allow after 4s = %v, want 5.96s", got)
	}
	c.advance(6 * time.Second)
	if got := g.Allow(tok, "core", 0); got != 0 {
		t.Fatalf("Allow after spacing = %v, want 0", got)
	}
}

func TestRetryAfter_Respected(t *testing.T) {
	const base = 10 * time.Second
	g, c := newGov()
	g.Observe(tok, headers("core", 4900, 5000, c.now().Add(time.Hour)))
	g.ObserveRetryAfter(tok, "core", 90*time.Second)

	if got := g.Allow(tok, "core", 1); got != 90*time.Second {
		t.Fatalf("Allow = %v, want 90s", got)
	}
	if got := g.PollInterval(tok, "core", base); got != 90*time.Second {
		t.Fatalf("PollInterval = %v, want 90s", got)
	}

	// Healthy headers (secondary limits keep plenty of quota) do not lift it.
	c.advance(30 * time.Second)
	g.Observe(tok, headers("core", 4899, 5000, c.now().Add(time.Hour)))
	if got := g.Allow(tok, "core", 1); got != 60*time.Second {
		t.Fatalf("Allow after healthy Observe = %v, want 60s", got)
	}

	// A shorter Retry-After does not shorten the block.
	g.ObserveRetryAfter(tok, "core", 5*time.Second)
	if got := g.Allow(tok, "core", 1); got != 60*time.Second {
		t.Fatalf("Allow after shorter Retry-After = %v, want 60s", got)
	}

	// Other resources are unaffected.
	if got := g.Allow(tok, "graphql", 1); got != 0 {
		t.Fatalf("graphql Allow = %v, want 0", got)
	}

	c.advance(60 * time.Second)
	if got := g.Allow(tok, "core", 1); got != 0 {
		t.Fatalf("Allow after block = %v, want 0", got)
	}
	if got := g.PollInterval(tok, "core", base); got != base {
		t.Fatalf("PollInterval after block = %v, want %v", got, base)
	}
}

func TestRetryAfter_UnobservedResource(t *testing.T) {
	g, _ := newGov()
	g.ObserveRetryAfter(tok, "core", time.Minute)
	if got := g.Allow(tok, "core", 1); got != time.Minute {
		t.Fatalf("Allow = %v, want 1m", got)
	}
	if got := g.PollInterval(tok, "core", time.Second); got != time.Minute {
		t.Fatalf("PollInterval = %v, want 1m", got)
	}
	if snap := g.Snapshot(tok); len(snap) != 0 {
		t.Fatalf("Snapshot = %v, want empty", snap)
	}
}

// Snapshot works from headers alone: no server, no network.
func TestSnapshot_WithoutNetwork(t *testing.T) {
	g, c := newGov()
	reset := c.now().Add(30 * time.Minute)
	g.Observe(tok, headers("core", 4321, 5000, reset))
	g.Observe(tok, headers("graphql", 4200, 5000, reset))

	snap := g.Snapshot(tok)
	want := map[string]Bucket{
		"core":    {Remaining: 4321, Limit: 5000, Reset: reset, Known: true},
		"graphql": {Remaining: 4200, Limit: 5000, Reset: reset, Known: true},
	}
	if len(snap) != len(want) {
		t.Fatalf("Snapshot = %v, want %v", snap, want)
	}
	for k, w := range want {
		if got := snap[k]; got != w {
			t.Fatalf("Snapshot[%q] = %+v, want %+v", k, got, w)
		}
	}

	// It is a copy.
	snap["core"] = Bucket{}
	if got := g.Snapshot(tok)["core"]; got != want["core"] {
		t.Fatalf("Snapshot mutated through copy: %+v", got)
	}

	if other := g.Snapshot("github.com:ffffffffffffffff"); len(other) != 0 {
		t.Fatalf("unknown token Snapshot = %v, want empty", other)
	}
}

func TestObserve_TokensIsolated(t *testing.T) {
	g, c := newGov()
	g.Observe(tok, headers("core", 10, 5000, c.now().Add(time.Hour)))
	other := "github.com:ffffffffffffffff"
	if got := g.Allow(other, "core", 1); got != 0 {
		t.Fatalf("other token Allow = %v, want 0", got)
	}
	if got := g.PollInterval(other, "core", time.Second); got != time.Second {
		t.Fatalf("other token PollInterval = %v, want 1s", got)
	}
}

func TestObserve_Headers(t *testing.T) {
	g, c := newGov()
	reset := c.now().Add(time.Hour)

	// Without X-RateLimit-Resource the bucket is core.
	g.Observe(tok, headers("", 3000, 5000, reset))
	if got := g.Snapshot(tok)["core"]; got.Remaining != 3000 || !got.Known {
		t.Fatalf("core = %+v, want remaining 3000", got)
	}

	// Remaining falls back to Limit - Used.
	h := headers("search", 0, 30, reset)
	h.Del("X-RateLimit-Remaining")
	h.Set("X-RateLimit-Used", "12")
	g.Observe(tok, h)
	if got := g.Snapshot(tok)["search"]; got.Remaining != 18 || got.Limit != 30 {
		t.Fatalf("search = %+v, want remaining 18 of 30", got)
	}

	// Malformed or missing headers are ignored.
	for _, bad := range []http.Header{
		{},
		{"X-Ratelimit-Limit": {"x"}, "X-Ratelimit-Remaining": {"1"}, "X-Ratelimit-Reset": {"1"}},
		{"X-Ratelimit-Limit": {"10"}, "X-Ratelimit-Remaining": {"x"}, "X-Ratelimit-Reset": {"1"}},
		{"X-Ratelimit-Limit": {"10"}, "X-Ratelimit-Remaining": {"1"}, "X-Ratelimit-Reset": {"x"}},
	} {
		g.Observe(tok, bad)
	}
	if got := g.Snapshot(tok)["core"]; got.Remaining != 3000 {
		t.Fatalf("core changed by bad headers: %+v", got)
	}
	if n := len(g.Snapshot(tok)); n != 2 {
		t.Fatalf("Snapshot has %d buckets, want 2", n)
	}
}

func TestHealthyWhenUnknownZeroLimitOrReset(t *testing.T) {
	const base = 10 * time.Second

	g, c := newGov()
	if got := g.Allow(tok, "core", 1); got != 0 {
		t.Fatalf("unknown Allow = %v, want 0", got)
	}
	if got := g.PollInterval(tok, "core", base); got != base {
		t.Fatalf("unknown PollInterval = %v, want %v", got, base)
	}

	g.Observe(tok, headers("core", 0, 0, c.now().Add(time.Hour)))
	if got := g.Allow(tok, "core", 1); got != 0 {
		t.Fatalf("zero-limit Allow = %v, want 0", got)
	}
	if got := g.PollInterval(tok, "core", base); got != base {
		t.Fatalf("zero-limit PollInterval = %v, want %v", got, base)
	}

	g.Observe(tok, headers("core", 0, 5000, c.now().Add(time.Minute)))
	c.advance(time.Minute)
	if got := g.Allow(tok, "core", 1); got != 0 {
		t.Fatalf("after reset Allow = %v, want 0", got)
	}
	if got := g.PollInterval(tok, "core", base); got != base {
		t.Fatalf("after reset PollInterval = %v, want %v", got, base)
	}
}

func TestGovernor_Concurrent(t *testing.T) {
	g, c := newGov()
	reset := c.now().Add(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				g.Observe(tok, headers("core", 100+j, 5000, reset))
				g.ObserveRetryAfter(tok, "graphql", time.Millisecond)
				g.Allow(tok, "core", i%2)
				g.PollInterval(tok, "core", time.Second)
				_ = g.Snapshot(tok)
			}
		}(i)
	}
	wg.Wait()
}
