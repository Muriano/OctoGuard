// Package wait evaluates conditions on GitHub resources and polls them on
// behalf of any number of waiters, one poller per resource.
package wait

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/ref"
)

// Condition is what a waiter waits for.
type Condition string

// Conditions. Which apply depends on the kind of ref; see ParseCondition.
const (
	ChecksDone  Condition = "checks-done"
	ChecksGreen Condition = "checks-green"
	Review      Condition = "review"
	Comment     Condition = "comment"
	Merged      Condition = "merged"
	Closed      Condition = "closed"
	Changed     Condition = "changed"
	Labeled     Condition = "labeled"
	Done        Condition = "done"
)

var conditions = map[ref.Kind][]Condition{
	ref.PR:    {ChecksDone, ChecksGreen, Review, Comment, Merged, Closed, Changed},
	ref.Issue: {Comment, Closed, Labeled, Changed},
	ref.Run:   {Done},
}

// ParseCondition validates s as a condition for a ref of kind.
func ParseCondition(kind ref.Kind, s string) (Condition, error) {
	valid := conditions[kind]
	if c := Condition(s); slices.Contains(valid, c) {
		return c, nil
	}
	names := make([]string, len(valid))
	for i, c := range valid {
		names[i] = string(c)
	}
	return "", fmt.Errorf("%w: unknown condition %q for a %s: use %s", ref.ErrUsage, s, kind, strings.Join(names, ", "))
}

// Needs lists the resources a condition reads.
func Needs(kind ref.Kind, c Condition) []gh.Resource {
	switch c {
	case ChecksDone:
		return []gh.Resource{gh.Checks}
	case ChecksGreen:
		return []gh.Resource{gh.Checks, gh.RequiredChecks}
	case Review:
		return []gh.Resource{gh.Reviews}
	case Comment:
		return []gh.Resource{gh.Comments}
	case Done:
		return []gh.Resource{gh.Runs}
	case Changed:
		if kind == ref.PR {
			return []gh.Resource{gh.PR, gh.Comments}
		}
		return []gh.Resource{gh.Issue, gh.Comments}
	}
	// Merged, Closed, Labeled.
	if kind == ref.PR {
		return []gh.Resource{gh.PR}
	}
	return []gh.Resource{gh.Issue}
}

// Item is one review or comment.
type Item struct {
	ID    int64  `json:"id"`
	User  string `json:"user,omitempty"`
	State string `json:"state,omitempty"` // reviews only
}

// Snapshot is the normalized state of a resource at one point in time. Only
// the parts read for the waiters' conditions are set.
type Snapshot struct {
	State    string     `json:"state,omitempty"` // open or closed
	Merged   bool       `json:"merged,omitempty"`
	Labels   []string   `json:"labels,omitempty"`
	Reviews  []Item     `json:"reviews,omitempty"`
	Comments []Item     `json:"comments,omitempty"`
	Checks   []gh.Check `json:"checks,omitempty"`
	// PRUpdatedAt is when the pull request last changed, at or after the
	// last push to its head; zero when unknown.
	PRUpdatedAt time.Time `json:"-"`
	Required    []string  `json:"required_checks,omitempty"`
	RunStatus   string    `json:"run_status,omitempty"`
	Conclusion  string    `json:"run_conclusion,omitempty"`
}

// Change is one difference between two snapshots.
type Change struct {
	Kind string `json:"kind"` // state, merged, label, review, comment, check, run
	Name string `json:"name,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// BuildSnapshot normalizes the data read for each resource (as returned by
// gh.Client.Get) into a Snapshot.
func BuildSnapshot(parts map[gh.Resource]json.RawMessage) (Snapshot, error) {
	var s Snapshot
	for what, data := range parts {
		var err error
		switch what {
		case gh.PR:
			var pr struct {
				Pull    json.RawMessage `json:"pull"`
				Reviews json.RawMessage `json:"reviews"`
				Checks  gh.ChecksData   `json:"checks"`
			}
			if err = json.Unmarshal(data, &pr); err == nil {
				if err = s.setIssue(pr.Pull); err == nil {
					s.Reviews, err = items(pr.Reviews)
				}
				if s.Checks == nil {
					s.Checks = pr.Checks.Checks
				}
				s.setPRUpdatedAt(pr.Checks.PRUpdatedAt)
			}
		case gh.Issue:
			err = s.setIssue(data)
		case gh.Reviews:
			s.Reviews, err = items(data)
		case gh.Comments:
			s.Comments, err = items(data)
		case gh.Checks:
			var c gh.ChecksData
			if err = json.Unmarshal(data, &c); err == nil {
				s.Checks = c.Checks
				s.setPRUpdatedAt(c.PRUpdatedAt)
			}
		case gh.RequiredChecks:
			var r gh.RequiredData
			if err = json.Unmarshal(data, &r); err == nil {
				s.Required = r.Contexts
			}
		case gh.Runs:
			var run struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}
			if err = json.Unmarshal(data, &run); err == nil {
				s.RunStatus, s.Conclusion = run.Status, run.Conclusion
			}
		}
		if err != nil {
			return Snapshot{}, fmt.Errorf("wait: decode %s: %w", what, err)
		}
	}
	if s.Checks == nil && parts[gh.Checks] != nil {
		s.Checks = []gh.Check{}
	}
	return s, nil
}

func (s *Snapshot) setPRUpdatedAt(v string) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		s.PRUpdatedAt = t
	}
}

func (s *Snapshot) setIssue(data json.RawMessage) error {
	var is struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(data, &is); err != nil {
		return err
	}
	s.State, s.Merged = is.State, is.Merged
	s.Labels = []string{}
	for _, l := range is.Labels {
		s.Labels = append(s.Labels, l.Name)
	}
	sort.Strings(s.Labels)
	return nil
}

func items(data json.RawMessage) ([]Item, error) {
	var raw []struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
		User  *struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(raw))
	for _, r := range raw {
		it := Item{ID: r.ID, State: r.State}
		if r.User != nil {
			it.User = r.User.Login
		}
		out = append(out, it)
	}
	return out, nil
}

// Evaluate reports whether cond holds for cur, given prev, the snapshot taken
// when the wait started, and every change from prev to cur.
func Evaluate(cond Condition, prev, cur Snapshot) (met bool, changes []Change) {
	changes = diff(prev, cur)
	has := func(kind string) bool {
		return slices.ContainsFunc(changes, func(c Change) bool { return c.Kind == kind && (kind != "label" || c.To != "") })
	}
	switch cond {
	case ChecksDone:
		if len(cur.Checks) == 0 {
			met = NoCI(cur)
		} else {
			met = !slices.ContainsFunc(cur.Checks, func(c gh.Check) bool { return c.Status != "completed" })
		}
	case ChecksGreen:
		met = green(cur)
	case Review:
		met = has("review")
	case Comment:
		met = has("comment")
	case Labeled:
		met = has("label")
	case Changed:
		met = len(changes) > 0
	case Merged:
		met = cur.Merged
	case Closed:
		met = cur.State == "closed"
	case Done:
		met = cur.RunStatus == "completed"
	}
	return met, changes
}

// Failed reports whether cond can no longer be met: for checks-green, a
// relevant check ended in failure, cancelled or timed_out.
func Failed(cond Condition, cur Snapshot) bool {
	if cond != ChecksGreen {
		return false
	}
	for _, c := range relevant(cur) {
		switch c.Conclusion {
		case "failure", "cancelled", "timed_out":
			return true
		}
	}
	return false
}

// relevant returns the checks checks-green looks at: the required ones when
// the base branch requires any, otherwise all.
func relevant(s Snapshot) []gh.Check {
	if len(s.Required) == 0 {
		return s.Checks
	}
	var out []gh.Check
	for _, c := range s.Checks {
		if slices.Contains(s.Required, c.Name) {
			out = append(out, c)
		}
	}
	return out
}

// NoCIGrace is how long a pull request must go without any check after its
// last update before it counts as having no CI. CI registers its checks a few
// seconds after a push.
const NoCIGrace = time.Minute

// now is replaced in tests.
var now = time.Now

// NoCI reports whether s looks like a pull request on a repository without
// CI: no check at all, no required check, and no change for NoCIGrace. Right
// after a push there are no checks yet either, so a recent or unknown update
// time is not enough.
func NoCI(s Snapshot) bool {
	return len(s.Checks) == 0 && len(s.Required) == 0 &&
		!s.PRUpdatedAt.IsZero() && now().Sub(s.PRUpdatedAt) >= NoCIGrace
}

// green: every relevant check succeeded, was neutral or skipped, and every
// required check has reported. With no checks at all it holds only for a pull
// request without CI (see NoCI).
func green(s Snapshot) bool {
	if len(s.Checks) == 0 {
		return NoCI(s)
	}
	rel := relevant(s)
	for _, name := range s.Required {
		if !slices.ContainsFunc(rel, func(c gh.Check) bool { return c.Name == name }) {
			return false
		}
	}
	for _, c := range rel {
		switch c.Conclusion {
		case "success", "neutral", "skipped":
		default:
			return false
		}
	}
	return true
}

func diff(prev, cur Snapshot) []Change {
	changes := []Change{}
	add := func(kind, name, from, to string) {
		changes = append(changes, Change{Kind: kind, Name: name, From: from, To: to})
	}
	if prev.State != cur.State && prev.State != "" && cur.State != "" {
		add("state", "", prev.State, cur.State)
	}
	if !prev.Merged && cur.Merged {
		add("merged", "", "false", "true")
	}
	for _, l := range cur.Labels {
		if !slices.Contains(prev.Labels, l) {
			add("label", l, "", l)
		}
	}
	for _, l := range prev.Labels {
		if !slices.Contains(cur.Labels, l) {
			add("label", l, l, "")
		}
	}
	for _, k := range []struct {
		kind       string
		prev, curr []Item
	}{{"review", prev.Reviews, cur.Reviews}, {"comment", prev.Comments, cur.Comments}} {
		for _, it := range k.curr {
			if !slices.ContainsFunc(k.prev, func(p Item) bool { return p.ID == it.ID }) {
				add(k.kind, it.User, "", it.State)
			}
		}
	}
	for _, c := range cur.Checks {
		i := slices.IndexFunc(prev.Checks, func(p gh.Check) bool { return p.Name == c.Name && p.Kind == c.Kind })
		switch {
		case i < 0:
			add("check", c.Name, "", checkState(c))
		case checkState(prev.Checks[i]) != checkState(c):
			add("check", c.Name, checkState(prev.Checks[i]), checkState(c))
		}
	}
	if prev.RunStatus != cur.RunStatus && prev.RunStatus != "" {
		add("run", "status", prev.RunStatus, cur.RunStatus)
	}
	return changes
}

func checkState(c gh.Check) string {
	if c.Conclusion != "" {
		return c.Conclusion
	}
	return c.Status
}
