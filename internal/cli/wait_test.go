package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/stats"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

type waitOutput struct {
	SchemaVersion int    `json:"schema_version"`
	Ref           string `json:"ref"`
	Status        string `json:"status"`
	Condition     string `json:"condition"`
	Changes       []struct {
		Kind string `json:"kind"`
	} `json:"changes"`
	State struct {
		State  string `json:"state"`
		Checks []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"checks"`
	} `json:"state"`
}

func decodeWait(t *testing.T, stdout string) waitOutput {
	t.Helper()
	var w waitOutput
	if err := json.Unmarshal([]byte(stdout), &w); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	return w
}

func TestWait_TimeoutReturnsPendingExit3(t *testing.T) {
	isolate(t)
	t.Setenv(stats.BaselineEnv, "10ms")
	srv := fakegh.New(t)
	srv.SetResource("/repos/o/r/issues/1", []byte(`{"number":1,"state":"open"}`), `"v1"`)
	startDaemon(t, srv.URL)
	setDial(t, daemon.DialRunning)

	stdout, stderr, err := run(t, "wait", "issue", "o/r#1", "--until", "closed", "--timeout", "200ms")
	if !errors.Is(err, ErrPending) {
		t.Fatalf("err = %v, want ErrPending (stderr %q)", err, stderr)
	}
	w := decodeWait(t, stdout)
	if w.SchemaVersion != 1 || w.Status != "pending" || w.Condition != "closed" || w.State.State != "open" {
		t.Fatalf("unexpected output: %s", stdout)
	}

	// wait_saved: a 10ms baseline over 200ms is 20 polls; the daemon made 1.
	ch, err := openCache()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ch.Close() }()
	buckets, err := ch.Counters(gh.Scope(ref.Host, auth.FromValue("test-token")), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var saved int64
	for _, b := range buckets {
		if b.Name == stats.CounterWaitSaved {
			saved += b.N
		}
	}
	if saved < 15 {
		t.Fatalf("wait_saved = %d, want about 19", saved)
	}
}

func TestWait_ChecksGreenFailedExit0(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResource("/repos/o/r/pulls/1", []byte(`{"number":1,"state":"open","head":{"sha":"abc"},"base":{"ref":"main"}}`), `"p"`)
	srv.SetResource("/repos/o/r/commits/abc/check-runs", []byte(`{"check_runs":[
		{"name":"ci","status":"completed","conclusion":"failure"},
		{"name":"lint","status":"in_progress","conclusion":null}]}`), `"c"`)
	srv.SetResource("/repos/o/r/commits/abc/status", []byte(`{"statuses":[]}`), `"s"`)
	srv.SetResource("/repos/o/r/branches/main", []byte(`{"protection":{}}`), `"b"`)
	srv.SetResource("/repos/o/r/rules/branches/main", []byte(`[]`), `"r"`)
	setDial(t, noDaemon)

	stdout, stderr, err := run(t, "--api-url", srv.URL, "wait", "pr", "o/r#1", "--until", "checks-green", "--timeout", "5s")
	if err != nil {
		t.Fatalf("wait: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "daemon unavailable") {
		t.Fatalf("stderr %q lacks the fallback notice", stderr)
	}
	w := decodeWait(t, stdout)
	if w.Status != "failed" || len(w.State.Checks) != 2 {
		t.Fatalf("unexpected output: %s", stdout)
	}
}

func TestWait_MetThroughDaemon(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResource("/repos/o/r/actions/runs/7", []byte(`{"id":7,"status":"completed","conclusion":"success"}`), `"r"`)
	startDaemon(t, srv.URL)
	setDial(t, daemon.DialRunning)

	stdout, stderr, err := run(t, "wait", "run", "o/r#7")
	if err != nil {
		t.Fatalf("wait: %v (stderr %q)", err, stderr)
	}
	if w := decodeWait(t, stdout); w.Status != "met" || w.Condition != "done" {
		t.Fatalf("unexpected output: %s", stdout)
	}
	if strings.Contains(stdout+stderr, "test-token") {
		t.Fatal("token leaked to the output")
	}
}

func TestWait_UsageErrors(t *testing.T) {
	isolate(t)
	setDial(t, noDaemon)
	for _, args := range [][]string{
		{"wait", "nope", "o/r#1", "--until", "closed"},
		{"wait", "pr", "o/r#1"},                         // --until required
		{"wait", "issue", "o/r#1", "--until", "merged"}, // not an issue condition
		{"wait", "pr", "o/r#1", "--until", "closed", "--timeout", "0s"},
	} {
		if _, _, err := run(t, args...); !errors.Is(err, ref.ErrUsage) {
			t.Errorf("%v: err = %v, want a usage error", args, err)
		}
	}
}
