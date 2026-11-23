// Package write performs OctoGuard's GitHub writes: each one is spaced under
// the secondary rate limit for content creation, invalidates the cache of
// what it touches, and a merge first validates the checks fresh.
package write

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/gh"
	"github.com/Muriano/OctoGuard/internal/ref"
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
// its Host, Owner and Repo are used. A project item-add takes either node IDs
// (Project, Content: GraphQL) or ProjectOwner and ProjectNumber with Ref as
// the content (REST, core quota).
type Request struct {
	Op        Op       `json:"op"`
	Ref       ref.Ref  `json:"ref"`
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body,omitempty"`
	Base      string   `json:"base,omitempty"`
	Head      string   `json:"head,omitempty"`
	Labels    []string `json:"labels,omitempty"`    // create: labels; edit: labels to add
	Reviewers []string `json:"reviewers,omitempty"` // logins or org/team; "@me" is the token's user
	Assignees []string `json:"assignees,omitempty"` // logins; "@me" is the token's user
	Method    string   `json:"method,omitempty"`    // merge: merge, squash or rebase
	Project   string   `json:"project,omitempty"`
	// ProjectOwner and ProjectNumber name a project for the REST item-add.
	ProjectOwner  string `json:"project_owner,omitempty"`
	ProjectNumber int    `json:"project_number,omitempty"`
	Item          string `json:"item,omitempty"`
	Content       string `json:"content,omitempty"`
	Field         string `json:"field,omitempty"`
	Value         string `json:"value,omitempty"`
	ValueType     string `json:"value_type,omitempty"` // text (default), number, date or single-select
}

// Statuses of a Result.
const (
	StatusOK       = "ok"
	StatusRejected = "rejected"
)

// Result is the outcome of a write. A merge GitHub refuses is a Result with
// StatusRejected, not an error.
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
		if err != nil || len(req.Labels)+len(req.Reviewers)+len(req.Assignees) == 0 {
			return ok(data), err
		}
		var pr struct {
			Number int `json:"number"`
		}
		if err := json.Unmarshal(data, &pr); err != nil || pr.Number <= 0 {
			return Result{}, fmt.Errorf("write: pull created but its number is unreadable; labels, reviewers and assignees not added")
		}
		if err := w.extras(ctx, repo, pr.Number, req); err != nil {
			return Result{}, fmt.Errorf("write: pull #%d created, %w", pr.Number, err)
		}
		return ok(data), nil
	case IssueCreate:
		body := map[string]any{"title": req.Title, "body": req.Body}
		if len(req.Labels) > 0 {
			body["labels"] = req.Labels
		}
		assignees, err := w.resolveMe(ctx, req.Assignees)
		if err != nil {
			return Result{}, err
		}
		if len(assignees) > 0 {
			body["assignees"] = assignees
		}
		data, err := w.do(ctx, "POST", repo+"/issues", body)
		if err != nil {
			return Result{}, err
		}
		if err := checkAssigned(data, assignees); err != nil {
			var is struct {
				Number int `json:"number"`
			}
			_ = json.Unmarshal(data, &is)
			return Result{}, fmt.Errorf("write: issue #%d created, %w", is.Number, err)
		}
		return ok(data), nil
	case PRComment, IssueComment:
		data, err := w.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/comments", repo, r.Number), map[string]any{"body": req.Body})
		return w.invalidate(r, ok(data), err)
	case PREdit:
		return w.edit(ctx, req, repo)
	case PRMerge:
		return w.merge(ctx, req, repo)
	case ProjectItemAdd:
		if req.ProjectOwner != "" {
			return w.projectItemAdd(ctx, req)
		}
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
		if req.ProjectOwner != "" {
			if req.ProjectNumber <= 0 || r.Host != ref.Host || r.Owner == "" || r.Repo == "" || r.Number <= 0 {
				return usagef("project item-add --project OWNER/NUMBER needs --pr or --issue <ref>")
			}
			return nil
		}
		if req.Project == "" || req.Content == "" {
			return usagef("project item-add needs --project and --content-id (PVT_ IDs) or --pr/--issue (OWNER/NUMBER)")
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
		if req.Title == "" && req.Body == "" && req.Base == "" && len(req.Labels)+len(req.Reviewers)+len(req.Assignees) == 0 {
			return usagef("pr edit needs --title, --body, --body-file, --base, --add-label, --add-reviewer or --add-assignee")
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
	if err = w.extras(ctx, repo, r.Number, req); err != nil {
		if data != nil {
			err = fmt.Errorf("write: pull #%d edited, %w", r.Number, err)
		} else {
			err = fmt.Errorf("write: pull #%d: %w", r.Number, err)
		}
	}
	// Invalidate even when only an extra failed: the PATCH went through.
	if ierr := w.Cache.InvalidatePrefix(w.Scope, r.Key()+"/"); ierr != nil && err == nil {
		err = ierr
	}
	if err != nil {
		return Result{}, err
	}
	return ok(data), nil
}

// merge asks GitHub to merge: octoguard applies no criteria of its own, the
// agent decides when to merge and GitHub decides whether it can. The head SHA
// comes from a fresh read and is pinned, so a push in between is not merged
// unseen. A refusal from GitHub (405 not mergeable, 409 head moved) is a
// Result with StatusRejected.
func (w *Writer) merge(ctx context.Context, req Request, repo string) (Result, error) {
	r := req.Ref
	checks, err := w.Client.Get(ctx, r, gh.Checks, gh.GetOpts{Fresh: true})
	if err != nil {
		return Result{}, err
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
	var apiErr *gh.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusMethodNotAllowed || apiErr.Status == http.StatusConflict) {
		return Result{
			Status: StatusRejected,
			Reason: "GitHub refused the merge: " + apiErr.Message,
			Hint:   fmt.Sprintf("octoguard wait pr %s/%s#%d --until changed", r.Owner, r.Repo, r.Number),
		}, nil
	}
	return w.invalidate(r, ok(data), err)
}

// extras adds the labels, reviewers and assignees of req to PR n, in that
// order, stopping at the first failure.
func (w *Writer) extras(ctx context.Context, repo string, n int, req Request) error {
	if len(req.Labels) > 0 {
		if _, err := w.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/labels", repo, n), map[string]any{"labels": req.Labels}); err != nil {
			return fmt.Errorf("adding labels failed: %w", err)
		}
	}
	if len(req.Reviewers) > 0 {
		reviewers, err := w.resolveMe(ctx, req.Reviewers)
		if err != nil {
			return err
		}
		users, teams := []string{}, []string{}
		for _, r := range reviewers {
			if _, team, isTeam := strings.Cut(r, "/"); isTeam {
				teams = append(teams, team)
			} else {
				users = append(users, r)
			}
		}
		if _, err := w.do(ctx, "POST", fmt.Sprintf("%s/pulls/%d/requested_reviewers", repo, n),
			map[string]any{"reviewers": users, "team_reviewers": teams}); err != nil {
			return fmt.Errorf("requesting reviewers failed: %w", err)
		}
	}
	if len(req.Assignees) > 0 {
		assignees, err := w.resolveMe(ctx, req.Assignees)
		if err != nil {
			return err
		}
		data, err := w.do(ctx, "POST", fmt.Sprintf("%s/issues/%d/assignees", repo, n), map[string]any{"assignees": assignees})
		if err != nil {
			return fmt.Errorf("adding assignees failed: %w", err)
		}
		return checkAssigned(data, assignees)
	}
	return nil
}

// resolveMe replaces "@me" in logins with the login of the token's user,
// asking GitHub only when "@me" is there.
func (w *Writer) resolveMe(ctx context.Context, logins []string) ([]string, error) {
	if !slices.Contains(logins, "@me") {
		return logins, nil
	}
	data, err := w.Client.Do(ctx, "GET", "/user", nil)
	if err != nil {
		return nil, fmt.Errorf("write: resolve @me: %w", err)
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(data, &u); err != nil || u.Login == "" {
		return nil, fmt.Errorf("write: resolve @me: no login in GET /user")
	}
	out := make([]string, len(logins))
	for i, l := range logins {
		if l == "@me" {
			l = u.Login
		}
		out[i] = l
	}
	return out, nil
}

// checkAssigned reports the logins GitHub left out of the "assignees" of
// data: it drops users who cannot be assigned without an error.
func checkAssigned(data json.RawMessage, want []string) error {
	if len(want) == 0 {
		return nil
	}
	var got struct {
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return fmt.Errorf("assignees unverifiable: %w", err)
	}
	assigned := map[string]bool{}
	for _, a := range got.Assignees {
		assigned[strings.ToLower(a.Login)] = true
	}
	var missing []string
	for _, login := range want {
		if !assigned[strings.ToLower(login)] {
			missing = append(missing, login)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("GitHub did not assign %s (no access to the repository?)", strings.Join(missing, ", "))
	}
	return nil
}

// projectItemAdd adds the PR or issue req.Ref to a Projects (v2) project by
// REST, which spends core quota rather than GraphQL. The owner is tried as an
// organization first, then as a user.
func (w *Writer) projectItemAdd(ctx context.Context, req Request) (Result, error) {
	r := req.Ref
	typ := "Issue"
	if r.Kind == ref.PR {
		typ = "PullRequest"
	}
	body := map[string]any{"type": typ, "owner": r.Owner, "repo": r.Repo, "number": r.Number}
	owner := url.PathEscape(req.ProjectOwner)
	data, err := w.do(ctx, "POST", fmt.Sprintf("/orgs/%s/projectsV2/%d/items", owner, req.ProjectNumber), body)
	var apiErr *gh.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		data, err = w.do(ctx, "POST", fmt.Sprintf("/users/%s/projectsV2/%d/items", owner, req.ProjectNumber), body)
	}
	if err != nil {
		return Result{}, err
	}
	return ok(data), nil
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
