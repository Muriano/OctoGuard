package stats

import (
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/quota"
)

// fixedNow pins the clock at 2026-11-12 10:20 UTC for the test.
func fixedNow(t *testing.T) time.Time {
	t.Helper()
	at := time.Date(2026, 11, 12, 10, 20, 0, 0, time.UTC)
	prev := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = prev })
	return at
}

func TestCompute_SavingsFormula(t *testing.T) {
	at := fixedNow(t)
	t.Setenv(BaselineEnv, "")
	h := func(ago int) time.Time { return at.Truncate(time.Hour).Add(-time.Duration(ago) * time.Hour) }
	buckets := []cache.Bucket{
		{Name: "cache_hit", Hour: h(2), N: 40},
		{Name: "etag_304", Hour: h(2), N: 10},
		{Name: "rest_call", Hour: h(2), N: 7},
		{Name: "dedup", Hour: h(1), N: 4},
		{Name: "wait_saved", Hour: h(1), N: 16},
		{Name: "rest_call", Hour: h(0), N: 3},
		{Name: "ratelimit_hit", Hour: h(0), N: 1},
		{Name: "throttle", Hour: h(0), N: 2},
		{Name: "graphql_points", Hour: h(0), N: 99}, // spent, not saved
		{Name: "cache_hit", Hour: h(3), N: 1000},    // outside a 3h window
	}
	snap := map[string]quota.Bucket{"core": {Remaining: 4000, Limit: 5000, Known: true}}

	r := Compute(buckets, snap, 3*time.Hour)

	want := Saved{Cache: 40, ETag304: 10, Dedup: 4, WaitVsPoll: 16, RESTSaved: 70, TotalCalls: 80}
	if r.Saved != want {
		t.Errorf("Saved = %+v, want %+v", r.Saved, want)
	}
	if r.RateLimits != (RateLimits{Avoided: 2, Suffered: 1}) {
		t.Errorf("RateLimits = %+v", r.RateLimits)
	}
	if r.Baseline != 30*time.Second {
		t.Errorf("Baseline = %v, want 30s", r.Baseline)
	}
	wantHourly := []HourPoint{
		{Hour: h(2), Real: 7, Avoided: 50},
		{Hour: h(1), Real: 0, Avoided: 20},
		{Hour: h(0), Real: 3, Avoided: 0},
	}
	if len(r.Hourly) != len(wantHourly) {
		t.Fatalf("len(Hourly) = %d, want %d", len(r.Hourly), len(wantHourly))
	}
	for i, p := range wantHourly {
		if r.Hourly[i] != p {
			t.Errorf("Hourly[%d] = %+v, want %+v", i, r.Hourly[i], p)
		}
	}
	if r.Quota["core"].Remaining != 4000 || !r.At.Equal(at) || r.Since != 3*time.Hour {
		t.Errorf("unexpected report header: %+v", r)
	}
}

func TestCompute_EmptyFillsWindow(t *testing.T) {
	fixedNow(t)
	r := Compute(nil, nil, 7*24*time.Hour)
	if len(r.Hourly) != 168 || r.Quota == nil || r.Saved != (Saved{}) {
		t.Fatalf("empty report: %d hours, quota=%v, saved=%+v", len(r.Hourly), r.Quota, r.Saved)
	}
}

func TestBaseline_Env(t *testing.T) {
	t.Setenv(BaselineEnv, "45s")
	if got := Baseline(); got != 45*time.Second {
		t.Errorf("Baseline = %v, want 45s", got)
	}
	t.Setenv(BaselineEnv, "nonsense")
	if got := Baseline(); got != DefaultBaseline {
		t.Errorf("Baseline = %v, want default", got)
	}
}
