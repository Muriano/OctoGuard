package wait

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
)

// Result statuses.
const (
	StatusMet     = "met"
	StatusPending = "pending" // the timeout expired first
	StatusFailed  = "failed"  // the condition can no longer be met
)

// Default intervals.
const (
	DefaultChecksBase = 15 * time.Second // checks and runs
	DefaultOtherBase  = 30 * time.Second
	DefaultGrace      = 2 * time.Minute
)

// Fetch reads one resource of r, as gh.Client.Get returns it.
type Fetch func(ctx context.Context, r ref.Ref, what gh.Resource) (json.RawMessage, error)

// Config configures a Pool. Zero durations take the defaults, except Grace
// and MinInterval, where zero means none.
type Config struct {
	// Gov scales the base interval by the remaining quota; nil uses the base.
	Gov *quota.Governor
	// ChecksBase is the base interval of checks and run conditions,
	// OtherBase the one of the rest.
	ChecksBase, OtherBase time.Duration
	// MinInterval is a floor for every interval.
	MinInterval time.Duration
	// Grace keeps a poller alive after its last waiter leaves, so that a
	// waiter coming back continues without an extra call.
	Grace time.Duration
	// OnPoll is called after every poll cycle; a test hook.
	OnPoll func()
}

// Request is one wait.
type Request struct {
	// Scope identifies the token (gh.Scope): pollers are shared per scope
	// and ref, and the governor is keyed by it.
	Scope   string
	Ref     ref.Ref
	Cond    Condition
	Timeout time.Duration
	// Fetch reads with the token of Scope. A new poller keeps the Fetch of
	// the request that created it.
	Fetch Fetch
}

// Result is the outcome of a wait.
type Result struct {
	Status    string        `json:"status"`
	Condition Condition     `json:"condition"`
	Changes   []Change      `json:"changes"`
	State     Snapshot      `json:"state"`
	Polls     int           `json:"polls"`    // poll cycles run while waiting
	Duration  time.Duration `json:"duration"` // time spent waiting
}

// Saved returns the polls a loop at baseline would have made over d, minus
// the polls actually made; never negative.
func Saved(d, baseline time.Duration, polls int) int64 {
	if baseline <= 0 {
		return 0
	}
	n := int64(math.Ceil(float64(d)/float64(baseline))) - int64(polls)
	return max(n, 0)
}

// Pool runs one poller per (scope, ref) and serves any number of waiters
// from it. It is safe for concurrent use.
type Pool struct {
	cfg Config

	mu      sync.Mutex
	pollers map[string]*poller
}

// NewPool returns a Pool configured by cfg.
func NewPool(cfg Config) *Pool {
	if cfg.ChecksBase <= 0 {
		cfg.ChecksBase = DefaultChecksBase
	}
	if cfg.OtherBase <= 0 {
		cfg.OtherBase = DefaultOtherBase
	}
	return &Pool{cfg: cfg, pollers: map[string]*poller{}}
}

type update struct {
	snap Snapshot
	err  error
}

type waiter struct {
	needs   []gh.Resource
	base    time.Duration
	updates chan update // latest wins
	polls   int         // guarded by Pool.mu
}

// poller fields other than the immutable ones are guarded by Pool.mu.
type poller struct {
	key    string
	scope  string
	ref    ref.Ref
	fetch  Fetch
	cancel context.CancelFunc
	wake   chan struct{}

	waiters map[*waiter]struct{}
	needs   []gh.Resource // of the last waiters, kept through the grace
	base    time.Duration
	snap    *Snapshot
	have    []gh.Resource // resources snap was built from
	flight  []gh.Resource // resources of the poll in flight; nil if none
	grace   *time.Timer
}

// Stats returns the number of live pollers and of waiters subscribed to them.
func (p *Pool) Stats() (pollers, waiters int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pl := range p.pollers {
		waiters += len(pl.waiters)
	}
	return len(p.pollers), waiters
}

// Close stops every poller.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, pl := range p.pollers {
		p.stopLocked(k, pl)
	}
}

func (p *Pool) stopLocked(key string, pl *poller) {
	if pl.grace != nil {
		pl.grace.Stop()
	}
	pl.cancel()
	delete(p.pollers, key)
}

// Wait blocks until req.Cond is met or can no longer be met, req.Timeout
// expires (StatusPending) or ctx ends (ctx.Err()). An error reading the
// resource is returned if no snapshot has been taken yet for this wait;
// later errors are retried on the next cycle.
func (p *Pool) Wait(ctx context.Context, req Request) (Result, error) {
	start := time.Now()
	w := &waiter{needs: Needs(req.Ref.Kind, req.Cond), base: p.cfg.OtherBase, updates: make(chan update, 1)}
	if req.Cond == ChecksDone || req.Cond == ChecksGreen || req.Cond == Done {
		w.base = p.cfg.ChecksBase
	}
	pl := p.subscribe(req, w)

	res := Result{Condition: req.Cond, Changes: []Change{}}
	finish := func(status string) (Result, error) {
		p.mu.Lock()
		res.Polls = w.polls
		p.mu.Unlock()
		res.Status, res.Duration = status, time.Since(start)
		return res, nil
	}
	defer p.leave(pl, w)
	timeout := time.NewTimer(req.Timeout)
	defer timeout.Stop()
	var first *Snapshot
	for {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-timeout.C:
			return finish(StatusPending)
		case u := <-w.updates:
			if u.err != nil {
				if first == nil {
					return Result{}, u.err
				}
				continue
			}
			if first == nil {
				first = &u.snap
			}
			var met bool
			met, res.Changes = Evaluate(req.Cond, *first, u.snap)
			res.State = u.snap
			switch {
			case met:
				return finish(StatusMet)
			case Failed(req.Cond, u.snap):
				return finish(StatusFailed)
			}
		}
	}
}

// subscribe adds w to the poller of req, creating it. A poller that already
// holds a snapshot with every resource w needs serves it without a call;
// otherwise it polls now.
func (p *Pool) subscribe(req Request, w *waiter) *poller {
	key := req.Scope + "|" + req.Ref.Key()
	p.mu.Lock()
	defer p.mu.Unlock()
	pl, ok := p.pollers[key]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		pl = &poller{
			key: key, scope: req.Scope, ref: req.Ref, fetch: req.Fetch, cancel: cancel,
			wake: make(chan struct{}, 1), waiters: map[*waiter]struct{}{},
		}
		p.pollers[key] = pl
		go p.run(ctx, pl)
	}
	if pl.grace != nil {
		pl.grace.Stop()
		pl.grace = nil
	}
	pl.waiters[w] = struct{}{}
	switch {
	case pl.snap != nil && covers(pl.have, w.needs):
		w.updates <- update{snap: *pl.snap}
	case pl.flight != nil && covers(pl.flight, w.needs):
		// The poll in flight will hand it its snapshot.
	default:
		select {
		case pl.wake <- struct{}{}:
		default:
		}
	}
	return pl
}

// leave removes w and, when it was the last waiter, stops the poller after
// the grace period unless another waiter arrives first.
func (p *Pool) leave(pl *poller, w *waiter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(pl.waiters, w)
	if len(pl.waiters) > 0 || p.pollers[pl.key] != pl {
		return
	}
	if p.cfg.Grace <= 0 {
		p.stopLocked(pl.key, pl)
		return
	}
	var t *time.Timer
	t = time.AfterFunc(p.cfg.Grace, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if pl.grace == t && len(pl.waiters) == 0 && p.pollers[pl.key] == pl {
			p.stopLocked(pl.key, pl)
		}
	})
	pl.grace = t
}

func (p *Pool) run(ctx context.Context, pl *poller) {
	tick := time.NewTimer(time.Hour)
	tick.Stop()
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pl.wake:
		case <-tick.C:
		}
		p.poll(ctx, pl)
		if ctx.Err() != nil {
			return
		}
		tick.Reset(p.interval(pl))
	}
}

// poll reads the resources the waiters need and hands the snapshot to every
// waiter it covers.
func (p *Pool) poll(ctx context.Context, pl *poller) {
	p.mu.Lock()
	if len(pl.waiters) > 0 {
		pl.needs, pl.base = nil, 0
		for w := range pl.waiters {
			for _, n := range w.needs {
				if !slices.Contains(pl.needs, n) {
					pl.needs = append(pl.needs, n)
				}
			}
			if pl.base == 0 || w.base < pl.base {
				pl.base = w.base
			}
		}
	}
	needs := slices.Clone(pl.needs)
	pl.flight = needs
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		pl.flight = nil
		p.mu.Unlock()
	}()

	parts := map[gh.Resource]json.RawMessage{}
	var err error
	for _, what := range needs {
		var data json.RawMessage
		if data, err = pl.fetch(ctx, pl.ref, what); err != nil {
			break
		}
		parts[what] = data
	}
	var snap Snapshot
	if err == nil {
		snap, err = BuildSnapshot(parts)
	}
	if ctx.Err() != nil {
		return
	}
	if p.cfg.OnPoll != nil {
		p.cfg.OnPoll()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	pl.flight = nil
	if err == nil {
		pl.snap, pl.have = &snap, needs
	}
	for w := range pl.waiters {
		w.polls++
		if err == nil && !covers(needs, w.needs) {
			continue // joined mid-cycle; it has asked for another one
		}
		select {
		case <-w.updates:
		default:
		}
		w.updates <- update{snap: snap, err: err}
	}
}

func (p *Pool) interval(pl *poller) time.Duration {
	p.mu.Lock()
	base := pl.base
	p.mu.Unlock()
	if base <= 0 {
		base = p.cfg.OtherBase
	}
	iv := base
	if p.cfg.Gov != nil {
		iv = p.cfg.Gov.PollInterval(pl.scope, "core", base)
	}
	return max(iv, p.cfg.MinInterval)
}

func covers(have, needs []gh.Resource) bool {
	for _, n := range needs {
		if !slices.Contains(have, n) {
			return false
		}
	}
	return true
}
