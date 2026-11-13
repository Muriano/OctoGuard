// Package write performs OctoGuard's GitHub writes: each one is spaced under
// the secondary rate limit for content creation, invalidates the cache of
// what it touches, and a merge first validates the checks fresh.
package write

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/internal/wait"
)

// Op is a kind of write.
type Op string

// Writes of v1.
const (
	PRCreate        Op = "pr-create"
	PRComment       Op = "pr-comment"
	PREdit          Op = "pr-edit"
	PRMerge         Op = "pr-merge"
	IssueComment    Op = "issue-comment"
	IssueCreate     Op = "issue-create"
	ProjectItemAdd  Op = "project-item-add"
	ProjectItemEdit Op = "project-item-edit"
)

// Request is one write. Ref is the PR or issue written to; for a create only
// its Host, Owner and Repo are used. Project fields take node IDs.
type Request struct {
	Op        Op       `json:"op"`
	Ref       ref.Ref  `json:"ref"`
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body,omitempty"`
	Base      string   `json:"base,omitempty"`
	Head      string   `json:"head,omitempty"`
	Labels    []string `json:"labels,omitempty"` // create: labels; edit: labels to add
	Method    string   `json:"method,omitempty"` // merge: merge, squash or rebase
	Project   string   `json:"project,omitempty"`
	Item      string   `json:"item,omitempty"`
	Content   string   `json:"content,omitempty"`
	Field     string   `json:"field,omitempty"`
	Value     string   `json:"value,omitempty"`
	ValueType string   `json:"value_type,omitempty"` // text (default), number, date or single-select
}

// Statuses of a Result.
const (
	StatusOK       = "ok"
	StatusRejected = "rejected"
)

// Result is the outcome of a write. A merge refused because the checks are
// not green is a Result with StatusRejected, not an error.
type Result struct {
	Status string          `json:"status"`
	Reason string          `json:"reason,omitempty"`
	Hint   string          `json:"hint,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Spacer keeps the writes of each scope (host + token) under GitHub's
// secondary limit for content creation: at most one per second and 60 per
// minute. Writes over the limit wait their turn; none is refused.
type Spacer struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu    sync.Mutex
	slots map[string][]time.Time // the last 60 reserved slots per scope
}

const (
	minGap    = time.Second
	perWindow = 60
	window    = time.Minute
)

// NewSpacer returns a Spacer on the given clock; nil means the real one.
func NewSpacer(now func() time.Time, sleep func(ctx context.Context, d time.Duration) error) *Spacer {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepCtx
	}
	return &Spacer{now: now, sleep: sleep, slots: map[string][]time.Time{}}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Acquire reserves the next free slot of scope and waits for it. Callers are
// served in the order they reserve.
func (s *Spacer) Acquire(ctx context.Context, scope string) error {
	s.mu.Lock()
	now := s.now()
	slot := now
	prev := s.slots[scope]
	if n := len(prev); n > 0 {
		slot = latest(slot, prev[n-1].Add(minGap))
		if n >= perWindow {
			slot = latest(slot, prev[n-perWindow].Add(window))
		}
	}
	prev = append(prev, slot)
	if len(prev) > perWindow {
		prev = prev[len(prev)-perWindow:]
	}
	s.slots[scope] = prev
	s.mu.Unlock()
	if d := slot.Sub(now); d > 0 {
		return s.sleep(ctx, d)
	}
	return nil
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Writer performs writes with one token.
type Writer struct {
	Client *gh.Client
	Cache  *cache.Cache
	Spacer *Spacer
	Scope  string // gh.Scope of the token
}

func usagef(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ref.ErrUsage}, args...)...)
}

// Execute performs req.
func (w *Writer) Execute(ctx context.Context, req Request) (Result, error) {
	if err := validate(req); err != nil {
		return Result{}, err
	}
	r := req.Ref
	repo := "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Repo)
	switch req.Op {
	case PRCreate:
		data, err := w.do(ctx, "POST", repo+"/pulls", map[string]any{
			"title": req.Title, "body": req.Body, "head": req.Head, "base": req.Base,
		})
		if err != nil || len(req.Labels) == 0 {
			return ok(data), err
		}
		var pr struct {
			Number int `json:"number"`
		}
		if err := json.Unmarshal(data, &pr); err != nil || pr.Number <= 0 {
			return Result{}, fmt.Errorf("write: pull created but its number is unreadable; labels not added")
		}
		if _, err := w.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/labels", repo, pr.Number), map[string]any{"labels": req.Labels}); err != nil {
			return Result{}, fmt.Errorf("write: pull #%d created, adding labels failed: %w", pr.Number, err)
		}
		return ok(data), nil
	case IssueCreate:
		body := map[string]any{"title": req.Title, "body": req.Body}
		if len(req.Labels) > 0 {
			body["labels"] = req.Labels
		}
		data, err := w.do(ctx, "POST", repo+"/issues", body)
		return ok(data), err
	case PRComment, IssueComment:
		data, err := w.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/comments", repo, r.Number), map[string]any{"body": req.Body})
		return w.invalidate(r, ok(data), err)
	case PREdit:
		return w.edit(ctx, req, repo)
	case PRMerge:
		return w.merge(ctx, req, repo)
	case ProjectItemAdd:
		return w.mutate(ctx, `mutation($input: AddProjectV2ItemByIdInput!) { addProjectV2ItemById(input: $input) { item { id } } }`,
			map[string]any{"projectId": req.Project, "contentId": req.Content})
	case ProjectItemEdit:
		value, err := fieldValue(req.Value, req.ValueType)
		if err != nil {
			return Result{}, err
		}
		return w.mutate(ctx, `mutation($input: UpdateProjectV2ItemFieldValueInput!) { updateProjectV2ItemFieldValue(input: $input) { projectV2Item { id } } }`,
			map[string]any{"projectId": req.Project, "itemId": req.Item, "fieldId": req.Field, "value": value})
	}
	return Result{}, usagef("unknown write %q", req.Op)
}

func validate(req Request) error {
	r := req.Ref
	switch req.Op {
	case ProjectItemAdd:
		if req.Project == "" || req.Content == "" {
			return usagef("project item-add needs --project and --content-id")
		}
		return nil
	case ProjectItemEdit:
		if req.Project == "" || req.Item == "" || req.Field == "" {
			return usagef("project item-edit needs --project, --id and --field")
		}
		return nil
	}
	if r.Host != ref.Host || r.Owner == "" || r.Repo == "" {
		return usagef("invalid ref: v1 supports github.com owner/repo")
	}
	switch req.Op {
	case PRCreate:
		if req.Title == "" || req.Head == "" || req.Base == "" {
			return usagef("pr create needs --title, --head and --base")
		}
		return nil
	case IssueCreate:
		if req.Title == "" {
			return usagef("issue create needs --title")
		}
		return nil
	}
	want := ref.PR
	if req.Op == IssueComment {
		want = ref.Issue
	}
	if r.Number <= 0 || r.Kind != want {
		return usagef("%s needs a %s ref", req.Op, want)
	}
	switch req.Op {
	case PRComment, IssueComment:
		if req.Body == "" {
			return usagef("a comment needs --body or --body-file")
		}
	case PREdit:
		if req.Title == "" && req.Body == "" && req.Base == "" && len(req.Labels) == 0 {
			return usagef("pr edit needs --title, --body, --body-file, --base or --add-label")
		}
	case PRMerge:
		if !slices.Contains([]string{"merge", "squash", "rebase"}, req.Method) {
			return usagef("merge method must be merge, squash or rebase")
		}
	}
	return nil
}

func ok(data json.RawMessage) Result { return Result{Status: StatusOK, Data: data} }

// do spaces and sends one REST write.
func (w *Writer) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	if err := w.Spacer.Acquire(ctx, w.Scope); err != nil {
		return nil, err
	}
	return w.Client.Do(ctx, method, path, body)
}

// invalidate drops every cache entry of r once a write to it went through.
func (w *Writer) invalidate(r ref.Ref, res Result, err error) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	if err := w.Cache.InvalidatePrefix(w.Scope, r.Key()+"/"); err != nil {
		return Result{}, err
	}
	return res, nil
}

func (w *Writer) edit(ctx context.Context, req Request, repo string) (Result, error) {
	r := req.Ref
	patch := map[string]any{}
	for k, v := range map[string]string{"title": req.Title, "body": req.Body, "base": req.Base} {
		if v != "" {
			patch[k] = v
		}
	}
	var data json.RawMessage
	var err error
	if len(patch) > 0 {
		data, err = w.do(ctx, "PATCH", fmt.Sprintf("%s/pulls/%d", repo, r.Number), patch)
		if err != nil {
			return Result{}, err
		}
	}
	if len(req.Labels) > 0 {
		labels, lerr := w.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/labels", repo, r.Number), map[string]any{"labels": req.Labels})
		if data == nil {
			data = labels
		}
		err = lerr
	}
	// Invalidate even when only the labels failed: the PATCH went through.
	if ierr := w.Cache.InvalidatePrefix(w.Scope, r.Key()+"/"); ierr != nil && err == nil {
		err = ierr
	}
	if err != nil {
		return Result{}, err
	}
	return ok(data), nil
}

// merge reads the checks fresh and merges only when checks-green holds,
// pinning the head SHA that was validated.
func (w *Writer) merge(ctx context.Context, req Request, repo string) (Result, error) {
	r := req.Ref
	fresh := gh.GetOpts{Fresh: true}
	checks, err := w.Client.Get(ctx, r, gh.Checks, fresh)
	if err != nil {
		return Result{}, err
	}
	required, err := w.Client.Get(ctx, r, gh.RequiredChecks, fresh)
	if err != nil {
		return Result{}, err
	}
	snap, err := wait.BuildSnapshot(map[gh.Resource]json.RawMessage{gh.Checks: checks.Data, gh.RequiredChecks: required.Data})
	if err != nil {
		return Result{}, err
	}
	if met, _ := wait.Evaluate(wait.ChecksGreen, wait.Snapshot{}, snap); !met {
		return Result{
			Status: StatusRejected,
			Reason: notGreen(snap, wait.Failed(wait.ChecksGreen, snap)),
			Hint:   fmt.Sprintf("octoguard wait pr %s/%s#%d --until checks-green", r.Owner, r.Repo, r.Number),
		}, nil
	}
	var cd gh.ChecksData
	if err := json.Unmarshal(checks.Data, &cd); err != nil {
		return Result{}, fmt.Errorf("write: decode checks: %w", err)
	}
	body := map[string]any{"merge_method": req.Method}
	if cd.HeadSHA != "" {
		body["sha"] = cd.HeadSHA
	}
	data, err := w.do(ctx, "PUT", fmt.Sprintf("%s/pulls/%d/merge", repo, r.Number), body)
	return w.invalidate(r, ok(data), err)
}

// notGreen explains why checks-green does not hold.
func notGreen(s wait.Snapshot, failed bool) string {
	var bad []string
	for _, c := range s.Checks {
		if len(s.Required) > 0 && !slices.Contains(s.Required, c.Name) {
			continue
		}
		switch c.Conclusion {
		case "success", "neutral", "skipped":
		default:
			state := c.Conclusion
			if state == "" {
				state = c.Status
			}
			bad = append(bad, c.Name+"="+state)
		}
	}
	for _, name := range s.Required {
		if !slices.ContainsFunc(s.Checks, func(c gh.Check) bool { return c.Name == name }) {
			bad = append(bad, name+"=missing")
		}
	}
	what := "checks are not green yet"
	if failed {
		what = "checks failed"
	}
	if len(bad) == 0 {
		if len(s.Checks) == 0 {
			return fmt.Sprintf("%s: no checks reported yet (if none appear, it merges %s after the PR's last update)", what, wait.NoCIGrace)
		}
		return what
	}
	return what + ": " + strings.Join(bad, ", ")
}

func (w *Writer) mutate(ctx context.Context, query string, input map[string]any) (Result, error) {
	if err := w.Spacer.Acquire(ctx, w.Scope); err != nil {
		return Result{}, err
	}
	m := &gh.Mutation{Query: query}
	if err := w.Client.Mutate(ctx, m, input); err != nil {
		return Result{}, err
	}
	return ok(m.Data), nil
}

// fieldValue builds a ProjectV2FieldValue.
func fieldValue(v, typ string) (map[string]any, error) {
	switch typ {
	case "", "text":
		return map[string]any{"text": v}, nil
	case "number":
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, usagef("--value %q is not a number", v)
		}
		return map[string]any{"number": n}, nil
	case "date":
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return nil, usagef("--value %q is not a YYYY-MM-DD date", v)
		}
		return map[string]any{"date": v}, nil
	case "single-select":
		return map[string]any{"singleSelectOptionId": v}, nil
	}
	return nil, usagef("--value-type must be text, number, date or single-select")
}
