package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Muriano/OctoGuard/internal/daemon"
	"github.com/Muriano/OctoGuard/internal/ref"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

type writeOutput struct {
	SchemaVersion int             `json:"schema_version"`
	Op            string          `json:"op"`
	Status        string          `json:"status"`
	Reason        string          `json:"reason"`
	Hint          string          `json:"hint"`
	Data          json.RawMessage `json:"data"`
}

func decodeWrite(t *testing.T, stdout string) writeOutput {
	t.Helper()
	var w writeOutput
	if err := json.Unmarshal([]byte(stdout), &w); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	return w
}

func TestMergeCLI_GitHubRefusalExit4(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResource("/repos/o/r/pulls/1", []byte(`{"number":1,"head":{"sha":"abc"},"base":{"ref":"main"}}`), `"p"`)
	srv.SetResource("/repos/o/r/commits/abc/check-runs", []byte(`{"check_runs":[
		{"name":"ci","status":"completed","conclusion":"failure"}]}`), `"c"`)
	srv.SetResource("/repos/o/r/commits/abc/status", []byte(`{"statuses":[]}`), `"s"`)
	srv.SetResource("/repos/o/r/branches/main", []byte(`{"protection":{}}`), `"b"`)
	srv.SetResource("/repos/o/r/rules/branches/main", []byte(`[]`), `"r"`)
	srv.SetResponse("PUT", "/repos/o/r/pulls/1/merge", 405, []byte(`{"message":"Pull Request is not mergeable"}`))
	setDial(t, noDaemon)

	stdout, stderr, err := run(t, "--api-url", srv.URL, "pr", "merge", "o/r#1", "--squash")
	if !errors.Is(err, ErrMergeRejected) {
		t.Fatalf("err = %v, want ErrMergeRejected (stderr %q)", err, stderr)
	}
	w := decodeWrite(t, stdout)
	if w.SchemaVersion != 1 || w.Status != "rejected" || !strings.Contains(w.Reason, "not mergeable") ||
		w.Hint != "octoguard wait pr o/r#1 --until changed" {
		t.Fatalf("unexpected output: %s", stdout)
	}
}

func TestWriteCLI_CommentThroughDaemon(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResponse("POST", "/repos/o/r/issues/3/comments", 201, []byte(`{"id":42}`))
	startDaemon(t, srv.URL)
	setDial(t, daemon.DialRunning)

	stdout, stderr, err := run(t, "issue", "comment", "o/r#3", "--body", "hello")
	if err != nil {
		t.Fatalf("issue comment: %v (stderr %q)", err, stderr)
	}
	if w := decodeWrite(t, stdout); w.Status != "ok" || w.Op != "issue-comment" || !strings.Contains(string(w.Data), "42") {
		t.Fatalf("unexpected output: %s", stdout)
	}
	if got := string(srv.Body("/repos/o/r/issues/3/comments")); got != `{"body":"hello"}` {
		t.Fatalf("request body = %s", got)
	}
}

func TestWriteCLI_CreateFromBodyFileWithoutDaemon(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResponse("POST", "/repos/o/r/pulls", 201, []byte(`{"number":5}`))
	srv.SetResponse("POST", "/repos/o/r/issues/5/labels", 200, []byte(`[]`))
	setDial(t, noDaemon)
	file := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(file, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := run(t, "--api-url", srv.URL, "pr", "create", "-R", "o/r",
		"--title", "T", "--head", "feat", "--base", "main", "--body-file", file, "--label", "bug")
	if err != nil {
		t.Fatalf("pr create: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "daemon unavailable") {
		t.Fatalf("stderr %q lacks the fallback notice", stderr)
	}
	if w := decodeWrite(t, stdout); w.Status != "ok" {
		t.Fatalf("unexpected output: %s", stdout)
	}
	var sent map[string]string
	if err := json.Unmarshal(srv.Body("/repos/o/r/pulls"), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["body"] != "from file" || sent["head"] != "feat" || sent["base"] != "main" {
		t.Fatalf("create body = %v", sent)
	}
	if got := string(srv.Body("/repos/o/r/issues/5/labels")); got != `{"labels":["bug"]}` {
		t.Fatalf("labels body = %s", got)
	}
}

func TestWriteCLI_UsageErrors(t *testing.T) {
	isolate(t)
	setDial(t, noDaemon)
	for _, args := range [][]string{
		{"pr", "comment", "o/r#1"},
		{"pr", "merge", "o/r#1", "--squash", "--rebase"},
		{"issue", "create", "-R", "o/r"},
		{"project", "item-edit", "--project", "P"},
		{"project", "item-add", "--project", "acme", "--pr", "o/r#1"},
		{"project", "item-add", "--project", "acme/x", "--pr", "o/r#1"},
	} {
		if _, _, err := run(t, args...); err == nil {
			t.Errorf("%v: no error", args)
		} else if !errors.Is(err, ref.ErrUsage) && !strings.Contains(err.Error(), "none of the others") {
			t.Errorf("%v: err = %v, want a usage error", args, err)
		}
	}
}

func TestWriteCLI_EditReviewersAndProjectByREST(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResponse("POST", "/repos/o/r/pulls/5/requested_reviewers", 201, []byte(`{}`))
	srv.SetResponse("POST", "/repos/o/r/issues/5/assignees", 201, []byte(`{"assignees":[{"login":"bo"}]}`))
	srv.SetResponse("POST", "/orgs/acme/projectsV2/1/items", 201, []byte(`{"id":9}`))
	setDial(t, noDaemon)

	if _, stderr, err := run(t, "--api-url", srv.URL, "pr", "edit", "o/r#5", "--add-reviewer", "ana,acme/core", "--add-assignee", "bo"); err != nil {
		t.Fatalf("pr edit: %v (stderr %q)", err, stderr)
	}
	if got := string(srv.Body("/repos/o/r/pulls/5/requested_reviewers")); got != `{"reviewers":["ana"],"team_reviewers":["core"]}` {
		t.Fatalf("reviewers body = %s", got)
	}
	if _, stderr, err := run(t, "--api-url", srv.URL, "project", "item-add", "--project", "acme/1", "--pr", "o/r#5"); err != nil {
		t.Fatalf("item-add: %v (stderr %q)", err, stderr)
	}
	if got := string(srv.Body("/orgs/acme/projectsV2/1/items")); got != `{"number":5,"owner":"o","repo":"r","type":"PullRequest"}` {
		t.Fatalf("item-add body = %s", got)
	}
	if srv.GraphQLCalls() != 0 {
		t.Fatal("spent GraphQL")
	}
}
