package fakegh

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, url, etag string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestFakeGH_ETag304(t *testing.T) {
	s := New(t)
	const path = "/repos/o/r/pulls/1"
	s.SetResource(path, []byte(`{"number":1}`), `"abc"`)

	first := get(t, s.URL+path, "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.StatusCode)
	}
	if body, _ := io.ReadAll(first.Body); string(body) != `{"number":1}` {
		t.Fatalf("first body = %q", body)
	}
	etag := first.Header.Get("ETag")
	if etag != `"abc"` {
		t.Fatalf("ETag = %q", etag)
	}

	second := get(t, s.URL+path, etag)
	if second.StatusCode != http.StatusNotModified {
		t.Fatalf("second status = %d, want 304", second.StatusCode)
	}
	if got := s.Calls(path); got != 2 {
		t.Fatalf("Calls = %d, want 2", got)
	}
}

func TestFakeGH_RateHeaders(t *testing.T) {
	s := New(t)
	const path = "/repos/o/r/issues/2"
	reset := time.Unix(1_900_000_000, 0)
	s.SetResource(path, []byte(`{}`), `"e"`)
	s.SetRate("core", 10, 5000, reset)

	resp := get(t, s.URL+path, "")
	h := resp.Header
	if h.Get("X-RateLimit-Remaining") != "9" || h.Get("X-RateLimit-Limit") != "5000" ||
		h.Get("X-RateLimit-Used") != "4991" || h.Get("X-RateLimit-Reset") != "1900000000" ||
		h.Get("X-RateLimit-Resource") != "core" {
		t.Fatalf("unexpected rate headers: %v", h)
	}
	// A 304 does not consume quota.
	if r := get(t, s.URL+path, `"e"`); r.Header.Get("X-RateLimit-Remaining") != "9" {
		t.Fatalf("304 consumed quota: remaining = %s", r.Header.Get("X-RateLimit-Remaining"))
	}
}

func TestFakeGH_FailNext(t *testing.T) {
	s := New(t)
	const path = "/repos/o/r/pulls/3"
	s.SetResource(path, []byte(`{}`), "")
	s.FailNext(path, http.StatusTooManyRequests, 1500*time.Millisecond)

	resp := get(t, s.URL+path, "")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("got %d Retry-After=%q, want 429 and 2", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp := get(t, s.URL+path, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("failure not one-shot: status %d", resp.StatusCode)
	}
}

func TestFakeGH_GraphQL(t *testing.T) {
	s := New(t)
	s.SetResource("/graphql", []byte(`{"data":{"x":1}}`), "")
	s.SetRate("graphql", 100, 5000, time.Now().Add(time.Hour))
	resp, err := http.Post(s.URL+"/graphql", "application/json", strings.NewReader(`{"query":"{x}"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"data":{"x":1}}` || resp.Header.Get("X-RateLimit-Resource") != "graphql" {
		t.Fatalf("body=%q resource=%q", body, resp.Header.Get("X-RateLimit-Resource"))
	}
	if s.GraphQLCalls() != 1 {
		t.Fatalf("GraphQLCalls = %d, want 1", s.GraphQLCalls())
	}
}
