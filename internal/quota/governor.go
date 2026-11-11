// Package quota tracks GitHub API quota from the X-RateLimit-* headers of
// responses already received (never calling /rate_limit) and decides how fast
// callers may spend it, so that OctoGuard slows down before hitting the limit.
package quota

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Budget thresholds, as the fraction of the limit still remaining.
const (
	relaxedAbove  = 0.50 // above: poll at base
	slowAbove     = 0.20 // above (up to relaxedAbove): base×2
	criticalBelow = 0.05 // from here up to slowAbove: base×4; below: critical
)

// Bucket is the last known state of one quota resource (core, graphql, ...).
type Bucket struct {
	Remaining, Limit int
	Reset            time.Time
	Known            bool
}

type state struct {
	bucket       Bucket
	blockedUntil time.Time // from Retry-After; zero if never blocked
	lastGrant    time.Time // last Allow granted in the critical zone
}

// Governor holds the quota state per token ID and resource. It is safe for
// concurrent use.
type Governor struct {
	now func() time.Time

	mu     sync.Mutex
	tokens map[string]map[string]*state
}

// New returns a Governor that reads the current time from now.
func New(now func() time.Time) *Governor {
	return &Governor{now: now, tokens: make(map[string]map[string]*state)}
}

// state returns the state for (tokenID, resource), creating it. Callers must
// hold g.mu.
func (g *Governor) state(tokenID, resource string) *state {
	res, ok := g.tokens[tokenID]
	if !ok {
		res = make(map[string]*state)
		g.tokens[tokenID] = res
	}
	st, ok := res[resource]
	if !ok {
		st = &state{}
		res[resource] = st
	}
	return st
}

// lookup returns the state for (tokenID, resource) or nil. Callers must hold
// g.mu.
func (g *Governor) lookup(tokenID, resource string) *state {
	return g.tokens[tokenID][resource]
}

// Observe records the quota reported by the X-RateLimit-* headers of a
// response. The resource comes from X-RateLimit-Resource (core, graphql,
// search, ...) and defaults to core. Missing or malformed headers are ignored.
func (g *Governor) Observe(tokenID string, h http.Header) {
	limit, err := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	if err != nil {
		return
	}
	remaining, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		used, uerr := strconv.Atoi(h.Get("X-RateLimit-Used"))
		if uerr != nil {
			return
		}
		remaining = limit - used
	}
	reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return
	}
	resource := h.Get("X-RateLimit-Resource")
	if resource == "" {
		resource = "core"
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(tokenID, resource)
	st.bucket = Bucket{Remaining: remaining, Limit: limit, Reset: time.Unix(reset, 0), Known: true}
}

// ObserveRetryAfter records that GitHub asked to wait d before using resource
// again (a 403/429 with Retry-After). The block only ever grows; later healthy
// headers do not lift it, since secondary limits arrive with plenty of quota.
func (g *Governor) ObserveRetryAfter(tokenID, resource string, d time.Duration) {
	if d <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(tokenID, resource)
	if until := g.now().Add(d); until.After(st.blockedUntil) {
		st.blockedUntil = until
	}
}

// Snapshot returns a copy of the known buckets of tokenID, keyed by resource.
// It only reads local state and never touches the network.
func (g *Governor) Snapshot(tokenID string) map[string]Bucket {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]Bucket)
	for res, st := range g.tokens[tokenID] {
		if st.bucket.Known {
			out[res] = st.bucket
		}
	}
	return out
}

// Allow reports how long to wait before spending cost units of resource; 0
// means now. Below 5% of the limit only revalidations of cached ETags may run:
// callers express them with cost 0 (a 304 does not consume quota), and they are
// paced at remaining/seconds-until-reset. Any cost > 0 waits for the reset.
func (g *Governor) Allow(tokenID, resource string, cost int) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	st := g.lookup(tokenID, resource)
	if st == nil {
		return 0
	}
	if wait := st.blockedUntil.Sub(now); wait > 0 {
		return wait
	}
	b := st.bucket
	if healthy(b, now) {
		return 0
	}
	untilReset := b.Reset.Sub(now)
	if b.Remaining < cost {
		return untilReset
	}
	if fraction(b) >= criticalBelow {
		return 0
	}
	// Critical zone: only paced cost-0 revalidations.
	if cost > 0 || b.Remaining <= 0 {
		return untilReset
	}
	spacing := untilReset / time.Duration(b.Remaining)
	if !st.lastGrant.IsZero() {
		if wait := st.lastGrant.Add(spacing).Sub(now); wait > 0 {
			return wait
		}
	}
	st.lastGrant = now
	return 0
}

// PollInterval scales base by the remaining budget of resource: base above 50%,
// base×2 down to 20%, base×4 down to 5%, and below that the pace that lasts
// until the reset (never less than base×4). An active Retry-After always wins.
func (g *Governor) PollInterval(tokenID, resource string, base time.Duration) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	st := g.lookup(tokenID, resource)
	if st == nil {
		return base
	}
	interval := base
	if b := st.bucket; !healthy(b, now) {
		frac := fraction(b)
		untilReset := b.Reset.Sub(now)
		switch {
		case frac > relaxedAbove:
			interval = base
		case frac > slowAbove:
			interval = base * 2
		case frac >= criticalBelow:
			interval = base * 4
		case b.Remaining <= 0:
			interval = max(base*4, untilReset)
		default:
			interval = max(base*4, untilReset/time.Duration(b.Remaining))
		}
	}
	if blocked := st.blockedUntil.Sub(now); blocked > interval {
		interval = blocked
	}
	return interval
}

// healthy reports whether b carries no usable constraint: unknown, without a
// limit, or already past its reset (the quota has been refilled).
func healthy(b Bucket, now time.Time) bool {
	return !b.Known || b.Limit <= 0 || !b.Reset.After(now)
}

// fraction returns the share of b's limit still remaining. b.Limit must be > 0.
func fraction(b Bucket) float64 {
	return float64(b.Remaining) / float64(b.Limit)
}
