# Waiting

    octoguard wait pr|issue|run <ref> --until <condition> [--timeout 8m]

Conditions:
- pr: `checks-done`, `checks-green`, `review`, `comment`, `merged`, `closed`, `changed`
- issue: `comment`, `closed`, `labeled`, `changed`
- run: `done` (the default)

The JSON has `status`: `met`, `failed` (it can no longer be met, e.g. a red
check for `checks-green`) or `pending`, plus `changes` and the current `state`.

With no checks, `checks-done`/`checks-green` wait: CI may not have started.
Still none (and none required) 1 min after the last push means no CI: both
hold, and `pr merge` merges.

`pending` means `--timeout` expired first: the exit code is 3 and it is not an
error. Run the same command again. The daemon keeps polling for 2 minutes after
the last waiter leaves, so the next call resumes at no extra cost.

The default timeout of 8m stays under the 10-minute limit of agent shell tools.
Many waiters on one resource share a single poller.
Without the daemon, `wait` polls in-process no faster than every 60s.
