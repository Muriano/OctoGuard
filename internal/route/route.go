// Package route chooses, for each read, whether to spend REST or GraphQL quota.
package route

import (
	"time"

	"github.com/Muriano/OctoGuard/internal/quota"
)

// Resource is a kind of read the GitHub client can serve.
type Resource string

// Readable resources.
const (
	PR             Resource = "pr" // composite: pull + reviews + checks
	Issue          Resource = "issue"
	Reviews        Resource = "reviews"
	Comments       Resource = "comments"
	Checks         Resource = "checks"
	Runs           Resource = "runs"
	RequiredChecks Resource = "required_checks"
)

// Via is the API used to serve a read.
type Via string

// Available vias.
const (
	REST    Via = "rest"
	GraphQL Via = "graphql"
)

// lowBelow is the fraction of a bucket under which it is considered low.
const lowBelow = 0.20

// graphQLCapable lists the resources the client can read through GraphQL.
// Checks, Runs and RequiredChecks are REST only.
var graphQLCapable = map[Resource]bool{PR: true, Issue: true, Reviews: true, Comments: true}

// Choose picks the via for a read of what. cached tells whether the REST
// entry of what is already in the cache, so a conditional request (a 304
// costs no quota) can revalidate it. snap is the quota snapshot keyed by
// resource ("core", "graphql").
//
// REST is chosen when the resource is cached or GraphQL has less than 20%
// left; GraphQL when the read is composite (PR) and not cached, or when REST
// has less than 20% left. Resources GraphQL cannot serve always go to REST.
// The gh client does not ask when it can serve a GraphQL entry that is still
// within the read's MaxAge straight from the cache.
func Choose(what Resource, cached bool, snap map[string]quota.Bucket) Via {
	if !graphQLCapable[what] {
		return REST
	}
	now := time.Now()
	switch {
	case low(snap["graphql"], now):
		return REST
	case cached:
		return REST
	case low(snap["core"], now):
		return GraphQL
	case what == PR:
		return GraphQL
	default:
		return REST
	}
}

// low reports whether b is known, still before its reset, and under lowBelow.
func low(b quota.Bucket, now time.Time) bool {
	if !b.Known || b.Limit <= 0 || !b.Reset.After(now) {
		return false
	}
	return float64(b.Remaining)/float64(b.Limit) < lowBelow
}
