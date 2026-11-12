package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/wait"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

const waitIssuePath = "/repos/o/r/issues/1"

var waitIssueRef = ref.Ref{Host: ref.Host, Owner: "o", Repo: "r", Number: 1, Kind: ref.Issue}

func setWaitConfig(t *testing.T, cfg wait.Config) {
	t.Helper()
	prev := waitConfig
	waitConfig = cfg
	t.Cleanup(func() { waitConfig = prev })
}

func waitArgs(t *testing.T, a WaitArgs) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func status(t *testing.T) Status {
	t.Helper()
	resp := call(t, Request{Op: OpStatus})
	var st Status
	if err := json.Unmarshal(resp.Data, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestWait_DaemonMet(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResource(waitIssuePath, []byte(`{"number":1,"state":"closed"}`), `"v1"`)
	startDaemon(t, Config{IdleTimeout: time.Minute, APIURL: srv.URL})

	resp := call(t, Request{Op: OpWait, Token: "wait-token",
		Args: waitArgs(t, WaitArgs{Ref: waitIssueRef, Condition: wait.Closed, Timeout: time.Second})})
	var res wait.Result
	if !resp.OK || json.Unmarshal(resp.Data, &res) != nil || res.Status != wait.StatusMet {
		t.Fatalf("resp = %+v", resp)
	}

	bad := call(t, Request{Op: OpWait, Token: "wait-token",
		Args: waitArgs(t, WaitArgs{Ref: waitIssueRef, Condition: wait.Merged, Timeout: time.Second})})
	if bad.OK || bad.Code != CodeUsage {
		t.Fatalf("merged on an issue: %+v, want a usage error", bad)
	}
}

func TestWait_ClientDisconnectReleasesWaiter(t *testing.T) {
	isolate(t)
	setWaitConfig(t, wait.Config{ChecksBase: 10 * time.Millisecond, OtherBase: 10 * time.Millisecond, Grace: 200 * time.Millisecond})
	srv := fakegh.New(t)
	srv.SetResource(waitIssuePath, []byte(`{"number":1,"state":"open"}`), `"v1"`)
	startDaemon(t, Config{IdleTimeout: time.Minute, APIURL: srv.URL})

	// The agent's CLI starts a long wait and is killed half way.
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := DialRunning(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = conn.Call(ctx, Request{Op: OpWait, Token: "wait-token",
			Args: waitArgs(t, WaitArgs{Ref: waitIssueRef, Condition: wait.Closed, Timeout: time.Hour})})
	}()
	eventually(t, "the waiter to subscribe", func() bool {
		st := status(t)
		return st.Waiters == 1 && srv.Calls(waitIssuePath) >= 2
	})
	cancel()
	_ = conn.Close()

	eventually(t, "the waiter to be released", func() bool { return status(t).Waiters == 0 })
	if st := status(t); st.Pollers != 1 {
		t.Fatalf("pollers = %d right after the disconnect, want 1 (grace)", st.Pollers)
	}
	eventually(t, "the poller to stop after the grace", func() bool { return status(t).Pollers == 0 })
	calls := srv.Calls(waitIssuePath)
	time.Sleep(100 * time.Millisecond)
	if after := srv.Calls(waitIssuePath); after != calls {
		t.Fatalf("GitHub still polled after the grace: %d → %d", calls, after)
	}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
