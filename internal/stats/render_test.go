package stats

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/quota"
)

var update = flag.Bool("update", false, "rewrite golden files")

// sampleReport is a fixed 24h report with every section populated.
func sampleReport(t *testing.T) Report {
	t.Helper()
	at := fixedNow(t)
	t.Setenv(BaselineEnv, "")
	hour := at.Truncate(time.Hour)
	var buckets []cache.Bucket
	for i := 0; i < 24; i++ {
		h := hour.Add(-time.Duration(i) * time.Hour)
		buckets = append(buckets,
			cache.Bucket{Name: "rest_call", Hour: h, N: int64(i % 7)},
			cache.Bucket{Name: "cache_hit", Hour: h, N: int64((i * 5) % 23)},
		)
	}
	buckets = append(buckets,
		cache.Bucket{Name: "etag_304", Hour: hour, N: 31},
		cache.Bucket{Name: "dedup", Hour: hour, N: 12},
		cache.Bucket{Name: "wait_saved", Hour: hour.Add(-3 * time.Hour), N: 88},
		cache.Bucket{Name: "ratelimit_hit", Hour: hour, N: 1},
		cache.Bucket{Name: "throttle", Hour: hour, N: 4},
	)
	snap := map[string]quota.Bucket{
		"core":    {Remaining: 1900, Limit: 5000, Reset: at.Add(37 * time.Minute), Known: true},
		"graphql": {Remaining: 120, Limit: 5000, Reset: at.Add(65 * time.Minute), Known: true},
	}
	return Compute(buckets, snap, 24*time.Hour)
}

func TestRender_Golden(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, sampleReport(t), 80, false)

	golden := filepath.Join("testdata", "stats_80cols.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if buf.String() != string(want) {
		t.Errorf("output differs from %s (run with -update after checking it):\n%s", golden, buf.String())
	}
	assertMaxWidth(t, buf.String(), 80)
}

func TestRender_NoColorAndNarrow(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	Render(&buf, sampleReport(t), 40, false)
	out := buf.String()
	if strings.Contains(out, "\x1b") {
		t.Errorf("output has ANSI codes:\n%q", out)
	}
	assertMaxWidth(t, out, 40)
}

func TestRender_ColorKeepsWidth(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, sampleReport(t), 40, true)
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatal("expected ANSI codes with color on")
	}
	assertMaxWidth(t, stripANSI(out), 40)
}

func TestRender_UnknownQuota(t *testing.T) {
	fixedNow(t)
	var buf bytes.Buffer
	Render(&buf, Compute(nil, nil, time.Hour), 80, false)
	if !strings.Contains(buf.String(), "core      unknown") {
		t.Errorf("missing unknown quota row:\n%s", buf.String())
	}
}

func TestRender_DropsUnsafeResourceNames(t *testing.T) {
	fixedNow(t)
	snap := map[string]quota.Bucket{
		"code_scanning":   {Remaining: 9, Limit: 10, Known: true},
		"x\x1b]0;pwn\x07": {Remaining: 9, Limit: 10, Known: true},
	}
	var buf bytes.Buffer
	Render(&buf, Compute(nil, snap, time.Hour), 80, false)
	if strings.Contains(buf.String(), "\x1b") || !strings.Contains(buf.String(), "code_scanning") {
		t.Errorf("unexpected quota rows:\n%q", buf.String())
	}
}

func assertMaxWidth(t *testing.T, out string, width int) {
	t.Helper()
	for i, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if n := utf8.RuneCountInString(l); n > width {
			t.Errorf("line %d has %d columns (> %d): %q", i+1, n, width, l)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
