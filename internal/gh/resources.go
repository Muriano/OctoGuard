package gh

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Muriano/OctoGuard/internal/ref"
)

// Shapes. REST parts are returned as GitHub sends them. GraphQL reads are
// normalized to the same field names for the subset they select.
//
//	PR:             {"pull": <pull>, "reviews": [<review>...], "checks": <ChecksData>}
//	Issue:          <issue>
//	Reviews:        [<review>...]
//	Comments:       [<comment>...]
//	Checks:         ChecksData
//	Runs:           <run> for a run ref; {"workflow_runs": [...]} for a PR
//	RequiredChecks: RequiredData

// Check is one check run or commit status of a commit.
type Check struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`       // "check_run" or "status"
	Status     string `json:"status"`     // queued, in_progress, completed, pending, ...
	Conclusion string `json:"conclusion"` // success, failure, neutral, skipped, ...; "" until completed
}

// ChecksData is the merge of the check runs and commit statuses of a head SHA.
// PRUpdatedAt is the pull request's updated_at: GitHub moves it on every push
// to the head branch, so it bounds how long ago the head SHA arrived.
type ChecksData struct {
	HeadSHA     string  `json:"head_sha"`
	PRUpdatedAt string  `json:"pr_updated_at,omitempty"`
	Checks      []Check `json:"checks"`
}

// RequiredData lists the status checks the base branch requires, from branch
// protection and rulesets.
type RequiredData struct {
	Base     string   `json:"base"`
	Contexts []string `json:"contexts"`
}

const (
	perPage  = 100
	maxPages = 10
)

// checkKind rejects resources that do not apply to the kind of r.
func checkKind(r ref.Ref, what Resource) error {
	var ok bool
	switch what {
	case PR, Reviews, Checks, RequiredChecks:
		ok = r.Kind == ref.PR
	case Issue:
		ok = r.Kind == ref.Issue
	case Comments:
		ok = r.Kind == ref.PR || r.Kind == ref.Issue
	case Runs:
		ok = r.Kind == ref.Run || r.Kind == ref.PR
	default:
		return fmt.Errorf("%w: unknown resource %q", ref.ErrUsage, what)
	}
	if !ok {
		return fmt.Errorf("%w: %s does not apply to a %s", ref.ErrUsage, what, r.Kind)
	}
	return nil
}

// primaryKey is the REST cache key whose presence makes the router revalidate
// through REST; "" for resources GraphQL cannot serve.
func primaryKey(r ref.Ref, what Resource) string {
	switch what {
	case PR:
		return r.Key() + "/pull"
	case Issue:
		return r.Key() + "/issue"
	case Reviews, Comments:
		return pageKey(r.Key()+"/"+string(what), 1)
	}
	return ""
}

// graphQLKey is the cache key of what read through GraphQL.
func graphQLKey(r ref.Ref, what Resource) string { return r.Key() + "/" + string(what) + "@graphql" }

func pageKey(base string, page int) string { return fmt.Sprintf("%s?page=%d", base, page) }

func repoPath(r ref.Ref) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Repo)
}

// restResource reads what through REST.
func (rd *read) restResource(r ref.Ref, what Resource) ([]byte, error) {
	base, repo := r.Key(), repoPath(r)
	switch what {
	case Issue:
		return rd.rest(base+"/issue", fmt.Sprintf("%s/issues/%d", repo, r.Number))
	case Reviews:
		return rd.reviews(r)
	case Comments:
		return rd.restList(base+"/comments", fmt.Sprintf("%s/issues/%d/comments", repo, r.Number), "")
	case Checks:
		pull, err := rd.pull(r)
		if err != nil {
			return nil, err
		}
		return rd.checks(r, pull)
	case RequiredChecks:
		pull, err := rd.pull(r)
		if err != nil {
			return nil, err
		}
		return rd.required(r, pull.Base.Ref)
	case Runs:
		if r.Kind == ref.Run {
			return rd.rest(base+"/run", fmt.Sprintf("%s/actions/runs/%d", repo, r.Number))
		}
		pull, err := rd.pull(r)
		if err != nil {
			return nil, err
		}
		runs, err := rd.restList(base+"/runs/"+pull.Head.SHA,
			fmt.Sprintf("%s/actions/runs?head_sha=%s", repo, url.QueryEscape(pull.Head.SHA)), "workflow_runs")
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]json.RawMessage{"workflow_runs": runs})
	case PR:
		pull, err := rd.pull(r)
		if err != nil {
			return nil, err
		}
		reviews, err := rd.reviews(r)
		if err != nil {
			return nil, err
		}
		checks, err := rd.checks(r, pull)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]json.RawMessage{"pull": pull.raw, "reviews": reviews, "checks": checks})
	}
	return nil, fmt.Errorf("%w: unknown resource %q", ref.ErrUsage, what)
}

type pullInfo struct {
	raw  json.RawMessage
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	UpdatedAt string `json:"updated_at"`
}

func (rd *read) pull(r ref.Ref) (pullInfo, error) {
	body, err := rd.rest(r.Key()+"/pull", fmt.Sprintf("%s/pulls/%d", repoPath(r), r.Number))
	if err != nil {
		return pullInfo{}, err
	}
	var p pullInfo
	if err := json.Unmarshal(body, &p); err != nil {
		return pullInfo{}, fmt.Errorf("github: decode pull: %w", err)
	}
	if p.Head.SHA == "" {
		return pullInfo{}, fmt.Errorf("github: pull %s has no head sha", r.Key())
	}
	p.raw = body
	return p, nil
}

func (rd *read) reviews(r ref.Ref) (json.RawMessage, error) {
	return rd.restList(r.Key()+"/reviews", fmt.Sprintf("%s/pulls/%d/reviews", repoPath(r), r.Number), "")
}

// restList reads every page of a list endpoint (up to maxPages), each page
// cached and revalidated on its own, and returns the concatenated items.
// field names the array inside an object response; "" for a bare array.
// A page shorter than perPage is the last one.
func (rd *read) restList(keyBase, path, field string) (json.RawMessage, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	items := []json.RawMessage{}
	for page := 1; page <= maxPages; page++ {
		body, err := rd.rest(pageKey(keyBase, page), fmt.Sprintf("%s%sper_page=%d&page=%d", path, sep, perPage, page))
		if err != nil {
			return nil, err
		}
		var got []json.RawMessage
		if field == "" {
			err = json.Unmarshal(body, &got)
		} else {
			var obj map[string]json.RawMessage
			if err = json.Unmarshal(body, &obj); err == nil && obj[field] != nil {
				err = json.Unmarshal(obj[field], &got)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("github: decode %s: %w", path, err)
		}
		items = append(items, got...)
		if len(got) < perPage {
			break
		}
	}
	return json.Marshal(items)
}

// checks merges the check runs and the commit statuses of sha.
func (rd *read) checks(r ref.Ref, pull pullInfo) (json.RawMessage, error) {
	sha := pull.Head.SHA
	base, repo := r.Key()+"/checks/"+sha, repoPath(r)+"/commits/"+url.PathEscape(sha)
	runsRaw, err := rd.restList(base+"/runs", repo+"/check-runs", "check_runs")
	if err != nil {
		return nil, err
	}
	statusRaw, err := rd.rest(base+"/status", repo+"/status?per_page=100")
	if err != nil {
		return nil, err
	}
	var runs []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(runsRaw, &runs); err != nil {
		return nil, fmt.Errorf("github: decode check runs: %w", err)
	}
	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(statusRaw, &combined); err != nil {
		return nil, fmt.Errorf("github: decode statuses: %w", err)
	}
	out := ChecksData{HeadSHA: sha, PRUpdatedAt: pull.UpdatedAt, Checks: []Check{}}
	for _, cr := range runs {
		out.Checks = append(out.Checks, Check{Name: cr.Name, Kind: "check_run", Status: cr.Status, Conclusion: cr.Conclusion})
	}
	for _, st := range combined.Statuses {
		out.Checks = append(out.Checks, statusCheck(st.Context, st.State))
	}
	return json.Marshal(out)
}

// statusCheck maps a commit status state onto the check run vocabulary.
func statusCheck(context, state string) Check {
	c := Check{Name: context, Kind: "status", Status: "completed"}
	switch state = strings.ToLower(state); state {
	case "pending", "expected":
		c.Status = "pending"
	case "error":
		c.Conclusion = "failure"
	default:
		c.Conclusion = state
	}
	return c
}

// required lists the status checks required on branch by classic branch
// protection and by rulesets. Both endpoints are readable with read access.
func (rd *read) required(r ref.Ref, branch string) (json.RawMessage, error) {
	base, repo := r.Key()+"/required/"+branch, repoPath(r)
	b := url.PathEscape(branch)
	branchRaw, err := rd.rest(base+"/branch", repo+"/branches/"+b)
	if err != nil {
		return nil, err
	}
	var br struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := json.Unmarshal(branchRaw, &br); err != nil {
		return nil, fmt.Errorf("github: decode branch: %w", err)
	}
	set := map[string]bool{}
	for _, ctx := range br.Protection.RequiredStatusChecks.Contexts {
		set[ctx] = true
	}

	rulesRaw, err := rd.restList(base+"/rules", repo+"/rules/branches/"+b, "")
	if err != nil {
		return nil, err
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(rulesRaw, &rules); err != nil {
		return nil, fmt.Errorf("github: decode rules: %w", err)
	}
	for _, rule := range rules {
		if rule.Type == "required_status_checks" {
			for _, c := range rule.Parameters.RequiredStatusChecks {
				set[c.Context] = true
			}
		}
	}
	out := RequiredData{Base: branch, Contexts: []string{}}
	for ctx := range set {
		out.Contexts = append(out.Contexts, ctx)
	}
	sort.Strings(out.Contexts)
	return json.Marshal(out)
}

// GraphQL reads. Lists take the most recent 100 items.

const gqlUser = `author { login }`

const gqlReview = `databaseId state body submittedAt ` + gqlUser

const gqlComment = `databaseId body url createdAt updatedAt ` + gqlUser

const gqlChecks = `commits(last: 1) { nodes { commit { oid statusCheckRollup { contexts(first: 100) { nodes {
	__typename
	... on CheckRun { name status conclusion }
	... on StatusContext { context state }
} } } } } }`

const gqlPull = `number title body state isDraft merged mergeable url createdAt updatedAt closedAt mergedAt
	headRefName headRefOid baseRefName ` + gqlUser + ` labels(first: 100) { nodes { name } }`

const gqlHead = `query($owner: String!, $repo: String!, $number: Int!) { rateLimit { cost } repository(owner: $owner, name: $repo) { `

var gqlQueries = map[Resource]string{
	PR: gqlHead + `pullRequest(number: $number) { ` + gqlPull +
		` reviews(last: 100) { nodes { ` + gqlReview + ` } } ` + gqlChecks + ` } } }`,
	Reviews: gqlHead + `pullRequest(number: $number) { reviews(last: 100) { nodes { ` + gqlReview + ` } } } } }`,
	Issue: gqlHead + `issue(number: $number) { number title body state url createdAt updatedAt closedAt ` +
		gqlUser + ` labels(first: 100) { nodes { name } } } } }`,
	Comments: gqlHead + `issueOrPullRequest(number: $number) {
		... on Issue { comments(last: 100) { nodes { ` + gqlComment + ` } } }
		... on PullRequest { comments(last: 100) { nodes { ` + gqlComment + ` } } } } } }`,
}

type gqlActor struct {
	Login string `json:"login"`
}

type gqlLabels struct {
	Nodes []struct {
		Name string `json:"name"`
	} `json:"nodes"`
}

type gqlReviewNode struct {
	DatabaseID  int64     `json:"databaseId"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	SubmittedAt *string   `json:"submittedAt"`
	Author      *gqlActor `json:"author"`
}

type gqlCommentNode struct {
	DatabaseID int64     `json:"databaseId"`
	Body       string    `json:"body"`
	URL        string    `json:"url"`
	CreatedAt  string    `json:"createdAt"`
	UpdatedAt  string    `json:"updatedAt"`
	Author     *gqlActor `json:"author"`
}

type gqlRepo struct {
	Repository struct {
		PullRequest *struct {
			Number      int       `json:"number"`
			Title       string    `json:"title"`
			Body        string    `json:"body"`
			State       string    `json:"state"`
			IsDraft     bool      `json:"isDraft"`
			Merged      bool      `json:"merged"`
			Mergeable   string    `json:"mergeable"`
			URL         string    `json:"url"`
			CreatedAt   string    `json:"createdAt"`
			UpdatedAt   string    `json:"updatedAt"`
			ClosedAt    *string   `json:"closedAt"`
			MergedAt    *string   `json:"mergedAt"`
			HeadRefName string    `json:"headRefName"`
			HeadRefOid  string    `json:"headRefOid"`
			BaseRefName string    `json:"baseRefName"`
			Author      *gqlActor `json:"author"`
			Labels      gqlLabels `json:"labels"`
			Reviews     struct {
				Nodes []gqlReviewNode `json:"nodes"`
			} `json:"reviews"`
			Commits struct {
				Nodes []struct {
					Commit struct {
						Oid               string `json:"oid"`
						StatusCheckRollup *struct {
							Contexts struct {
								Nodes []struct {
									Typename   string `json:"__typename"`
									Name       string `json:"name"`
									Status     string `json:"status"`
									Conclusion string `json:"conclusion"`
									Context    string `json:"context"`
									State      string `json:"state"`
								} `json:"nodes"`
							} `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"pullRequest"`
		Issue *struct {
			Number    int       `json:"number"`
			Title     string    `json:"title"`
			Body      string    `json:"body"`
			State     string    `json:"state"`
			URL       string    `json:"url"`
			CreatedAt string    `json:"createdAt"`
			UpdatedAt string    `json:"updatedAt"`
			ClosedAt  *string   `json:"closedAt"`
			Author    *gqlActor `json:"author"`
			Labels    gqlLabels `json:"labels"`
		} `json:"issue"`
		IssueOrPullRequest *struct {
			Comments struct {
				Nodes []gqlCommentNode `json:"nodes"`
			} `json:"comments"`
		} `json:"issueOrPullRequest"`
	} `json:"repository"`
}

// graphQLResource reads what through GraphQL, normalized to the REST shape.
func (rd *read) graphQLResource(r ref.Ref, what Resource) ([]byte, error) {
	query, ok := gqlQueries[what]
	if !ok {
		return nil, fmt.Errorf("github: %s cannot be read through graphql", what)
	}
	vars := map[string]any{"owner": r.Owner, "repo": r.Repo, "number": r.Number}
	return rd.graphQL(graphQLKey(r, what), query, vars, func(data []byte) ([]byte, error) {
		return normalizeGraphQL(r, what, data)
	})
}

func normalizeGraphQL(r ref.Ref, what Resource, data []byte) ([]byte, error) {
	var g gqlRepo
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("github: decode graphql %s: %w", what, err)
	}
	repo := g.Repository
	notFound := &APIError{Status: 404, Message: "Not Found: " + r.Key()}
	switch what {
	case Issue:
		is := repo.Issue
		if is == nil {
			return nil, notFound
		}
		return json.Marshal(map[string]any{
			"number": is.Number, "title": is.Title, "body": is.Body, "state": strings.ToLower(is.State),
			"html_url": is.URL, "created_at": is.CreatedAt, "updated_at": is.UpdatedAt, "closed_at": is.ClosedAt,
			"user": user(is.Author), "labels": labels(is.Labels),
		})
	case Comments:
		if repo.IssueOrPullRequest == nil {
			return nil, notFound
		}
		out := []map[string]any{}
		for _, c := range repo.IssueOrPullRequest.Comments.Nodes {
			out = append(out, map[string]any{
				"id": c.DatabaseID, "body": c.Body, "html_url": c.URL,
				"created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "user": user(c.Author),
			})
		}
		return json.Marshal(out)
	}

	p := repo.PullRequest
	if p == nil {
		return nil, notFound
	}
	reviews := []map[string]any{}
	for _, rv := range p.Reviews.Nodes {
		reviews = append(reviews, map[string]any{
			"id": rv.DatabaseID, "state": rv.State, "body": rv.Body, "submitted_at": rv.SubmittedAt, "user": user(rv.Author),
		})
	}
	if what == Reviews {
		return json.Marshal(reviews)
	}

	checks := ChecksData{HeadSHA: p.HeadRefOid, PRUpdatedAt: p.UpdatedAt, Checks: []Check{}}
	if n := p.Commits.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		for _, c := range n[0].Commit.StatusCheckRollup.Contexts.Nodes {
			if c.Typename == "StatusContext" {
				checks.Checks = append(checks.Checks, statusCheck(c.Context, c.State))
				continue
			}
			checks.Checks = append(checks.Checks, Check{
				Name: c.Name, Kind: "check_run", Status: strings.ToLower(c.Status), Conclusion: strings.ToLower(c.Conclusion),
			})
		}
	}
	var mergeable *bool
	switch p.Mergeable {
	case "MERGEABLE":
		mergeable = ptr(true)
	case "CONFLICTING":
		mergeable = ptr(false)
	}
	state := "open"
	if p.State != "OPEN" {
		state = "closed"
	}
	pull := map[string]any{
		"number": p.Number, "title": p.Title, "body": p.Body, "state": state, "draft": p.IsDraft,
		"merged": p.Merged, "mergeable": mergeable, "html_url": p.URL,
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "closed_at": p.ClosedAt, "merged_at": p.MergedAt,
		"user": user(p.Author), "labels": labels(p.Labels),
		"head": map[string]string{"ref": p.HeadRefName, "sha": p.HeadRefOid},
		"base": map[string]string{"ref": p.BaseRefName},
	}
	return json.Marshal(map[string]any{"pull": pull, "reviews": reviews, "checks": checks})
}

func user(a *gqlActor) map[string]string {
	if a == nil {
		return nil // deleted ("ghost") account
	}
	return map[string]string{"login": a.Login}
}

func labels(l gqlLabels) []map[string]string {
	out := []map[string]string{}
	for _, n := range l.Nodes {
		out = append(out, map[string]string{"name": n.Name})
	}
	return out
}

func ptr[T any](v T) *T { return &v }
