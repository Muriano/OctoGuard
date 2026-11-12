package gh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

var (
	tok      = auth.Token{Value: "ghp_secret", Source: "GH_TOKEN", ID: "0123456789abcdef"}
	scope    = Scope("github.com", tok)
	issueRef = ref.Ref{Host: "github.com", Owner: "o", Repo: "r", Number: 1, Kind: ref.Issue}
	prRef    = ref.Ref{Host: "github.com", Owner: "o", Repo: "r", Number: 2, Kind: ref.PR}
)

const issuePath = "/repos/o/r/issues/1"

type env struct {
	srv   *fakegh.Server
	cache *cache.Cache
	gov   *quota.Governor
	c     *Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := fakegh.New(t)
	ch, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	gov := quota.New(time.Now)
	return &env{srv: srv, cache: ch, gov: gov, c: NewClient(srv.URL, tok, ch, gov, ch)}
}

func (e *env) get(t *testing.T, r ref.Ref, what Resource, opt GetOpts) Result {
	t.Helper()
	res, err := e.c.Get(context.Background(), r, what, opt)
	if err != nil {
		t.Fatalf("Get(%s): %v", what, err)
	}
	return res
}

func (e *env) counter(t *testing.T, name string) int64 {
	t.Helper()
	buckets, err := e.cache.Counters(scope, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, b := range buckets {
		if b.Name == name {
			n += b.N
		}
	}
	return n
}

func TestGet_SecondCallIs304AndCountsSaving(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResource(issuePath, []byte(`{"number":1,"title":"bug"}`), `"v1"`)

	first := e.get(t, issueRef, Issue, GetOpts{})
	if first.FromCache || first.NotModified || first.Via != "rest" {
		t.Fatalf("first = %+v, want a fresh REST read", first)
	}
	second := e.get(t, issueRef, Issue, GetOpts{})
	if !second.NotModified || second.FromCache {
		t.Fatalf("second = %+v, want NotModified", second)
	}
	if string(second.Data) != string(first.Data) {
		t.Errorf("Data = %s, want %s", second.Data, first.Data)
	}
	if got := e.srv.Calls(issuePath); got != 2 {
		t.Errorf("Calls = %d, want 2", got)
	}
	if got := e.counter(t, CounterETag304); got != 1 {
		t.Errorf("etag_304 = %d, want 1", got)
	}
	if got := e.counter(t, CounterRESTCall); got != 1 {
		t.Errorf("rest_call = %d, want 1", got)
	}
}

func TestGet_MaxAgeServesFromCacheNoCall(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResource(issuePath, []byte(`{"number":1}`), `"v1"`)

	e.get(t, issueRef, Issue, GetOpts{})
	res := e.get(t, issueRef, Issue, GetOpts{MaxAge: time.Hour})
	if !res.FromCache {
		t.Fatalf("res = %+v, want FromCache", res)
	}
	if got := e.srv.Calls(issuePath); got != 1 {
		t.Errorf("Calls = %d, want 1", got)
	}
	if got := e.counter(t, CounterCacheHit); got != 1 {
		t.Errorf("cache_hit = %d, want 1", got)
	}
}

func TestGet_FreshBypassesMaxAge(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResource(issuePath, []byte(`{"number":1}`), `"v1"`)

	e.get(t, issueRef, Issue, GetOpts{})
	res := e.get(t, issueRef, Issue, GetOpts{Fresh: true, MaxAge: time.Hour})
	if res.FromCache || !res.NotModified {
		t.Fatalf("res = %+v, want a revalidation answered 304", res)
	}
	if got := e.srv.Calls(issuePath); got != 2 {
		t.Errorf("Calls = %d, want 2", got)
	}
}

func TestGet_RoutesToGraphQLWhenRestLow(t *testing.T) {
	e := newEnv(t)
	e.srv.SetRate("core", 100, 5000, time.Now().Add(time.Hour))
	e.srv.SetResource("/repos/o/r/issues/1/comments", []byte(`[]`), `"c1"`)
	e.srv.SetResource("/graphql", []byte(`{"data":{"rateLimit":{"cost":1},"repository":{"issue":{
		"number":1,"title":"bug","state":"OPEN","author":{"login":"ana"},"labels":{"nodes":[{"name":"p1"}]}}}}}`), "")

	e.get(t, issueRef, Comments, GetOpts{}) // a REST response reports the low core bucket
	res := e.get(t, issueRef, Issue, GetOpts{})
	if res.Via != "graphql" || e.srv.GraphQLCalls() != 1 {
		t.Fatalf("Via = %s, GraphQLCalls = %d, want graphql and 1", res.Via, e.srv.GraphQLCalls())
	}
	var is struct {
		State  string                  `json:"state"`
		User   struct{ Login string }  `json:"user"`
		Labels []struct{ Name string } `json:"labels"`
	}
	if err := json.Unmarshal(res.Data, &is); err != nil {
		t.Fatal(err)
	}
	if is.State != "open" || is.User.Login != "ana" || len(is.Labels) != 1 || is.Labels[0].Name != "p1" {
		t.Errorf("normalized issue = %+v", is)
	}
	if got := e.counter(t, CounterGraphQLPoints); got != 1 {
		t.Errorf("graphql_points = %d, want 1", got)
	}
}

func setPR(srv *fakegh.Server) {
	srv.SetResource("/repos/o/r/pulls/2", []byte(`{"number":2,"head":{"sha":"abc"},"base":{"ref":"main"}}`), `"p1"`)
	srv.SetResource("/repos/o/r/pulls/2/reviews", []byte(`[{"id":7,"state":"APPROVED"}]`), `"rv1"`)
	srv.SetResource("/repos/o/r/commits/abc/check-runs", []byte(`{"total_count":2,"check_runs":[
		{"name":"build","status":"completed","conclusion":"success"},
		{"name":"test","status":"in_progress","conclusion":null}]}`), `"cr1"`)
	srv.SetResource("/repos/o/r/commits/abc/status", []byte(`{"state":"pending","statuses":[
		{"context":"ci/lint","state":"failure"},
		{"context":"ci/deploy","state":"pending"},
		{"context":"ci/legacy","state":"error"}]}`), `"st1"`)
}

func TestChecks_MergesRunsAndStatuses(t *testing.T) {
	e := newEnv(t)
	setPR(e.srv)

	res := e.get(t, prRef, Checks, GetOpts{})
	var got ChecksData
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	want := ChecksData{HeadSHA: "abc", Checks: []Check{
		{Name: "build", Kind: "check_run", Status: "completed", Conclusion: "success"},
		{Name: "test", Kind: "check_run", Status: "in_progress"},
		{Name: "ci/lint", Kind: "status", Status: "completed", Conclusion: "failure"},
		{Name: "ci/deploy", Kind: "status", Status: "pending"},
		{Name: "ci/legacy", Kind: "status", Status: "completed", Conclusion: "failure"},
	}}
	if g, w := mustJSON(t, got), mustJSON(t, want); g != w {
		t.Errorf("checks =\n%s\nwant\n%s", g, w)
	}

	// Revalidating costs three 304s and no quota.
	again := e.get(t, prRef, Checks, GetOpts{})
	if !again.NotModified || e.counter(t, CounterETag304) != 3 {
		t.Errorf("second read: NotModified = %v, etag_304 = %d, want true and 3", again.NotModified, e.counter(t, CounterETag304))
	}
}

func TestGet_PRCompositeViaGraphQL(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResource("/graphql", []byte(`{"data":{"rateLimit":{"cost":3},"repository":{"pullRequest":{
		"number":2,"state":"MERGED","merged":true,"mergeable":"UNKNOWN","headRefOid":"abc","baseRefName":"main",
		"reviews":{"nodes":[{"databaseId":7,"state":"APPROVED","author":{"login":"ana"}}]},
		"commits":{"nodes":[{"commit":{"oid":"abc","statusCheckRollup":{"contexts":{"nodes":[
			{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SKIPPED"},
			{"__typename":"StatusContext","context":"ci/lint","state":"SUCCESS"}]}}}}]}}}}}`), "")

	res := e.get(t, prRef, PR, GetOpts{})
	if res.Via != "graphql" {
		t.Fatalf("Via = %s, want graphql for an uncached composite read", res.Via)
	}
	var pr struct {
		Pull struct {
			State     string               `json:"state"`
			Merged    bool                 `json:"merged"`
			Mergeable *bool                `json:"mergeable"`
			Head      struct{ SHA string } `json:"head"`
		} `json:"pull"`
		Reviews []struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		} `json:"reviews"`
		Checks ChecksData `json:"checks"`
	}
	if err := json.Unmarshal(res.Data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Pull.State != "closed" || !pr.Pull.Merged || pr.Pull.Mergeable != nil || pr.Pull.Head.SHA != "abc" {
		t.Errorf("pull = %+v", pr.Pull)
	}
	if len(pr.Reviews) != 1 || pr.Reviews[0].ID != 7 {
		t.Errorf("reviews = %+v", pr.Reviews)
	}
	wantChecks := []Check{
		{Name: "build", Kind: "check_run", Status: "completed", Conclusion: "skipped"},
		{Name: "ci/lint", Kind: "status", Status: "completed", Conclusion: "success"},
	}
	if g, w := mustJSON(t, pr.Checks.Checks), mustJSON(t, wantChecks); g != w {
		t.Errorf("checks = %s, want %s", g, w)
	}
	if got := e.counter(t, CounterGraphQLPoints); got != 3 {
		t.Errorf("graphql_points = %d, want 3", got)
	}
}

func TestGet_PRCompositeViaREST(t *testing.T) {
	e := newEnv(t)
	setPR(e.srv)
	e.get(t, prRef, Checks, GetOpts{}) // caches the pull: the router now revalidates via REST

	res := e.get(t, prRef, PR, GetOpts{})
	if res.Via != "rest" || e.srv.GraphQLCalls() != 0 {
		t.Fatalf("Via = %s, GraphQLCalls = %d, want rest and 0", res.Via, e.srv.GraphQLCalls())
	}
	var pr map[string]json.RawMessage
	if err := json.Unmarshal(res.Data, &pr); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pull", "reviews", "checks"} {
		if len(pr[k]) == 0 {
			t.Errorf("composite PR misses %q: %s", k, res.Data)
		}
	}
}

func Test401NotCached(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResource(issuePath, []byte(`{"number":1}`), `"v1"`)
	key := issueRef.Key() + "/issue"

	e.srv.FailNext(issuePath, http.StatusUnauthorized, 0)
	_, err := e.c.Get(context.Background(), issueRef, Issue, GetOpts{})
	if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want ErrUnauthorized suggesting to check the token", err)
	}
	if strings.Contains(err.Error(), tok.Value) {
		t.Fatalf("error leaks the token: %v", err)
	}
	if _, ok, _ := e.cache.Get(scope, key); ok {
		t.Fatal("a 401 wrote a cache entry")
	}

	// A 401 while revalidating leaves the previous entry untouched.
	e.get(t, issueRef, Issue, GetOpts{})
	before, _, _ := e.cache.Get(scope, key)
	e.srv.FailNext(issuePath, http.StatusUnauthorized, 0)
	if _, err := e.c.Get(context.Background(), issueRef, Issue, GetOpts{Fresh: true}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	after, _, _ := e.cache.Get(scope, key)
	if mustJSON(t, after) != mustJSON(t, before) {
		t.Errorf("entry changed after a 401: %+v -> %+v", before, after)
	}
}

func TestGet_403RetryAfterFeedsGovernor(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResource(issuePath, []byte(`{"number":1}`), `"v1"`)
	e.srv.FailNext(issuePath, http.StatusForbidden, time.Minute)

	_, err := e.c.Get(context.Background(), issueRef, Issue, GetOpts{})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Resource != "core" || rl.Wait != time.Minute {
		t.Fatalf("err = %v, want a RateLimitError on core for 1m", err)
	}
	if wait := e.gov.Allow(scope, "core", 1); wait <= 0 {
		t.Errorf("governor Allow = %v, want a wait after Retry-After", wait)
	}
	if got := e.counter(t, CounterRateLimitHit); got != 1 {
		t.Errorf("ratelimit_hit = %d, want 1", got)
	}
	// While blocked, no request reaches GitHub.
	if _, err := e.c.Get(context.Background(), issueRef, Issue, GetOpts{}); !errors.As(err, &rl) {
		t.Errorf("err = %v, want RateLimitError while blocked", err)
	}
	if got := e.srv.Calls(issuePath); got != 1 {
		t.Errorf("Calls = %d, want 1", got)
	}
}

func TestGet_RejectsResourceForWrongKind(t *testing.T) {
	e := newEnv(t)
	if _, err := e.c.Get(context.Background(), issueRef, Checks, GetOpts{}); !errors.Is(err, ref.ErrUsage) {
		t.Errorf("err = %v, want ErrUsage", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRequiredChecks_MergesProtectionAndRulesets(t *testing.T) {
	e := newEnv(t)
	setPR(e.srv)
	e.srv.SetResource("/repos/o/r/branches/main", []byte(`{"name":"main","protection":{
		"required_status_checks":{"contexts":["build","ci/lint"]}}}`), `"b1"`)
	e.srv.SetResource("/repos/o/r/rules/branches/main", []byte(`[
		{"type":"pull_request"},
		{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build"},{"context":"e2e"}]}}]`), `"rl1"`)

	res := e.get(t, prRef, RequiredChecks, GetOpts{})
	want := `{"base":"main","contexts":["build","ci/lint","e2e"]}`
	if string(res.Data) != want {
		t.Errorf("Data = %s, want %s", res.Data, want)
	}
}
