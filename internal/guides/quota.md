# Quota

    octoguard quota

Prints the `core`, `graphql` and `search` buckets: `remaining`, `limit`,
`reset` and `known`. Buckets the daemon has already seen in response headers
are answered locally; the rest come from one `/rate_limit` call, which spends
no quota (`meta.probed` tells whether it was made).

It also resolves the token (`GH_TOKEN`, then `GITHUB_TOKEN`, then
`gh auth token`) and fails with a clear error when there is none, so it doubles
as the login check.

octoguard paces itself from the rate-limit headers: it slows down before a
bucket runs out and honours `Retry-After` and secondary limits. You do not need
to check the quota before each call.
