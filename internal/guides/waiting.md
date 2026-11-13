# Waiting

    octoguard wait pr|issue|run <ref> --until <condition> [--timeout 8m]

Conditions:
- pr: `checks-done`, `checks-green`, `review`, `comment`, `merged`, `closed`, `changed`
- issue: `comment`, `closed`, `labeled`, `changed`
- run: `done` (the default)

The JSON has `status`: `met`, `failed` (it can no longer be met, e.g. a red
check for `checks-green`) or `pending`, plus `changes` and the current `state`.

`changed` also fires when GitHub's `mergeable_state` changes (e.g. `blocked` ->
`clean`), whatever the repo's rules: read `state` and decide. `checks-*` need
at least one check, so on a repo without CI wait for `changed` instead.

`pending` means `--timeout` expired first: the exit code is 3 and it is not an
error. Run the same command again. The daemon keeps polling for 2 minutes after
the last waiter leaves, so the next call resumes at no extra cost.

The default timeout of 8m stays under the 10-minute limit of agent shell tools.
Many waiters on one resource share a single poller.
Without the daemon, `wait` polls in-process no faster than every 60s.
