#!/usr/bin/env python3
"""Reconnaissance of GitHub usage by AI agents in local transcripts.

Counts `gh` invocations, `sleep`+`gh` polling patterns, `--watch` usage and
GitHub rate limit errors in Claude Code and Codex transcripts (*.jsonl)
modified in the last N days. Read-only, stdlib only, no network.

Usage: python3 scripts/recon/gh_recon.py [--days 14] [--json]
"""

import argparse
import collections
import json
import os
import re
import shlex
import sys
import time

GH_HINTS = ('"gh ', "gh pr", "gh api", "mcp__github", "gh run", "gh issue")
GH_RE = re.compile(r"(?:^|[;&|(\s])gh\s+([a-z-]+)(?:\s+([a-z-]+))?")
SLEEP_RE = re.compile(r"\bsleep\b")
# Only applied to commands that already contain a `gh` call (see scan).
WATCH_RE = re.compile(r"--watch\b|\bgh\s+run\s+watch\b")
PLACEHOLDER_RE = re.compile(r"^\{[a-z_]+\}$")
GH_API_VALUE_FLAGS = {
    "-H", "--header", "-f", "--raw-field", "-F", "--field", "-X", "--method",
    "-q", "--jq", "-t", "--template", "--input", "--hostname", "--cache", "-p", "--preview",
}  # fmt: skip
# Path segments printed as-is; anything else (owner, repo, user, branch, SHA...) is masked.
API_KEYWORDS = {
    "actions", "artifacts", "assignees", "attempts", "branches", "check-runs", "check-suites",
    "collaborators", "comments", "commits", "compare", "contents", "deployments", "environments",
    "events", "files", "git", "graphql", "heads", "hooks", "issues", "items", "jobs", "labels",
    "logs", "members", "merge", "merges", "milestones", "notifications", "orgs", "projects",
    "protection", "pulls", "rate_limit", "reactions", "refs", "releases", "repos",
    "requested_reviewers", "required_status_checks", "rerun", "reviews", "rulesets", "runs",
    "search", "status", "statuses", "tags", "teams", "timeline", "user", "users", "variables",
    "workflows",
}  # fmt: skip


def is_rate_limit_line(line):
    low = line.lower()
    return "rate limit" in low and (
        "API rate limit exceeded" in line or "secondary rate limit" in low
    )


def extract_commands(obj, mcp):
    """Collect shell commands from Claude tool_use and Codex function_call entries."""
    texts = []

    def walk(o):
        if isinstance(o, dict):
            if o.get("type") == "tool_use":
                name = o.get("name", "")
                if "github" in name.lower():
                    mcp[name] += 1
                cmd = (o.get("input") or {}).get("command")
                if isinstance(cmd, str):
                    texts.append(cmd)
            if o.get("type") == "function_call" and isinstance(o.get("arguments"), str):
                try:
                    args = json.loads(o["arguments"])
                    cmd = args.get("cmd") or args.get("command")
                    if isinstance(cmd, list):
                        cmd = " ".join(map(str, cmd))
                    if isinstance(cmd, str):
                        texts.append(cmd)
                except (ValueError, AttributeError):
                    pass
            for v in o.values():
                walk(v)
        elif isinstance(o, list):
            for v in o:
                walk(v)

    walk(obj)
    return texts


def api_endpoint(text, start):
    """Return the endpoint argument of a `gh api` call, skipping flags and their values."""
    rest = text[start : start + 4000]
    try:
        toks = shlex.split(rest)
    except ValueError:
        toks = rest.split()
    i = 2  # skip "gh", "api"
    while i < len(toks):
        tok = toks[i]
        if tok in GH_API_VALUE_FLAGS:
            i += 2
        elif tok.startswith("-"):
            i += 1
        else:
            return tok
    return ""


def anonymize_endpoint(endpoint):
    """Keep only known REST keywords; owners, repos, users, branches, SHAs... become X."""
    path = re.sub(r"^https?://[^/]+(?:/api/v3)?", "", endpoint.split("?")[0])
    out = []
    for seg in path.split("/"):
        if not seg:
            continue
        if seg.isdigit():
            out.append("N")
        elif seg in API_KEYWORDS or PLACEHOLDER_RE.match(seg):
            out.append(seg)
        else:
            out.append("X")
    return "/".join(out)[:60]


def command_key(text, match):
    sub, sub2 = match.group(1), match.group(2) or ""
    if sub != "api":
        return f"gh {sub} {sub2}".strip()
    start = text.find("gh", match.start())
    return ("gh api " + anonymize_endpoint(api_endpoint(text, start))).strip()


def scan(roots, days):
    cutoff = time.time() - days * 86400
    totals = {src: collections.Counter() for src in roots}
    cmds = collections.Counter()
    mcp = collections.Counter()
    for src, root in roots.items():
        for dirpath, _, files in os.walk(root):
            for name in files:
                if not name.endswith(".jsonl"):
                    continue
                path = os.path.join(dirpath, name)
                try:
                    if os.path.getmtime(path) < cutoff:
                        continue
                    fh = open(path, errors="ignore")
                except OSError:
                    continue
                t = totals[src]
                with fh:
                    for line in fh:
                        if is_rate_limit_line(line):
                            t["rate_limit_errors"] += 1
                        if not any(h in line for h in GH_HINTS):
                            continue
                        try:
                            entry = json.loads(line)
                        except ValueError:
                            continue
                        for text in extract_commands(entry, mcp):
                            n = 0
                            for m in GH_RE.finditer(text):
                                n += 1
                                cmds[(src, command_key(text, m))] += 1
                            if not n:
                                continue
                            t["gh_invocations"] += n
                            if SLEEP_RE.search(text):
                                t["sleep_gh"] += 1
                            if WATCH_RE.search(text):
                                t["watch"] += 1
    return totals, cmds, mcp


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument(
        "--days",
        type=int,
        default=14,
        help="window in days (by file mtime, default 14)",
    )
    ap.add_argument("--claude-dir", default=os.path.expanduser("~/.claude/projects"))
    ap.add_argument("--codex-dir", default=os.path.expanduser("~/.codex/sessions"))
    ap.add_argument(
        "--top", type=int, default=45, help="number of top commands to list"
    )
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    args = ap.parse_args()

    roots = {"claude": args.claude_dir, "codex": args.codex_dir}
    totals, cmds, mcp = scan(roots, args.days)
    fields = ("gh_invocations", "sleep_gh", "watch", "rate_limit_errors")

    if args.json:
        out = {
            "days": args.days,
            "totals": {src: {f: c[f] for f in fields} for src, c in totals.items()},
            "top_commands": [
                {"source": s, "command": k, "count": v}
                for (s, k), v in cmds.most_common(args.top)
            ],
            "mcp_github_tools": dict(mcp.most_common(15)),
        }
        json.dump(out, sys.stdout, indent=2)
        print()
        return

    print(f"Window: last {args.days} days")
    print(f"{'source':8s} " + " ".join(f"{f:>18s}" for f in fields))
    for src, c in totals.items():
        print(f"{src:8s} " + " ".join(f"{c[f]:18d}" for f in fields))
    print(f"\nTop {args.top} commands:")
    for (s, k), v in cmds.most_common(args.top):
        print(f"{v:6d} {s:6s} {k}")
    print("\nMCP github tools:")
    for k, v in mcp.most_common(15):
        print(f"{v:6d} {k}")


if __name__ == "__main__":
    main()
