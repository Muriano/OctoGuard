# octoguard quickstart

Use octoguard instead of `gh` for every GitHub read, wait or quota check.
It caches with ETags, shares one poller per resource and never polls in a hot loop.

A `<ref>` is `owner/repo#N`, a github.com URL, or a bare `N` resolved against
the `origin` remote of the current directory.

    octoguard get pr owner/repo#123          # read (pr|issue|reviews|comments|checks|runs)
    octoguard wait pr 123 --until checks-green  # block until a condition holds
    octoguard quota                           # remaining core/graphql/search quota
    octoguard pr comment 123 --body "..."     # spaced writes that invalidate the cache

Output is JSON with `"schema_version": 1`; add `--format text` for people.
Exit codes: 0 ok · 1 error · 2 usage · 3 wait pending (call again) · 4 merge rejected.

More: `octoguard guide waiting|reading|quota|writes|anti-patterns|stats`.
Machine-readable command schema: `octoguard agent-context`.
