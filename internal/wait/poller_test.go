package wait

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

const issuePath = "/repos/o/r/issues/1"

var issueRef = ref.Ref{Host: ref.Host, Owner: "o", Repo: "r", Number: 1, Kind: ref.Issue}

type env struct {
	fake  *fakegh.Server
	gov   *quota.Governor
	scope string
	fetch Fetch
	polls atomic.Int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fake := fakegh.New(t)
	fake.SetResource(issuePath, []byte(`{"number":1,"state":"open","labels":[]}`), `"v1"`)
	ch, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	tok := auth.FromValue("test-token")
	gov := quota.New(time.Now)
	c := gh.NewClient(fake.URL, tok, ch, gov, nil)
	return &env{
		fake: fake, gov: gov, scope: gh.Scope(ref.Host, tok),
		fetch: func(ctx context.Context, r ref.Ref, what gh.Resource) (json.RawMessage, error) {
			res, err := c.Get(ctx, r, what, gh.GetOpts{})
			return res.Data, err
		},
	}
}

func (e *env) pool(base, grace time.Duration) *Pool {
	p := NewPool(Config{Gov: e.gov, ChecksBase: base, OtherBase: base, Grace: grace, OnPoll: func() { e.polls.Add(1) }})
	return p
}

func (e *env) req(cond Condition, timeout time.Duration) Request {
	return Request{Scope: e.scope, Ref: issueRef, Cond: cond, Timeout: timeout, Fetch: e.fetch}
}

// eventually polls f until it is true or fails the test after 3 s.
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWait_MetOnFirstSnapshot(t *testing.T) {
	e := newEnv(t)
	e.fake.SetResource(issuePath, []byte(`{"number":1,"state":"closed"}`), `"v2"`)
	p := e.pool(time.Hour, 0)
	res, err := p.Wait(context.Background(), e.req(Closed, time.Second))
	if err != nil || res.Status != StatusMet || res.State.State != "closed" || res.Polls != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestWait_FirstErrorIsReturned(t *testing.T) {
	e := newEnv(t)
	p := e.pool(time.Hour, 0)
	r := e.req(Closed, time.Second)
	r.Ref.Number = 2 // not served by fakegh
	var apiErr *gh.APIError
	if _, err := p.Wait(context.Background(), r); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("err = %v, want a 404", err)
	}
}

func TestWait_TwoWaitersOnePoller(t *testing.T) {
	e := newEnv(t)
	p := e.pool(30*time.Millisecond, 0)
	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = p.Wait(context.Background(), e.req(Closed, 5*time.Second))
		}()
	}
	eventually(t, "both waiters on one poller", func() bool {
		pollers, waiters := p.Stats()
		return pollers == 1 && waiters == 2
	})
	eventually(t, "a few cycles", func() bool { return e.polls.Load() >= 3 })
	e.fake.SetResource(issuePath, []byte(`{"number":1,"state":"closed"}`), `"v2"`)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Status != StatusMet {
			t.Fatalf("waiter %d: %+v, %v", i, results[i], errs[i])
		}
	}
	if pollers, _ := p.Stats(); pollers != 0 {
		t.Fatalf("pollers = %d after the last waiter left without grace", pollers)
	}
	if calls, cycles := e.fake.Calls(issuePath), int(e.polls.Load()); calls != cycles {
		t.Fatalf("fakegh calls = %d, want one per cycle (%d)", calls, cycles)
	}
}

func TestWait_ResumeWithinGraceNoExtraCall(t *testing.T) {
	e := newEnv(t)
	p := e.pool(time.Hour, time.Minute)
	defer p.Close()
	res, err := p.Wait(context.Background(), e.req(Closed, 50*time.Millisecond))
	if err != nil || res.Status != StatusPending || res.Polls != 1 {
		t.Fatalf("first wait: %+v, %v", res, err)
	}
	if calls := e.fake.Calls(issuePath); calls != 1 {
		t.Fatalf("calls after first wait = %d, want 1", calls)
	}
	res, err = p.Wait(context.Background(), e.req(Closed, 50*time.Millisecond))
	if err != nil || res.Status != StatusPending || res.Polls != 0 || res.State.State != "open" {
		t.Fatalf("resumed wait: %+v, %v", res, err)
	}
	if calls := e.fake.Calls(issuePath); calls != 1 {
		t.Fatalf("calls after resuming within the grace = %d, want still 1", calls)
	}
}

func TestWait_ClientDisconnectReleasesWaiter(t *testing.T) {
	e := newEnv(t)
	p := e.pool(10*time.Millisecond, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.Wait(ctx, e.req(Closed, time.Minute))
		done <- err
	}()
	eventually(t, "polling", func() bool { return e.polls.Load() >= 2 })
	cancel() // the client went away
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if pollers, waiters := p.Stats(); pollers != 1 || waiters != 0 {
		t.Fatalf("during the grace: pollers=%d waiters=%d, want 1 and 0", pollers, waiters)
	}
	eventually(t, "the poller to stop after the grace", func() bool {
		pollers, _ := p.Stats()
		return pollers == 0
	})
	calls := e.fake.Calls(issuePath)
	time.Sleep(50 * time.Millisecond)
	if after := e.fake.Calls(issuePath); after != calls {
		t.Fatalf("calls kept growing after the poller stopped: %d → %d", calls, after)
	}
}

func TestWait_ChangedReportsChanges(t *testing.T) {
	e := newEnv(t)
	e.fake.SetResource("/repos/o/r/issues/1/comments", []byte(`[]`), `"c1"`)
	p := e.pool(20*time.Millisecond, 0)
	done := make(chan Result, 1)
	go func() {
		res, _ := p.Wait(context.Background(), e.req(Comment, 5*time.Second))
		done <- res
	}()
	eventually(t, "a first cycle", func() bool { return e.polls.Load() >= 1 })
	e.fake.SetResource("/repos/o/r/issues/1/comments", []byte(`[{"id":9,"user":{"login":"bob"}}]`), `"c2"`)
	res := <-done
	if res.Status != StatusMet || len(res.Changes) != 1 || res.Changes[0] != (Change{Kind: "comment", Name: "bob"}) {
		t.Fatalf("res = %+v", res)
	}
}

func TestSaved(t *testing.T) {
	for _, tc := range []struct {
		d     time.Duration
		polls int
		want  int64
	}{
		{5 * time.Minute, 4, 6},
		{31 * time.Second, 0, 2},
		{10 * time.Second, 3, 0},
	} {
		if got := Saved(tc.d, 30*time.Second, tc.polls); got != tc.want {
			t.Errorf("Saved(%s, 30s, %d) = %d, want %d", tc.d, tc.polls, got, tc.want)
		}
	}
}

type sink struct {
	mu sync.Mutex
	n  map[string]int64
}

func (s *sink) IncCounter(_, name string, n int64, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[string]int64{}
	}
	s.n[name] += n
	return nil
}

func TestInterval_BackoffCountsThrottle(t *testing.T) {
	gov := quota.New(time.Now)
	cs := &sink{}
	p := NewPool(Config{Gov: gov, Counters: cs})
	pl := &poller{scope: "s", base: time.Second}

	if iv := p.interval(pl); iv != time.Second {
		t.Fatalf("interval = %v, want the base", iv)
	}
	if cs.n[gh.CounterThrottle] != 0 {
		t.Fatalf("throttle = %d with a healthy quota, want 0", cs.n[gh.CounterThrottle])
	}

	h := http.Header{}
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Remaining", "1000") // 20%: base×4
	h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	gov.Observe("s", h)
	if iv := p.interval(pl); iv <= time.Second {
		t.Fatalf("interval = %v, want a backoff", iv)
	}
	if cs.n[gh.CounterThrottle] != 1 {
		t.Errorf("throttle = %d, want 1", cs.n[gh.CounterThrottle])
	}
}
