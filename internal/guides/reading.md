# Reading

    octoguard get pr|issue|reviews|comments|checks|runs <ref>

Reads go through the daemon (started on demand) and its cache:
- parts younger than `--max-age` (default 30s) are served with no request;
- older parts are revalidated with an ETag: a `304` costs no quota;
- identical reads in flight are joined into one request.

`--fresh` revalidates every part against GitHub, ignoring `--max-age`.
`meta` tells how it was served: `from_cache`, `not_modified`, `via` (rest or
graphql) and `fetched_at`.

`runs` takes a workflow run ref (`owner/repo#RUN_ID` or an actions/runs URL).
Read once and reuse the result; to watch for changes use `octoguard wait`.
