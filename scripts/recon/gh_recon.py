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
import sys
import time

GH_HINTS = ('"gh ', "gh pr", "gh api", "mcp__github", "gh run", "gh issue")
GH_RE = re.compile(r"(?:^|[;&|(\s])gh\s+([a-z-]+)(?:\s+([a-z-]+))?")
GH_API_PATH_RE = re.compile(r"gh api\s+(?:-[^\s]+\s+)*([^\s]+)")
SLEEP_GH_RE = re.compile(r"\bsleep\b")
WATCH_RE = re.compile(r"\bgh\b.*(--watch\b|\brun\s+watch\b)", re.S)


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


def command_key(text, match):
    sub, sub2 = match.group(1), match.group(2) or ""
    if sub != "api":
        return f"gh {sub} {sub2}".strip()
    path = GH_API_PATH_RE.search(text[match.start() :])
    key = "gh api " + (path.group(1) if path else "").split("?")[0][:60]
    key = re.sub(r"/\d+", "/N", key)
    return re.sub(r"repos/[^/]+/[^/]+", "repos/O/R", key)


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
                            if SLEEP_GH_RE.search(text):
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
