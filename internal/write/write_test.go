package write

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

var (
	tok   = auth.FromValue("ghp_secret")
	scope = gh.Scope(ref.Host, tok)
	prRef = ref.Ref{Host: ref.Host, Owner: "o", Repo: "r", Number: 2, Kind: ref.PR}
)

const mergePath = "/repos/o/r/pulls/2/merge"

// fakeClock is a clock whose sleep advances it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}

type env struct {
	srv   *fakegh.Server
	cache *cache.Cache
	clock *fakeClock
	w     *Writer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := fakegh.New(t)
	ch, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	clock := &fakeClock{now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	return &env{srv: srv, cache: ch, clock: clock, w: &Writer{
		Client: gh.NewClient(srv.URL, tok, ch, quota.New(time.Now), ch),
		Cache:  ch,
		Spacer: NewSpacer(clock.Now, clock.Sleep),
		Scope:  scope,
	}}
}

// setPR serves PR 2 at head abc with the given check runs and no required
// checks.
func (e *env) setPR(checkRuns, etag string) {
	e.srv.SetResource("/repos/o/r/pulls/2", []byte(`{"number":2,"head":{"sha":"abc"},"base":{"ref":"main"}}`), `"p"`)
	e.srv.SetResource("/repos/o/r/commits/abc/check-runs", []byte(`{"check_runs":`+checkRuns+`}`), etag)
	e.srv.SetResource("/repos/o/r/commits/abc/status", []byte(`{"statuses":[]}`), `"s"`)
	e.srv.SetResource("/repos/o/r/branches/main", []byte(`{"protection":{}}`), `"b"`)
	e.srv.SetResource("/repos/o/r/rules/branches/main", []byte(`[]`), `"r"`)
}

const (
	greenRuns = `[{"name":"ci","status":"completed","conclusion":"success"}]`
	redRuns   = `[{"name":"ci","status":"completed","conclusion":"failure"}]`
)

func TestComment_InvalidatesComments(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResource("/repos/o/r/issues/2/comments", []byte(`[]`), `"c1"`)
	e.setPR(greenRuns, `"g"`)
	e.srv.SetResponse("POST", "/repos/o/r/issues/2/comments", 201, []byte(`{"id":9}`))
	ctx := context.Background()
	for _, what := range []gh.Resource{gh.Comments, gh.Checks} {
		if _, err := e.w.Client.Get(ctx, prRef, what, gh.GetOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	keys := []string{prRef.Key() + "/comments?page=1", prRef.Key() + "/pull"}
	for _, k := range keys {
		if _, ok, _ := e.cache.Get(scope, k); !ok {
			t.Fatalf("%s not cached before the write", k)
		}
	}

	res, err := e.w.Execute(ctx, Request{Op: PRComment, Ref: prRef, Body: "hi"})
	if err != nil || res.Status != StatusOK {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	if got := string(e.srv.Body("/repos/o/r/issues/2/comments")); got != `{"body":"hi"}` {
		t.Fatalf("request body = %s", got)
	}
	for _, k := range keys {
		if _, ok, _ := e.cache.Get(scope, k); ok {
			t.Errorf("%s still cached after the comment", k)
		}
	}
}

func TestMerge_RejectsWhenChecksRed(t *testing.T) {
	e := newEnv(t)
	e.setPR(redRuns, `"red"`)
	e.srv.SetResponse("PUT", mergePath, 200, []byte(`{"merged":true}`))

	res, err := e.w.Execute(context.Background(), Request{Op: PRMerge, Ref: prRef, Method: "squash"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRejected || !strings.Contains(res.Reason, "ci=failure") ||
		res.Hint != "octoguard wait pr o/r#2 --until checks-green" {
		t.Fatalf("unexpected result %+v", res)
	}
	if n := e.srv.Calls(mergePath); n != 0 {
		t.Fatalf("merge endpoint called %d times", n)
	}
}

func TestMerge_RejectsWhenNoChecksYet(t *testing.T) {
	e := newEnv(t)
	e.setPR(`[]`, `"none"`)
	e.srv.SetResponse("PUT", mergePath, 200, []byte(`{"merged":true}`))

	res, err := e.w.Execute(context.Background(), Request{Op: PRMerge, Ref: prRef, Method: "squash"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRejected || !strings.Contains(res.Reason, "no checks reported yet") {
		t.Fatalf("unexpected result %+v", res)
	}
	if n := e.srv.Calls(mergePath); n != 0 {
		t.Fatalf("merge endpoint called %d times", n)
	}
}

func TestMerge_NoCIMergesAfterGrace(t *testing.T) {
	e := newEnv(t)
	e.setPR(`[]`, `"none"`)
	e.srv.SetResource("/repos/o/r/pulls/2",
		[]byte(`{"number":2,"head":{"sha":"abc"},"base":{"ref":"main"},"updated_at":"2020-01-01T00:00:00Z"}`), `"old"`)
	e.srv.SetResponse("PUT", mergePath, 200, []byte(`{"merged":true}`))

	res, err := e.w.Execute(context.Background(), Request{Op: PRMerge, Ref: prRef, Method: "squash"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK {
		t.Fatalf("unexpected result %+v", res)
	}
	if n := e.srv.Calls(mergePath); n != 1 {
		t.Fatalf("merge endpoint called %d times", n)
	}
}

func TestMerge_RepoWithoutCIMergesAtOnce(t *testing.T) {
	e := newEnv(t)
	e.setPR(`[]`, `"none"`)
	e.srv.SetResource("/repos/o/r/pulls/2",
		[]byte(`{"number":2,"head":{"sha":"abc"},"base":{"ref":"main","sha":"def"},"updated_at":"`+
			time.Now().UTC().Format(time.RFC3339)+`"}`), `"new"`)
	e.srv.SetResource("/repos/o/r/actions/workflows", []byte(`{"total_count":0,"workflows":[]}`), `"w"`)
	e.srv.SetResource("/repos/o/r/commits/def/check-runs", []byte(`{"total_count":0,"check_runs":[]}`), `"bc"`)
	e.srv.SetResource("/repos/o/r/commits/def/status", []byte(`{"total_count":0,"statuses":[]}`), `"bs"`)
	e.srv.SetResponse("PUT", mergePath, 200, []byte(`{"merged":true}`))

	res, err := e.w.Execute(context.Background(), Request{Op: PRMerge, Ref: prRef, Method: "squash"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || e.srv.Calls(mergePath) != 1 {
		t.Fatalf("unexpected result %+v, merge calls %d", res, e.srv.Calls(mergePath))
	}
}

func TestMerge_RepoWithActiveWorkflowWaitsForChecks(t *testing.T) {
	e := newEnv(t)
	e.setPR(`[]`, `"none"`)
	e.srv.SetResource("/repos/o/r/pulls/2",
		[]byte(`{"number":2,"head":{"sha":"abc"},"base":{"ref":"main","sha":"def"},"updated_at":"`+
			time.Now().UTC().Format(time.RFC3339)+`"}`), `"new"`)
	e.srv.SetResource("/repos/o/r/actions/workflows", []byte(`{"total_count":1,"workflows":[{"state":"active"}]}`), `"w"`)
	e.srv.SetResponse("PUT", mergePath, 200, []byte(`{"merged":true}`))

	res, err := e.w.Execute(context.Background(), Request{Op: PRMerge, Ref: prRef, Method: "squash"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRejected || e.srv.Calls(mergePath) != 0 {
		t.Fatalf("unexpected result %+v, merge calls %d", res, e.srv.Calls(mergePath))
	}
}

func TestMerge_FreshReadIgnoresCache(t *testing.T) {
	e := newEnv(t)
	e.setPR(greenRuns, `"green"`)
	ctx := context.Background()
	// Green checks are cached; a max-age read would serve them.
	if _, err := e.w.Client.Get(ctx, prRef, gh.Checks, gh.GetOpts{MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	e.setPR(redRuns, `"red"`)
	e.srv.SetResponse("PUT", mergePath, 200, []byte(`{"merged":true}`))
	runs := e.srv.Calls("/repos/o/r/commits/abc/check-runs")

	res, err := e.w.Execute(ctx, Request{Op: PRMerge, Ref: prRef, Method: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRejected {
		t.Fatalf("merged on stale green checks: %+v", res)
	}
	if e.srv.Calls("/repos/o/r/commits/abc/check-runs") == runs {
		t.Fatal("checks were not read from GitHub")
	}
	if e.srv.Calls(mergePath) != 0 {
		t.Fatal("merge endpoint called")
	}
}

func TestMerge_GreenMergesPinnedToHeadSHA(t *testing.T) {
	e := newEnv(t)
	e.setPR(greenRuns, `"green"`)
	e.srv.SetResponse("PUT", mergePath, 200, []byte(`{"merged":true}`))

	res, err := e.w.Execute(context.Background(), Request{Op: PRMerge, Ref: prRef, Method: "squash"})
	if err != nil || res.Status != StatusOK {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	var body map[string]string
	if err := json.Unmarshal(e.srv.Body(mergePath), &body); err != nil {
		t.Fatal(err)
	}
	if body["merge_method"] != "squash" || body["sha"] != "abc" {
		t.Fatalf("merge body = %v", body)
	}
	if _, ok, _ := e.cache.Get(scope, prRef.Key()+"/pull"); ok {
		t.Fatal("pull still cached after the merge")
	}
}

func TestWrites_SpacedUnderSecondaryLimit(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResponse("POST", "/repos/o/r/issues/2/comments", 201, []byte(`{"id":1}`))
	start := e.clock.Now()
	var sent []time.Time
	for i := 0; i < 70; i++ {
		if _, err := e.w.Execute(context.Background(), Request{Op: PRComment, Ref: prRef, Body: "x"}); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, e.clock.Now())
	}
	if n := e.srv.Calls("/repos/o/r/issues/2/comments"); n != 70 {
		t.Fatalf("comments sent = %d, want 70 (none refused)", n)
	}
	if d := e.clock.Now().Sub(start); d < time.Minute {
		t.Fatalf("70 comments took %s simulated, want >= 60s", d)
	}
	for i := 1; i < len(sent); i++ {
		if sent[i].Sub(sent[i-1]) < time.Second {
			t.Fatalf("writes %d and %d less than 1s apart", i-1, i)
		}
	}
	for i := 60; i < len(sent); i++ {
		if sent[i].Sub(sent[i-60]) < time.Minute {
			t.Fatalf("more than 60 writes within a minute ending at write %d", i)
		}
	}
}

func TestSpacer_SeparateScopesDoNotWait(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	s := NewSpacer(clock.Now, clock.Sleep)
	for _, sc := range []string{"a", "b", "c"} {
		if err := s.Acquire(context.Background(), sc); err != nil {
			t.Fatal(err)
		}
	}
	if !clock.Now().Equal(time.Unix(0, 0)) {
		t.Fatalf("first write of each scope waited until %s", clock.Now())
	}
}

func TestProjectItemEdit_GraphQLMutation(t *testing.T) {
	e := newEnv(t)
	e.srv.SetResource("/graphql", []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_1"}}}}`), "")

	res, err := e.w.Execute(context.Background(), Request{
		Op: ProjectItemEdit, Project: "PVT_1", Item: "PVTI_1", Field: "PVTF_1", Value: "3", ValueType: "number",
	})
	if err != nil || res.Status != StatusOK {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	if n := e.srv.GraphQLCalls(); n != 1 {
		t.Fatalf("GraphQL calls = %d, want 1", n)
	}
	var sent struct {
		Query     string `json:"query"`
		Variables struct {
			Input struct {
				ProjectID string             `json:"projectId"`
				ItemID    string             `json:"itemId"`
				FieldID   string             `json:"fieldId"`
				Value     map[string]float64 `json:"value"`
			} `json:"input"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(e.srv.Body("/graphql"), &sent); err != nil {
		t.Fatal(err)
	}
	in := sent.Variables.Input
	if !strings.Contains(sent.Query, "updateProjectV2ItemFieldValue") ||
		in.ProjectID != "PVT_1" || in.ItemID != "PVTI_1" || in.FieldID != "PVTF_1" || in.Value["number"] != 3 {
		t.Fatalf("unexpected mutation %s", e.srv.Body("/graphql"))
	}
	if !strings.Contains(string(res.Data), "PVTI_1") {
		t.Fatalf("result data = %s", res.Data)
	}
}

func TestExecute_UsageErrors(t *testing.T) {
	e := newEnv(t)
	issueRef := ref.Ref{Host: ref.Host, Owner: "o", Repo: "r", Number: 1, Kind: ref.Issue}
	for _, req := range []Request{
		{Op: PRComment, Ref: prRef},
		{Op: PRComment, Ref: issueRef, Body: "x"},
		{Op: PRMerge, Ref: prRef, Method: "fast-forward"},
		{Op: PRCreate, Ref: prRef, Title: "t"},
		{Op: PREdit, Ref: prRef},
		{Op: ProjectItemEdit, Project: "P", Item: "I", Field: "F", Value: "x", ValueType: "number"},
		{Op: "pr-delete", Ref: prRef},
	} {
		if _, err := e.w.Execute(context.Background(), req); !errors.Is(err, ref.ErrUsage) {
			t.Errorf("%+v: err = %v, want a usage error", req, err)
		}
	}
	if n := e.srv.GraphQLCalls(); n != 0 {
		t.Fatalf("a rejected request reached GitHub")
	}
}
