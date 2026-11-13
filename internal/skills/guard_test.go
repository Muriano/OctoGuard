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
		{"gh api 'repos/o/r/actions/runs?branch=x'", "octoguard get runs"},
		{`gh api graphql -f query='{repository(owner:"o",name:"r"){pullRequest(number:1){state}}}'`, "octoguard get"},
		// Inside loops, pipes, subshells and after env assignments.
		{"until gh pr checks 1; do sleep 30; done", "octoguard wait pr"},
		{"sleep 60 && gh pr view 1", "octoguard get pr"},
		{"x=$(gh api repos/o/r/pulls/1 --jq .state)", "octoguard get"},
		{"GH_TOKEN=t gh pr view 1", "octoguard get pr"},
		{"cd repo; /opt/homebrew/bin/gh pr merge 1", "octoguard pr merge"},
		{"echo ok\ngh issue view 2", "octoguard get issue"},
		{"(cd repo && gh pr view 1)", "octoguard get pr"},
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
		"gh api repos/o/r/contents/README.md",
		"gh api graphql -f query='{viewer{login}}'",
		"git push origin main",
		"echo gh pr view 1",
		"grep 'gh pr view' notes.md",
		"octoguard get pr 1",
		"ghq get x",
	} {
		if deny, reason := Guard(cmd); deny {
			t.Errorf("Guard(%q) denied: %s", cmd, reason)
		}
	}
}
