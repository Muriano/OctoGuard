---
name: octoguard
description: Use for ANY read of, wait on or poll of GitHub (PRs, issues, reviews, comments, checks, workflow runs) and whenever GitHub rate limit or API quota comes up. Replaces gh polling loops, `gh pr checks --watch` and `gh api rate_limit` with octoguard, which caches with ETags and waits without burning quota.
---
<!-- managed by octoguard -->

# octoguard

Hard rule: never poll GitHub. No `sleep N && gh ...`, no `gh pr checks --watch`
or `gh run watch`, no `gh api rate_limit` or `gh api user` checks. Use octoguard
for every GitHub read, wait and quota check.

Key commands (`<ref>` is `owner/repo#N`, a github.com URL or a bare `N`):

    octoguard get pr <ref>                         # read: pr|issue|reviews|comments|checks|runs
    octoguard wait pr <ref> --until checks-green   # block until a condition holds
    octoguard quota                                # remaining quota, at no cost
    octoguard pr comment <ref> --body "..."        # spaced write that invalidates the cache

Output is JSON with `"schema_version": 1`. Exit code 3 from `wait` means
`pending`: run the same command again, without a sleep.

Before your first GitHub call, run `octoguard guide quickstart`.
