// Package stats turns the hourly usage counters and the quota snapshot into a
// report of quota use and savings, and renders it for a human.
package stats

import (
	"os"
	"time"

	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
)

// Counters read by stats that no package on main emits yet. They are named
// here so that their producers emit exactly these names.
const (
	// CounterWaitSaved is emitted by `wait` (T09): the polls a fixed-interval
	// loop at the baseline would have made, minus the polls actually made.
	CounterWaitSaved = "wait_saved"
	// CounterThrottle counts the times the governor slowed callers down
	// before the quota ran out (a rate limit avoided).
	CounterThrottle = "throttle"
)

// DefaultBaseline is the polling interval of the reference `sleep N && gh`
// loop that `wait` is compared against.
const DefaultBaseline = 30 * time.Second

// BaselineEnv overrides DefaultBaseline (a Go duration, e.g. "45s").
const BaselineEnv = "OCTOGUARD_POLL_BASELINE"

// now is a test hook.
var now = time.Now

// Report is what `octoguard stats` shows.
type Report struct {
	// At is when the report was computed.
	At time.Time `json:"at"`
	// Since is the length of the window covered by Saved, Hourly and RateLimits.
	Since time.Duration `json:"since"`
	// Quota is the last known state of each quota resource (core, graphql,
	// search...). Empty when unknown, e.g. the daemon is not running.
	Quota map[string]quota.Bucket `json:"quota"`
	// Saved breaks down the calls avoided in the window.
	Saved Saved `json:"saved"`
	// Hourly has one point per hour of the window, oldest first.
	Hourly []HourPoint `json:"hourly"`
	// RateLimits counts rate limits avoided and suffered in the window.
	RateLimits RateLimits `json:"rate_limits"`
	// Baseline is the polling interval `wait` is compared against.
	Baseline time.Duration `json:"baseline"`
}

// Saved is the number of GitHub requests avoided, by mechanism.
type Saved struct {
	Cache      int64 `json:"cache"`        // answered from the cache, no request
	ETag304    int64 `json:"etag_304"`     // revalidated with a 304, no quota spent
	Dedup      int64 `json:"dedup"`        // joined an identical read in flight
	WaitVsPoll int64 `json:"wait_vs_poll"` // polls a baseline loop would have made, minus real ones
	// TotalCalls is the requests that would have been made without octoguard:
	// the real ones that spent REST quota plus every avoided one.
	TotalCalls int64 `json:"total_calls"`
	// RESTSaved is every avoided request, counted as one REST call each.
	RESTSaved int64 `json:"rest_saved"`
	// GraphQLPointsSaved is the GraphQL points avoided. The counters do not
	// record which API an avoided read would have used, so it is 0 for now.
	GraphQLPointsSaved int64 `json:"graphql_points_saved"`
}

// HourPoint is one hour of the window.
type HourPoint struct {
	Hour    time.Time `json:"hour"`
	Real    int64     `json:"real"`    // REST requests that spent quota
	Avoided int64     `json:"avoided"` // requests avoided by any mechanism
}

// RateLimits counts rate limits in the window.
type RateLimits struct {
	Avoided  int64 `json:"avoided"`  // the governor slowed down before the limit
	Suffered int64 `json:"suffered"` // GitHub refused a request (403/429)
}

// Compute builds the report of the window that ends now and lasts since,
// from the hourly counter buckets and the quota snapshot.
func Compute(buckets []cache.Bucket, snap map[string]quota.Bucket, since time.Duration) Report {
	at := now().UTC()
	hours := int((since + time.Hour - 1) / time.Hour)
	if hours < 1 {
		hours = 1
	}
	first := at.Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour)

	r := Report{
		At:       at,
		Since:    since,
		Quota:    snap,
		Hourly:   make([]HourPoint, hours),
		Baseline: Baseline(),
	}
	if r.Quota == nil {
		r.Quota = map[string]quota.Bucket{}
	}
	for i := range r.Hourly {
		r.Hourly[i].Hour = first.Add(time.Duration(i) * time.Hour)
	}

	for _, b := range buckets {
		i := int(b.Hour.UTC().Sub(first) / time.Hour)
		if b.Hour.Before(first) || i >= hours {
			continue
		}
		p := &r.Hourly[i]
		switch b.Name {
		case gh.CounterCacheHit:
			r.Saved.Cache += b.N
			p.Avoided += b.N
		case gh.CounterETag304:
			r.Saved.ETag304 += b.N
			p.Avoided += b.N
		case daemon.CounterDedup:
			r.Saved.Dedup += b.N
			p.Avoided += b.N
		case CounterWaitSaved:
			r.Saved.WaitVsPoll += b.N
			p.Avoided += b.N
		case gh.CounterRESTCall:
			p.Real += b.N
		case gh.CounterRateLimitHit:
			r.RateLimits.Suffered += b.N
		case CounterThrottle:
			r.RateLimits.Avoided += b.N
		}
	}

	r.Saved.RESTSaved = r.Saved.Cache + r.Saved.ETag304 + r.Saved.Dedup + r.Saved.WaitVsPoll
	var realCalls int64
	for _, p := range r.Hourly {
		realCalls += p.Real
	}
	r.Saved.TotalCalls = realCalls + r.Saved.RESTSaved
	return r
}

// Baseline returns the reference polling interval, from BaselineEnv when it
// holds a positive duration and DefaultBaseline otherwise.
func Baseline() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(BaselineEnv)); err == nil && d > 0 {
		return d
	}
	return DefaultBaseline
}
