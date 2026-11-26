package skills

import (
	"strings"
	"testing"
)

func TestGuard_Blocks(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"gh pr view 12", "octoguard get pr"},
		{"gh pr checks 12 --watch", "octoguard wait pr"},
		{"gh pr checks 12", "octoguard get checks"},
		{"gh pr merge 12 --squash", "octoguard pr merge"},
		{"gh pr comment 12 --body hi", "octoguard pr comment"},
		{"gh pr create --title t", "octoguard pr create"},
		{"gh pr edit 12 --add-label x", "octoguard pr edit"},
		{"gh pr edit 12 --add-reviewer x --add-assignee @me", "octoguard pr edit"},
		{"gh pr edit 12 --title=foo", "octoguard pr edit"},
		// Quoted values are never flags.
		{`gh pr create --title t --body "adds --enforce and -x"`, "octoguard pr create"},
		{"gh pr edit 12 --body 'drop --auto here'", "octoguard pr edit"},
		{"gh pr create -t t --body \"line one\n--draft is not a flag\"", "octoguard pr create"},
		{`gh pr edit 12 --title="a --draft b"`, "octoguard pr edit"},
		{"gh pr create -t x -B main -H f -r ana -a @me", "octoguard pr create"},
		{"gh issue create -t x --assignee ana", "octoguard issue create"},
		{"gh issue view 3", "octoguard get issue"},
		{"gh issue comment 3 -b x", "octoguard issue comment"},
		{"gh issue create -t x", "octoguard issue create"},
		{"gh run watch 99", "octoguard wait run"},
		{"gh run view 99", "octoguard get runs"},
		{"gh api rate_limit", "octoguard quota"},
		{"gh api user --jq .login", "octoguard quota"},
		{"gh api repos/o/r/pulls/1", "octoguard get"},
		{"gh api repos/o/r/commits/abc/check-runs --jq .", "octoguard get checks"},
		{"gh api -X PUT repos/o/r/pulls/1/merge -f merge_method=squash", "octoguard pr merge"},
		{"gh api 'repos/o/r/actions/runs?head_sha=abc'", "octoguard get runs"},
		{"gh api repos/o/r/actions/runs/9", "octoguard get runs"},
		{"gh api -X POST repos/o/r/issues/1/comments -f body=x", "octoguard pr comment"},
		{"gh run view 99 -R o/r", "--log-failed"},
		{"gh pr view 12 --json state,title", "octoguard get pr"},
		{"gh issue view 3 --json title,body,comments", "octoguard get issue"},
		{"gh pr checks 12 --watch --fail-fast", "octoguard wait pr"},
		{`gh api graphql -f query='{repository(owner:"o",name:"r"){pullRequest(number:1){state}}}'`, "octoguard get"},
		// Inside loops, pipes, subshells and after env assignments.
		{"until gh pr checks 1; do sleep 30; done", "octoguard wait pr"},
		{"sleep 60 && gh pr view 1", "octoguard get pr"},
		{"x=$(gh api repos/o/r/pulls/1 --jq .state)", "octoguard get"},
		{"GH_TOKEN=t gh pr view 1", "octoguard get pr"},
		{"cd repo; /opt/homebrew/bin/gh pr merge 1", "octoguard pr merge"},
		{"echo ok\ngh issue view 2", "octoguard get issue"},
		{"(cd repo && gh pr view 1)", "octoguard get pr"},
		// Substitutions run even inside double quotes.
		{`state="$(gh pr view 1 --json state)"`, "octoguard get pr"},
		{"echo \"`gh pr view 1`\"", "octoguard get pr"},
		// A heredoc does not hide the commands after it.
		{"cat <<'EOF' > f\ntext\nEOF\ngh pr merge 1", "octoguard pr merge"},
	} {
		deny, reason := Guard(tc.cmd)
		if !deny || !strings.Contains(reason, tc.want) {
			t.Errorf("Guard(%q) = %t, %q; want deny mentioning %q", tc.cmd, deny, reason, tc.want)
		}
	}
}

func TestGuard_Allows(t *testing.T) {
	for _, cmd := range []string{
		"gh release create v1",
		"gh repo view",
		"gh auth status",
		"gh pr list",
		"gh run list -w Release",
		"gh secret list",
		"gh pr diff 3",
		// Flags octoguard has no equivalent for: gh does it.
		"gh pr edit 12 --remove-label x",
		"gh pr edit 12 --milestone v1",
		"gh pr merge 12 --squash --auto",
		"gh pr merge 12 --squash --delete-branch",
		"gh pr create --title t --draft",
		"gh issue create -t x --milestone=v1",
		"gh pr comment 12 --edit-last -b x",
		"gh issue comment 3 --delete-last",
		"gh api repos/o/r/contents/README.md",
		"gh api graphql -f query='{viewer{login}}'",
		// Reads octoguard does not serve: job logs, jobs, web, extra --json fields.
		"gh run view 45074797106 -R o/r --log-failed",
		"gh run view 9 --log",
		"gh run view 9 --job 123",
		"gh run view 9 --web",
		"gh run view 9 --json jobs",
		"gh pr checks 12 --json name,link",
		"gh pr view 12 --json files",
		"gh pr view 12 --web",
		"gh issue view 3 --json assignees",
		"gh pr status",
		"gh api repos/o/r/actions/runs/9/logs",
		"gh api repos/o/r/actions/jobs/1/logs",
		"gh api repos/o/r/actions/runs/9/jobs",
		"gh api -X POST repos/o/r/actions/runs/9/rerun",
		"gh api 'repos/o/r/actions/runs?branch=x'",
		"gh api repos/o/r/check-runs/5/annotations",
		"gh api repos/o/r/pulls/1/files",
		"gh api repos/o/r/pulls/1/comments",
		"gh api -X POST repos/o/r/pulls/1/comments -f body=x",
		"gh api 'repos/o/r/pulls?state=open'",
		"gh api -X PATCH repos/o/r/issues/comments/7 -f body=x",
		"gh api -X DELETE repos/o/r/issues/1/labels/bug",
		"gh api 'search/issues?q=repo:o/r'",
		`gh api graphql -f query='mutation{enablePullRequestAutoMerge(input:{pullRequestId:"x"}){clientMutationId}}'`,
		`gh api graphql -f query='{repository(owner:"o",name:"r"){pullRequest(number:1){reviewThreads(first:10){nodes{isResolved}}}}}'`,
		"git push origin main",
		"echo gh pr view 1",
		"grep 'gh pr view' notes.md",
		"octoguard get pr 1",
		"ghq get x",
		// Text that mentions gh is not a gh command: single quotes and quoted
		// heredoc bodies never run, even with Markdown backticks inside.
		"octoguard issue create --title t --body 'run `gh pr view` instead; then gh pr merge'",
		"git commit -m \"$(cat <<'EOF'\nUse gh pr view and `gh run watch` here.\ngh pr merge 1\nEOF\n)\"",
		"cat > notes.md <<EOF\ngh pr checks 1 --watch\nEOF\necho done",
		"echo \"see gh pr view; and gh run watch\"",
	} {
		if deny, reason := Guard(cmd); deny {
			t.Errorf("Guard(%q) denied: %s", cmd, reason)
		}
	}
}
