# Stats

    octoguard stats [--since 1h|24h|7d|30d] [--live] [--format json]

A panel for people: quota per bucket, requests per hour, rate limits avoided
and suffered, and the requests octoguard saved. It reads only local data (the
cache database and a running daemon): it never calls GitHub nor starts the
daemon. Pass `--format json` explicitly for scripts and agents.

How "saved" is counted:
- cache hit, or a `304` revalidation: 1 REST request avoided each;
- deduplication: N joined identical reads count as N-1 avoided;
- `wait`: the polls a fixed `sleep` loop would have made during the wait,
  minus the polls the daemon actually made. The loop interval (baseline) is
  30s by default and is shown in the output; set `OCTOGUARD_POLL_BASELINE`
  (e.g. `15s`) to change it.

`rest_saved` adds them all up as REST calls.
