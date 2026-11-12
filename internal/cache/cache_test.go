package cache

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Cache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "octoguard", "cache.db")
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, path
}

func mustPut(t *testing.T, c *Cache, tokenID, key string) {
	t.Helper()
	if err := c.Put(tokenID, key, Entry{Body: []byte(key), Via: "rest", FetchedAt: time.Now()}); err != nil {
		t.Fatalf("Put(%q, %q): %v", tokenID, key, err)
	}
}

func exists(t *testing.T, c *Cache, tokenID, key string) bool {
	t.Helper()
	_, ok, err := c.Get(tokenID, key)
	if err != nil {
		t.Fatalf("Get(%q, %q): %v", tokenID, key, err)
	}
	return ok
}

func TestCache_PutGet(t *testing.T) {
	c, _ := openTemp(t)

	if _, ok, err := c.Get("tok", "missing"); ok || err != nil {
		t.Fatalf("Get(missing) = ok %v, err %v; want false, nil", ok, err)
	}

	want := Entry{
		Body:         []byte(`{"number":12}`),
		ETag:         `W/"abc"`,
		LastModified: "Wed, 11 Nov 2026 20:00:00 GMT",
		FetchedAt:    time.Date(2026, 11, 11, 20, 1, 2, 345, time.FixedZone("CET", 3600)),
		Via:          "graphql",
	}
	if err := c.Put("tok", "github.com/o/r/pr/12", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.Get("tok", "github.com/o/r/pr/12")
	if err != nil || !ok {
		t.Fatalf("Get = ok %v, err %v", ok, err)
	}
	if !bytes.Equal(got.Body, want.Body) || got.ETag != want.ETag || got.LastModified != want.LastModified || got.Via != want.Via {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}
	if !got.FetchedAt.Equal(want.FetchedAt) || got.FetchedAt.Location() != time.UTC {
		t.Fatalf("FetchedAt = %v, want %v in UTC", got.FetchedAt, want.FetchedAt)
	}

	// Put over an existing key overwrites it.
	if err := c.Put("tok", "github.com/o/r/pr/12", Entry{Body: []byte("v2"), Via: "rest"}); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	got, _, _ = c.Get("tok", "github.com/o/r/pr/12")
	if string(got.Body) != "v2" || got.ETag != "" || got.Via != "rest" {
		t.Fatalf("after overwrite Get = %+v", got)
	}
}

func TestCache_IsolatedByToken(t *testing.T) {
	c, _ := openTemp(t)
	const key = "github.com/o/r/pr/12"
	if err := c.Put("github.com:aaaa", key, Entry{Body: []byte("A"), Via: "rest"}); err != nil {
		t.Fatal(err)
	}
	if exists(t, c, "github.com:bbbb", key) {
		t.Fatal("entry of token A is visible to token B")
	}
	mustPut(t, c, "github.com:bbbb", key)

	if err := c.Invalidate("github.com:bbbb", key); err != nil {
		t.Fatal(err)
	}
	if err := c.InvalidatePrefix("github.com:bbbb", "github.com/o/r/"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get("github.com:aaaa", key)
	if err != nil || !ok || string(got.Body) != "A" {
		t.Fatalf("token A entry after token B invalidation: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestCache_Invalidate(t *testing.T) {
	c, _ := openTemp(t)
	for _, k := range []string{"a", "b", "c"} {
		mustPut(t, c, "tok", k)
	}
	if err := c.Invalidate("tok"); err != nil {
		t.Fatalf("Invalidate with no keys: %v", err)
	}
	if err := c.Invalidate("tok", "a", "c", "nope"); err != nil {
		t.Fatal(err)
	}
	if exists(t, c, "tok", "a") || exists(t, c, "tok", "c") || !exists(t, c, "tok", "b") {
		t.Fatal("Invalidate removed the wrong keys")
	}
}

func TestCache_InvalidatePrefix(t *testing.T) {
	c, _ := openTemp(t)
	gone := []string{
		"github.com/o/r_x/pr/12/",
		"github.com/o/r_x/pr/12/reviews",
		"github.com/o/r_x/pr/12/checks",
	}
	kept := []string{
		"github.com/o/r_x/pr/123/reviews", // longer number
		"github.com/o/r_x/pr/1",
		"github.com/o/rax/pr/12/reviews", // `_` must not be a wildcard
		"github.com/o/R_X/pr/12/reviews", // case-sensitive
		"github.com/o/r_x/issue/12/comments",
	}
	for _, k := range append(append([]string{}, gone...), kept...) {
		mustPut(t, c, "tok", k)
	}
	mustPut(t, c, "other", gone[1])

	if err := c.InvalidatePrefix("tok", "github.com/o/r_x/pr/12/"); err != nil {
		t.Fatal(err)
	}
	for _, k := range gone {
		if exists(t, c, "tok", k) {
			t.Errorf("%q survived InvalidatePrefix", k)
		}
	}
	for _, k := range kept {
		if !exists(t, c, "tok", k) {
			t.Errorf("%q was wrongly invalidated", k)
		}
	}
	if !exists(t, c, "other", gone[1]) {
		t.Error("other token's entry was invalidated")
	}

	// `%` in the prefix is literal too.
	mustPut(t, c, "tok", "x%y")
	if err := c.InvalidatePrefix("tok", "%"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, c, "tok", "x%y") {
		t.Error("`%` acted as a wildcard")
	}
}

func TestCache_ConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")

	// Open both handles at the same time: WAL switch and schema creation race.
	var caches [2]*Cache
	var openErr [2]error
	var wg sync.WaitGroup
	for i := range caches {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			caches[i], openErr[i] = Open(path)
		}(i)
	}
	wg.Wait()
	for i, err := range openErr {
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		t.Cleanup(func() { _ = caches[i].Close() })
	}

	const workers, ops = 4, 50
	at := time.Date(2026, 11, 11, 20, 30, 0, 0, time.UTC)
	errs := make(chan error, 2*workers*ops*2)
	for i, c := range caches {
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(c *Cache, id string) {
				defer wg.Done()
				for n := 0; n < ops; n++ {
					key := fmt.Sprintf("github.com/o/r/pr/%s-%d", id, n)
					if err := c.Put("tok", key, Entry{Body: []byte(key), Via: "rest"}); err != nil {
						errs <- fmt.Errorf("Put: %w", err)
					}
					if err := c.IncCounter("tok", "rest_call", 1, at); err != nil {
						errs <- fmt.Errorf("IncCounter: %w", err)
					}
				}
			}(c, fmt.Sprintf("%d.%d", i, w))
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	buckets, err := caches[0].Counters("tok", at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(2 * workers * ops); len(buckets) != 1 || buckets[0].N != want {
		t.Fatalf("Counters = %+v, want one bucket with N=%d", buckets, want)
	}
	if !exists(t, caches[1], "tok", "github.com/o/r/pr/0.0-0") {
		t.Fatal("write from first handle not visible to second")
	}
}

func TestCache_FileMode0600(t *testing.T) {
	c, path := openTemp(t)
	mustPut(t, c, "tok", "k")

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if os.IsNotExist(err) && p != path {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(p), mode)
		}
	}
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %o, want 700", mode)
	}
}

func TestCache_CountersHourly(t *testing.T) {
	c, _ := openTemp(t)
	h := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	inc := func(tok, name string, n int64, at time.Time) {
		t.Helper()
		if err := c.IncCounter(tok, name, n, at); err != nil {
			t.Fatal(err)
		}
	}
	inc("tok", "cache_hit", 1, h.Add(-30*time.Minute)) // before since
	inc("tok", "cache_hit", 2, h.Add(5*time.Minute))
	inc("tok", "cache_hit", 3, h.Add(59*time.Minute))
	inc("tok", "etag_304", 1, h.Add(10*time.Minute))
	inc("tok", "cache_hit", 4, h.Add(61*time.Minute))
	inc("other", "cache_hit", 100, h.Add(5*time.Minute))

	got, err := c.Counters("tok", h)
	if err != nil {
		t.Fatal(err)
	}
	want := []Bucket{
		{Name: "cache_hit", Hour: h, N: 5},
		{Name: "etag_304", Hour: h, N: 1},
		{Name: "cache_hit", Hour: h.Add(time.Hour), N: 4},
	}
	if len(got) != len(want) {
		t.Fatalf("Counters = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].N != want[i].N || !got[i].Hour.Equal(want[i].Hour) || got[i].Hour.Location() != time.UTC {
			t.Errorf("bucket %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCache_PathWithSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Application Support", "a#b?c", "cache.db")
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	mustPut(t, c, "tok", "k")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at the given path: %v", err)
	}
	if !exists(t, c, "tok", "k") {
		t.Fatal("entry not readable")
	}
}

func TestCache_CostRoundTrip(t *testing.T) {
	c, _ := openTemp(t)
	const key = "github.com/o/r/issue/1/issue@graphql"
	if err := c.Put("tok", key, Entry{Body: []byte("a"), Via: "graphql", Cost: 3}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := c.Get("tok", key); got.Cost != 3 {
		t.Fatalf("Cost = %d, want 3", got.Cost)
	}
	if err := c.Put("tok", key, Entry{Body: []byte("b"), Via: "graphql", Cost: 5}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := c.Get("tok", key); got.Cost != 5 {
		t.Fatalf("Cost after overwrite = %d, want 5", got.Cost)
	}
}

func TestCache_MigratesEntriesWithoutCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE entries (
		token_id TEXT NOT NULL, key TEXT NOT NULL, body BLOB NOT NULL, etag TEXT NOT NULL,
		last_modified TEXT NOT NULL, fetched_at INTEGER NOT NULL, via TEXT NOT NULL,
		PRIMARY KEY (token_id, key)) WITHOUT ROWID;
		INSERT INTO entries VALUES ('tok', 'k', x'61', '', '', 0, 'graphql');`)
	_ = old.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the migration is idempotent
		c, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		got, ok, err := c.Get("tok", "k")
		_ = c.Close()
		if err != nil || !ok || got.Cost != 0 || string(got.Body) != "a" {
			t.Fatalf("Open #%d: Get = %+v ok=%v err=%v", i, got, ok, err)
		}
	}
}
