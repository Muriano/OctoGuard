# Writes

    octoguard pr create --title T --head BRANCH --base BRANCH [--body TEXT]
    octoguard pr comment <ref> --body TEXT     (or --body-file FILE, "-" = stdin)
    octoguard pr edit <ref> [--title T] [--body TEXT] [--base B] [--add-label L]
    octoguard pr merge <ref> [--merge|--squash|--rebase]
    octoguard issue create --title T [--body TEXT] [--label L]
    octoguard issue comment <ref> --body TEXT
    octoguard project item-add --project PVT_... --content-id NODE_ID
    octoguard project item-edit --project PVT_... --id PVTI_... --field PVTF_... --value V

Writes are spaced (1/s and 60/min per token) to stay clear of GitHub's
secondary limits. They are queued, never refused, and invalidate the cache of
what they touch, so the next `get` sees the change.

`pr merge` reads the checks fresh and merges only if `checks-green` holds.
Otherwise it exits 4 with `"status":"rejected"`, a `reason` and a `hint`:
`octoguard wait pr <ref> --until checks-green`, then merge again.

`create` uses `--repo owner/repo` or the `origin` remote of the current directory.
