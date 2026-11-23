# Writes

    octoguard pr create --title T --head B --base B [--body T] [--label L] [--reviewer R] [--assignee A]
    octoguard pr comment <ref> --body TEXT     (or --body-file FILE, "-" = stdin)
    octoguard pr edit <ref> [--title T] [--body T] [--base B] [--add-label L] [--add-reviewer R] [--add-assignee A]
    octoguard pr merge <ref> [--merge|--squash|--rebase]
    octoguard issue create --title T [--body TEXT] [--label L] [--assignee A]
    octoguard issue comment <ref> --body TEXT
    octoguard project item-add --project OWNER/NUMBER --pr <ref>   (or --issue <ref>)
    octoguard project item-add --project PVT_... --content-id NODE_ID
    octoguard project item-edit --project PVT_... --id PVTI_... --field PVTF_... --value V

Writes are spaced (1/s and 60/min per token), queued, never refused, and
invalidate the cache of what they touch, so the next `get` sees the change.

`pr merge` applies no criteria: you decide when, GitHub decides if it can. If
GitHub refuses it exits 4 with `"status":"rejected"` and its `reason`;
`octoguard wait pr <ref> --until changed`, then decide again.

`create` uses `--repo owner/repo` or the `origin` remote of the current directory.
Reviewers are logins or `org/team`; `@me` is you. Assignees GitHub drops
silently (no access) are reported as an error. `item-add --project OWNER/NUMBER`
writes by REST (core quota), so it works with GraphQL spent; `PVT_` uses GraphQL.
The hook lets through `gh pr|issue` writes with flags octoguard lacks (`--auto`).
