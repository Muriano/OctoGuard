package wait

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/ref"
)

func check(name, status, conclusion string) gh.Check {
	return gh.Check{Name: name, Kind: "check_run", Status: status, Conclusion: conclusion}
}

func done(name, conclusion string) gh.Check { return check(name, "completed", conclusion) }

func TestChecksGreen_NoRequiredChecksUsesAll(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []gh.Check
		met    bool
	}{
		{"all success", []gh.Check{done("a", "success"), done("b", "success")}, true},
		{"one still running", []gh.Check{done("a", "success"), check("b", "in_progress", "")}, false},
		{"one queued", []gh.Check{check("a", "queued", "")}, false},
		// No check reported yet: CI may not have registered them (#38).
		{"no checks at all", []gh.Check{}, false},
	} {
		cur := Snapshot{Checks: tc.checks}
		if met, _ := Evaluate(ChecksGreen, cur, cur); met != tc.met {
			t.Errorf("%s: met = %t, want %t", tc.name, met, tc.met)
		}
		if Failed(ChecksGreen, cur) {
			t.Errorf("%s: Failed = true", tc.name)
		}
	}
}

func TestNoChecks_NoCIAfterGrace(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return t0 }
	t.Cleanup(func() { now = time.Now })
	for _, tc := range []struct {
		name string
		snap Snapshot
		met  bool
	}{
		// Right after a push CI may not have registered its checks (#38).
		{"just pushed", Snapshot{Checks: []gh.Check{}, PRUpdatedAt: t0.Add(-5 * time.Second)}, false},
		{"update time unknown", Snapshot{Checks: []gh.Check{}}, false},
		// No checks for NoCIGrace even though the repository has CI.
		{"no checks after grace", Snapshot{Checks: []gh.Check{}, PRUpdatedAt: t0.Add(-NoCIGrace)}, true},
		// A repository without CI merges as soon as it can.
		{"repo without CI, just pushed", Snapshot{Checks: []gh.Check{}, RepoWithoutCI: true, PRUpdatedAt: t0}, true},
		{"repo without CI but a required check", Snapshot{Checks: []gh.Check{}, RepoWithoutCI: true, Required: []string{"ci"}}, false},
		{"required check never reported", Snapshot{Checks: []gh.Check{}, Required: []string{"ci"}, PRUpdatedAt: t0.Add(-time.Hour)}, false},
	} {
		for _, cond := range []Condition{ChecksGreen, ChecksDone} {
			if met, _ := Evaluate(cond, tc.snap, tc.snap); met != tc.met {
				t.Errorf("%s %s: met = %t, want %t", cond, tc.name, met, tc.met)
			}
		}
	}
}

func TestBuildSnapshot_PRUpdatedAt(t *testing.T) {
	s, err := BuildSnapshot(map[gh.Resource]json.RawMessage{
		gh.Checks: json.RawMessage(`{"head_sha":"abc","pr_updated_at":"2026-11-13T09:00:00Z","checks":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC); !s.PRUpdatedAt.Equal(want) {
		t.Fatalf("PRUpdatedAt = %v, want %v", s.PRUpdatedAt, want)
	}
}

func TestChecksGreen_RequiredOnly(t *testing.T) {
	cur := Snapshot{
		Checks:   []gh.Check{done("ci", "success"), done("optional", "failure")},
		Required: []string{"ci"},
	}
	if met, _ := Evaluate(ChecksGreen, cur, cur); !met {
		t.Error("a failing non-required check must not block checks-green")
	}
	if Failed(ChecksGreen, cur) {
		t.Error("a failing non-required check must not fail checks-green")
	}
	missing := Snapshot{Checks: []gh.Check{done("ci", "success")}, Required: []string{"ci", "lint"}}
	if met, _ := Evaluate(ChecksGreen, missing, missing); met {
		t.Error("a required check that has not reported yet must keep checks-green pending")
	}
}

func TestChecksGreen_SkippedNeutralCount(t *testing.T) {
	for _, required := range [][]string{nil, {"a", "b", "c"}} {
		cur := Snapshot{
			Checks:   []gh.Check{done("a", "success"), done("b", "skipped"), done("c", "neutral")},
			Required: required,
		}
		if met, _ := Evaluate(ChecksGreen, cur, cur); !met {
			t.Errorf("required=%v: skipped and neutral must count as green", required)
		}
	}
}

func TestChecksGreen_FailureReturnsFailed(t *testing.T) {
	for _, conclusion := range []string{"failure", "cancelled", "timed_out"} {
		cur := Snapshot{Checks: []gh.Check{done("a", "success"), done("b", conclusion), check("c", "in_progress", "")}}
		if met, _ := Evaluate(ChecksGreen, cur, cur); met {
			t.Errorf("%s: met = true", conclusion)
		}
		if !Failed(ChecksGreen, cur) {
			t.Errorf("%s: Failed = false, want true", conclusion)
		}
	}
	// A commit status in "error" is mapped to failure by gh.
	cur := Snapshot{Checks: []gh.Check{{Name: "ext", Kind: "status", Status: "completed", Conclusion: "failure"}}}
	if !Failed(ChecksGreen, cur) {
		t.Error("failing commit status: Failed = false")
	}
	if Failed(ChecksDone, cur) {
		t.Error("checks-done never fails")
	}
}

func TestChecksDone(t *testing.T) {
	for _, tc := range []struct {
		checks []gh.Check
		met    bool
	}{
		{[]gh.Check{done("a", "failure"), done("b", "success")}, true},
		{[]gh.Check{done("a", "success"), check("b", "queued", "")}, false},
		{[]gh.Check{done("a", "success"), check("b", "in_progress", "")}, false},
		{[]gh.Check{{Name: "s", Kind: "status", Status: "pending"}}, false},
	} {
		cur := Snapshot{Checks: tc.checks}
		if met, _ := Evaluate(ChecksDone, cur, cur); met != tc.met {
			t.Errorf("%v: met = %t, want %t", tc.checks, met, tc.met)
		}
	}
}

func TestEvaluate_Events(t *testing.T) {
	base := Snapshot{
		State: "open", Labels: []string{"bug"},
		Comments: []Item{{ID: 1, User: "a"}}, Reviews: []Item{{ID: 1, User: "a", State: "COMMENTED"}},
	}
	withComment := base
	withComment.Comments = []Item{{ID: 1, User: "a"}, {ID: 2, User: "b"}}
	withReview := base
	withReview.Reviews = []Item{{ID: 1, User: "a", State: "COMMENTED"}, {ID: 2, User: "b", State: "APPROVED"}}
	labeled := base
	labeled.Labels = []string{"bug", "ready"}
	unlabeled := base
	unlabeled.Labels = []string{}
	closed := base
	closed.State = "closed"
	merged := closed
	merged.Merged = true

	for _, tc := range []struct {
		name string
		cond Condition
		cur  Snapshot
		met  bool
	}{
		{"no comment", Comment, base, false},
		{"new comment", Comment, withComment, true},
		{"new review", Review, withReview, true},
		{"comment is not a review", Review, withComment, false},
		{"label added", Labeled, labeled, true},
		{"label removed", Labeled, unlabeled, false},
		{"unchanged", Changed, base, false},
		{"label removed is a change", Changed, unlabeled, true},
		{"closed", Closed, closed, true},
		{"open", Closed, base, false},
		{"merged", Merged, merged, true},
		{"closed without merge", Merged, closed, false},
	} {
		if met, changes := Evaluate(tc.cond, base, tc.cur); met != tc.met {
			t.Errorf("%s: met = %t, want %t (changes %v)", tc.name, met, tc.met, changes)
		}
	}
	_, changes := Evaluate(Closed, base, merged)
	if len(changes) != 2 || changes[0] != (Change{Kind: "state", From: "open", To: "closed"}) || changes[1].Kind != "merged" {
		t.Errorf("changes = %+v", changes)
	}
}

func TestEvaluate_RunDone(t *testing.T) {
	prev := Snapshot{RunStatus: "in_progress"}
	cur := Snapshot{RunStatus: "completed", Conclusion: "failure"}
	met, changes := Evaluate(Done, prev, cur)
	if !met || len(changes) != 1 || changes[0].To != "completed" {
		t.Errorf("met = %t, changes = %+v", met, changes)
	}
	if met, _ := Evaluate(Done, prev, prev); met {
		t.Error("in_progress run: met = true")
	}
}

func TestParseCondition(t *testing.T) {
	if c, err := ParseCondition(ref.PR, "checks-green"); err != nil || c != ChecksGreen {
		t.Errorf("pr checks-green: %q, %v", c, err)
	}
	for _, tc := range []struct {
		kind ref.Kind
		s    string
	}{{ref.Issue, "merged"}, {ref.Run, "closed"}, {ref.PR, "labeled"}, {ref.PR, "nope"}} {
		if _, err := ParseCondition(tc.kind, tc.s); !errors.Is(err, ref.ErrUsage) {
			t.Errorf("%s %s: err = %v, want usage error", tc.kind, tc.s, err)
		}
	}
}

func TestBuildSnapshot(t *testing.T) {
	s, err := BuildSnapshot(map[gh.Resource]json.RawMessage{
		gh.PR: json.RawMessage(`{"pull":{"state":"closed","merged":true,"labels":[{"name":"z"},{"name":"a"}]},
			"reviews":[{"id":7,"state":"APPROVED","user":{"login":"r"}}],
			"checks":{"head_sha":"x","checks":[{"name":"ci","status":"completed","conclusion":"success"}]}}`),
		gh.Comments:       json.RawMessage(`[{"id":3,"user":null}]`),
		gh.RequiredChecks: json.RawMessage(`{"base":"main","contexts":["ci"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "closed" || !s.Merged || len(s.Labels) != 2 || s.Labels[0] != "a" ||
		len(s.Reviews) != 1 || s.Reviews[0].User != "r" || len(s.Comments) != 1 ||
		len(s.Checks) != 1 || len(s.Required) != 1 {
		t.Errorf("snapshot = %+v", s)
	}
}
