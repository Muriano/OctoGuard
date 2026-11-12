package route

import (
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/quota"
)

func bucket(remaining, limit int) quota.Bucket {
	return quota.Bucket{Remaining: remaining, Limit: limit, Reset: time.Now().Add(time.Hour), Known: true}
}

func TestRoute_PrefersGraphQLWhenRestLow(t *testing.T) {
	restLow := map[string]quota.Bucket{"core": bucket(500, 5000), "graphql": bucket(5000, 5000)}
	for _, what := range []Resource{Issue, Reviews, Comments, PR} {
		if got := Choose(what, false, restLow); got != GraphQL {
			t.Errorf("Choose(%s, uncached, REST low) = %s, want graphql", what, got)
		}
	}
	// A cached resource still revalidates through REST: a 304 costs no quota.
	if got := Choose(Issue, true, restLow); got != REST {
		t.Errorf("Choose(issue, cached, REST low) = %s, want rest", got)
	}
	// GraphQL cannot serve these.
	for _, what := range []Resource{Checks, Runs, RequiredChecks} {
		if got := Choose(what, false, restLow); got != REST {
			t.Errorf("Choose(%s, REST low) = %s, want rest", what, got)
		}
	}
}

func TestRoute_Policy(t *testing.T) {
	healthy := map[string]quota.Bucket{"core": bucket(5000, 5000), "graphql": bucket(5000, 5000)}
	gqlLow := map[string]quota.Bucket{"core": bucket(5000, 5000), "graphql": bucket(100, 5000)}
	expired := map[string]quota.Bucket{"core": {Remaining: 0, Limit: 5000, Reset: time.Now().Add(-time.Minute), Known: true}}
	tests := []struct {
		name   string
		what   Resource
		cached bool
		snap   map[string]quota.Bucket
		want   Via
	}{
		{"composite uncached", PR, false, healthy, GraphQL},
		{"composite cached revalidates", PR, true, healthy, REST},
		{"composite with GraphQL low", PR, false, gqlLow, REST},
		{"simple uncached", Issue, false, healthy, REST},
		{"no snapshot", PR, false, nil, GraphQL},
		{"bucket past reset is not low", Issue, false, expired, REST},
	}
	for _, tt := range tests {
		if got := Choose(tt.what, tt.cached, tt.snap); got != tt.want {
			t.Errorf("%s: Choose = %s, want %s", tt.name, got, tt.want)
		}
	}
}
