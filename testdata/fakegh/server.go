// Package fakegh is a fake GitHub API server for tests. It serves fixed
// resources with ETags, answers 304 on a matching If-None-Match, emits
// X-RateLimit-* headers and can fail the next request to a path on demand.
package fakegh

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

const graphQLPath = "/graphql"

type resource struct {
	body []byte
	etag string
}

type rate struct {
	remaining, limit int
	reset            time.Time
}

type response struct {
	status int
	body   []byte
}

type failure struct {
	status     int
	retryAfter time.Duration
}

// Server is a fake GitHub API. All methods are safe for concurrent use.
type Server struct {
	URL string

	mu           sync.Mutex
	resources    map[string]resource
	rates        map[string]*rate
	calls        map[string]int
	failures     map[string]failure
	responses    map[string]response // by "METHOD path", for non-GET requests
	bodies       map[string][]byte   // last request body by path
	graphQLCalls int
}

// New starts a fake server that is closed when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		resources: map[string]resource{},
		rates:     map[string]*rate{},
		calls:     map[string]int{},
		failures:  map[string]failure{},
		responses: map[string]response{},
		bodies:    map[string][]byte{},
	}
	ts := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(ts.Close)
	s.URL = ts.URL
	return s
}

// SetResource makes GET path return body with the given ETag.
// Setting graphQLPath defines the response to POST /graphql.
func (s *Server) SetResource(path string, body []byte, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[path] = resource{body: body, etag: etag}
}

// SetResponse makes a method other than GET (POST, PATCH, PUT, ...) on path
// answer status with body.
func (s *Server) SetResponse(method, path string, status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[method+" "+path] = response{status: status, body: body}
}

// Body returns the body of the last request path received, or nil.
func (s *Server) Body(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[path]
}

// SetRate sets the quota bucket kind ("core", "graphql", "search", ...).
func (s *Server) SetRate(kind string, remaining, limit int, reset time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rates[kind] = &rate{remaining: remaining, limit: limit, reset: reset}
}

// FailNext makes the next request to path answer status with a Retry-After
// header (in whole seconds, rounded up) when retryAfter > 0.
func (s *Server) FailNext(path string, status int, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[path] = failure{status: status, retryAfter: retryAfter}
}

// Calls returns how many requests path received, including 304s and failures.
func (s *Server) Calls(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

// GraphQLCalls returns how many requests POST /graphql received.
func (s *Server) GraphQLCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.graphQLCalls
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := r.URL.Path
	s.calls[path]++
	if body, err := io.ReadAll(r.Body); err == nil && len(body) > 0 {
		s.bodies[path] = body
	}
	kind := "core"
	if path == graphQLPath {
		kind = "graphql"
		s.graphQLCalls++
	}
	rt := s.rateFor(kind)

	if f, ok := s.failures[path]; ok {
		delete(s.failures, path)
		if f.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(f.retryAfter.Seconds()))))
		}
		s.writeRateHeaders(w, kind, rt)
		writeError(w, f.status, "fakegh: injected failure")
		return
	}

	if path == graphQLPath {
		if r.Method != http.MethodPost {
			s.writeRateHeaders(w, kind, rt)
			writeError(w, http.StatusMethodNotAllowed, "fakegh: graphql requires POST")
			return
		}
		body := []byte(`{"data":{}}`)
		if res, ok := s.resources[graphQLPath]; ok {
			body = res.body
		}
		s.serve(w, kind, rt, http.StatusOK, body)
		return
	}

	if r.Method != http.MethodGet {
		if res, ok := s.responses[r.Method+" "+path]; ok {
			s.serve(w, kind, rt, res.status, res.body)
			return
		}
	}
	res, ok := s.resources[path]
	if !ok || r.Method != http.MethodGet {
		s.writeRateHeaders(w, kind, rt)
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if res.etag != "" {
		w.Header().Set("ETag", res.etag)
		if r.Header.Get("If-None-Match") == res.etag {
			// Like GitHub, a 304 does not consume quota.
			s.writeRateHeaders(w, kind, rt)
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	s.serve(w, kind, rt, http.StatusOK, res.body)
}

// rateFor returns the bucket for kind, creating a full default one if unset.
func (s *Server) rateFor(kind string) *rate {
	rt, ok := s.rates[kind]
	if !ok {
		rt = &rate{remaining: 5000, limit: 5000, reset: time.Now().Add(time.Hour)}
		s.rates[kind] = rt
	}
	return rt
}

func (s *Server) serve(w http.ResponseWriter, kind string, rt *rate, status int, body []byte) {
	if rt.remaining > 0 {
		rt.remaining--
	}
	s.writeRateHeaders(w, kind, rt)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) writeRateHeaders(w http.ResponseWriter, kind string, rt *rate) {
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(rt.limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(rt.remaining))
	h.Set("X-RateLimit-Used", strconv.Itoa(rt.limit-rt.remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(rt.reset.Unix(), 10))
	h.Set("X-RateLimit-Resource", kind)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, msg)
}
