// Package gh is OctoGuard's GitHub client: REST reads revalidated with ETags
// against the shared cache, GraphQL reads when the router picks them, and
// quota headers fed to the governor on every response.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/route"
)

// Resource is a kind of read; see the route package for the values.
type Resource = route.Resource

// Readable resources.
const (
	PR             = route.PR // composite: {"pull", "reviews", "checks"}
	Issue          = route.Issue
	Reviews        = route.Reviews
	Comments       = route.Comments
	Checks         = route.Checks
	Runs           = route.Runs
	RequiredChecks = route.RequiredChecks
)

// Counter names.
const (
	CounterCacheHit      = "cache_hit"      // a request answered from the cache, no network
	CounterETag304       = "etag_304"       // a conditional REST request answered 304
	CounterRESTCall      = "rest_call"      // a REST request not answered 304
	CounterGraphQLPoints = "graphql_points" // GraphQL points spent
	CounterRateLimitHit  = "ratelimit_hit"  // a response refused for rate limiting
)

// CounterSink receives usage counters. *cache.Cache implements it.
type CounterSink interface {
	IncCounter(tokenID, name string, n int64, at time.Time) error
}

// GetOpts tunes a read.
type GetOpts struct {
	// Fresh ignores MaxAge: every part is validated against GitHub (a
	// conditional request answered 304 still counts as validated).
	Fresh bool
	// MaxAge serves cached parts younger than this without any request.
	MaxAge time.Duration
}

// Result is the outcome of a read.
type Result struct {
	Data        json.RawMessage
	FromCache   bool      // served entirely from the cache, no request made
	NotModified bool      // requests were made and every one answered 304
	Via         string    // "rest" or "graphql"
	FetchedAt   time.Time // when the oldest part of Data was fetched or validated
}

// ErrUnauthorized is wrapped by the error of a 401 response.
var ErrUnauthorized = errors.New("GitHub rejected the token (401): check GH_TOKEN/GITHUB_TOKEN or run `gh auth login`")

// APIError is a non-success answer from GitHub.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return fmt.Sprintf("github: %v: %s", ErrUnauthorized, e.Message)
	}
	return fmt.Sprintf("github: %d %s", e.Status, e.Message)
}

// Unwrap lets errors.Is(err, ErrUnauthorized) match a 401.
func (e *APIError) Unwrap() error {
	if e.Status == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	return nil
}

// RateLimitError means GitHub refused the request, or the governor held it,
// for rate limiting; retry after Wait.
type RateLimitError struct {
	Resource string // quota resource: "core", "graphql", ...
	Wait     time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github: rate limited on %s, retry in %s", e.Resource, e.Wait.Round(time.Second))
}

// maxBody bounds the size of a response body read into memory.
const maxBody = 32 << 20

// Client reads GitHub with one token. It is safe for concurrent use.
type Client struct {
	baseURL  string
	tok      auth.Token
	cache    *cache.Cache
	gov      *quota.Governor
	counters CounterSink
	http     *http.Client
	now      func() time.Time
}

// NewClient returns a client for the API at baseURL (e.g.
// "https://api.github.com"; GraphQL is served at baseURL+"/graphql").
// counters may be nil.
func NewClient(baseURL string, tok auth.Token, c *cache.Cache, g *quota.Governor, counters CounterSink) *Client {
	return &Client{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		tok:      tok,
		cache:    c,
		gov:      g,
		counters: counters,
		http:     &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
	}
}

// Scope is the ID under which a token's cache entries, quota and counters are
// kept for host: the host plus the token hash, never the token itself.
func Scope(host string, tok auth.Token) string { return host + "/" + tok.ID }

// Get reads what for r. The REST parts are served from the cache when
// younger than opt.MaxAge, and otherwise revalidated with If-None-Match.
func (c *Client) Get(ctx context.Context, r ref.Ref, what Resource, opt GetOpts) (Result, error) {
	if err := checkKind(r, what); err != nil {
		return Result{}, err
	}
	rd := &read{c: c, ctx: ctx, scope: Scope(r.Host, c.tok), opt: opt}
	via := route.REST
	if key := primaryKey(r, what); key != "" {
		// A GraphQL entry also counts as cached: the next read moves to
		// REST, whose ETags make later revalidations free.
		cached := false
		for _, k := range []string{key, graphQLKey(r, what)} {
			_, ok, err := c.cache.Get(rd.scope, k)
			if err != nil {
				return Result{}, err
			}
			cached = cached || ok
		}
		via = route.Choose(what, cached, c.gov.Snapshot(rd.scope))
	}
	var data []byte
	var err error
	if via == route.GraphQL {
		data, err = rd.graphQLResource(r, what)
	} else {
		data, err = rd.restResource(r, what)
	}
	if err != nil {
		return Result{}, err
	}
	return Result{
		Data:        data,
		FromCache:   rd.requests == 0,
		NotModified: rd.requests > 0 && rd.notModified == rd.requests,
		Via:         string(via),
		FetchedAt:   rd.oldest,
	}, nil
}

// read carries the state of one Get across the parts it fetches.
type read struct {
	c     *Client
	ctx   context.Context
	scope string
	opt   GetOpts

	requests, notModified int
	oldest                time.Time
}

func (rd *read) note(fetchedAt time.Time, request, notModified bool) {
	if request {
		rd.requests++
		if notModified {
			rd.notModified++
		}
	}
	if rd.oldest.IsZero() || fetchedAt.Before(rd.oldest) {
		rd.oldest = fetchedAt
	}
}

func (rd *read) count(name string, n int64) {
	if rd.c.counters == nil || n == 0 {
		return
	}
	// Counters are best effort: losing one must not fail the read.
	_ = rd.c.counters.IncCounter(rd.scope, name, n, rd.c.now())
}

// fresh reports whether e may be served without a request.
func (rd *read) fresh(e cache.Entry) bool {
	return !rd.opt.Fresh && rd.opt.MaxAge > 0 && rd.c.now().Sub(e.FetchedAt) <= rd.opt.MaxAge
}

// serveCached returns a cached entry as a part of the read.
func (rd *read) serveCached(e cache.Entry) []byte {
	rd.count(CounterCacheHit, 1)
	rd.note(e.FetchedAt, false, false)
	return e.Body
}

// rest GETs path, caching the body under key and revalidating it with its
// ETag (or Last-Modified) when cached.
func (rd *read) rest(key, path string) ([]byte, error) {
	c := rd.c
	e, ok, err := c.cache.Get(rd.scope, key)
	if err != nil {
		return nil, err
	}
	if ok && rd.fresh(e) {
		return rd.serveCached(e), nil
	}
	conditional := ok && (e.ETag != "" || e.LastModified != "")
	cost := 1
	if conditional {
		cost = 0 // a 304 does not consume quota
	}
	if wait := c.gov.Allow(rd.scope, "core", cost); wait > 0 {
		if ok && !rd.opt.Fresh {
			return rd.serveCached(e), nil
		}
		return nil, &RateLimitError{Resource: "core", Wait: wait}
	}

	req, err := http.NewRequestWithContext(rd.ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	c.setHeaders(req)
	if conditional {
		if e.ETag != "" {
			req.Header.Set("If-None-Match", e.ETag)
		} else {
			req.Header.Set("If-Modified-Since", e.LastModified)
		}
	}
	resp, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	c.gov.Observe(rd.scope, resp.Header)
	now := c.now()

	if resp.StatusCode == http.StatusNotModified && ok {
		rd.count(CounterETag304, 1)
		e.FetchedAt = now
		if err := c.cache.Put(rd.scope, key, e); err != nil {
			return nil, err
		}
		rd.note(now, true, true)
		return e.Body, nil
	}
	rd.count(CounterRESTCall, 1)
	if resp.StatusCode != http.StatusOK {
		return nil, rd.failure(resp, body, "core")
	}
	entry := cache.Entry{
		Body:         body,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt:    now,
		Via:          string(route.REST),
	}
	if err := c.cache.Put(rd.scope, key, entry); err != nil {
		return nil, err
	}
	rd.note(now, true, false)
	return body, nil
}

// graphQL runs query, normalizes its data with normalize and caches the
// result under key. GraphQL has no ETags: a cached result is only reused
// within MaxAge, or while the governor holds GraphQL requests.
func (rd *read) graphQL(key, query string, vars map[string]any, normalize func(data []byte) ([]byte, error)) ([]byte, error) {
	c := rd.c
	e, ok, err := c.cache.Get(rd.scope, key)
	if err != nil {
		return nil, err
	}
	if ok && rd.fresh(e) {
		return rd.serveCached(e), nil
	}
	if wait := c.gov.Allow(rd.scope, "graphql", 1); wait > 0 {
		if ok && !rd.opt.Fresh {
			return rd.serveCached(e), nil
		}
		return nil, &RateLimitError{Resource: "graphql", Wait: wait}
	}

	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	req, err := http.NewRequestWithContext(rd.ctx, http.MethodPost, c.baseURL+"/graphql", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	c.gov.Observe(rd.scope, resp.Header)
	if resp.StatusCode != http.StatusOK {
		rd.count(CounterGraphQLPoints, 1)
		return nil, rd.failure(resp, body, "graphql")
	}

	var out struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("github: decode graphql response: %w", err)
	}
	var cost struct {
		RateLimit *struct {
			Cost int64 `json:"cost"`
		} `json:"rateLimit"`
	}
	points := int64(1)
	if json.Unmarshal(out.Data, &cost) == nil && cost.RateLimit != nil {
		points = cost.RateLimit.Cost
	}
	rd.count(CounterGraphQLPoints, points)
	if len(out.Errors) > 0 {
		first := out.Errors[0]
		switch {
		case first.Type == "RATE_LIMIT" || first.Type == "RATE_LIMITED" || first.Code == "graphql_rate_limit":
			rd.count(CounterRateLimitHit, 1)
			return nil, &RateLimitError{Resource: "graphql", Wait: rd.untilReset("graphql")}
		case first.Type == "NOT_FOUND":
			return nil, &APIError{Status: http.StatusNotFound, Message: first.Message}
		}
		return nil, &APIError{Status: http.StatusOK, Message: "graphql: " + first.Message}
	}
	data, err := normalize(out.Data)
	if err != nil {
		return nil, err
	}
	now := c.now()
	if err := c.cache.Put(rd.scope, key, cache.Entry{Body: data, FetchedAt: now, Via: string(route.GraphQL)}); err != nil {
		return nil, err
	}
	rd.note(now, true, false)
	return data, nil
}

// failure turns a non-success response into an error, feeding rate-limit
// refusals (403/429 with Retry-After or exhausted quota) to the governor.
// Nothing is written to the cache.
func (rd *read) failure(resp *http.Response, body []byte, resource string) error {
	if res := resp.Header.Get("X-RateLimit-Resource"); res != "" {
		resource = res
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			wait := time.Duration(secs) * time.Second
			rd.c.gov.ObserveRetryAfter(rd.scope, resource, wait)
			rd.count(CounterRateLimitHit, 1)
			return &RateLimitError{Resource: resource, Wait: wait}
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			rd.count(CounterRateLimitHit, 1)
			return &RateLimitError{Resource: resource, Wait: rd.untilReset(resource)}
		}
	}
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &msg)
	if msg.Message == "" {
		msg.Message = http.StatusText(resp.StatusCode)
	}
	return &APIError{Status: resp.StatusCode, Message: msg.Message}
}

// untilReset returns how long until resource's quota resets, as last seen.
func (rd *read) untilReset(resource string) time.Duration {
	b, ok := rd.c.gov.Snapshot(rd.scope)[resource]
	if !ok {
		return 0
	}
	return max(b.Reset.Sub(rd.c.now()), 0)
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.tok.Value)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "octoguard")
}

// do sends req and reads the body, bounded by maxBody.
func (c *Client) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("github: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, nil, fmt.Errorf("github: read %s: %w", req.URL.Path, err)
	}
	if len(body) > maxBody {
		return nil, nil, fmt.Errorf("github: response from %s exceeds %d bytes", req.URL.Path, maxBody)
	}
	return resp, body, nil
}
