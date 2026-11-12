# Anti-patterns

Each of these burns quota or hits the rate limit. Use the replacement.

- Do not: `sleep N && gh ...` (or any polling loop around `gh`).
  Instead: `octoguard wait <kind> <ref> --until <condition>`.

- Do not: `gh pr checks --watch` (or `gh run watch`).
  Instead: `octoguard wait pr <ref> --until checks-green` (or `checks-done`),
  and `octoguard wait run <ref>` for a workflow run.

- Do not: `gh api rate_limit` to see how much quota is left.
  Instead: `octoguard quota`. You rarely need it: octoguard paces itself.

- Do not: `gh api user` as a login check.
  Instead: `octoguard quota`. It resolves the token and fails clearly if
  there is none, without spending core quota.

- Do not: re-read the same PR or issue in a loop to see if it changed.
  Instead: `octoguard wait pr <ref> --until changed`.

On exit code 3 (`pending`), run the same `wait` again; do not add a sleep.
