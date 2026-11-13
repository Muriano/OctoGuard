# Installing the octoguard skill (instructions for coding agents)

You are reading this because someone asked you to "install the skill
https://github.com/Muriano/OctoGuard" (or similar). The skill is a `SKILL.md`
that needs the `octoguard` binary, so install both. Do not copy
`skills/octoguard/SKILL.md` by hand: the binary writes it in the right place for
each agent and keeps it updated.

## 1. Pick your agent and scope

- `--agent`: the agent you are: `claude` (Claude Code), `codex`, `gemini`
  (Gemini CLI) or `copilot` (GitHub Copilot CLI). Use `all` if the user asks
  for every agent or you are not sure.
- `--scope`: `user` (default, for every project of this user) or `project` (only
  the current repository, written under its root so it can be committed). Use
  `project` only if the user asks for it.

## 2. Install

Run, replacing the values from step 1:

```sh
curl -fsSL https://raw.githubusercontent.com/Muriano/OctoGuard/main/install.sh | sh -s -- --agent claude --scope user
```

The script ([install.sh](install.sh)):

1. Keeps `octoguard` if it is already in `PATH`.
2. Otherwise installs it with Homebrew (`brew install muriano/tap/octoguard`), or,
   without Homebrew, downloads the latest release for this OS and architecture
   (macOS or Linux, amd64 or arm64), checks it against `checksums.txt` and puts it
   in `$OCTOGUARD_BIN_DIR` (default `~/.local/bin`).
3. Runs `octoguard skills install --agent <agent> --scope <scope>`.

If you cannot pipe a script into `sh`, do the same steps by hand:

```sh
brew install muriano/tap/octoguard          # or: go install github.com/Muriano/OctoGuard/cmd/octoguard@latest
octoguard skills install --agent claude --scope user
```

## 3. Check and report

- The last lines print `<agent>  installed|updated  <path to SKILL.md>`.
  `skipped_unmanaged` means a `SKILL.md` not written by octoguard already exists
  there; it was left untouched, so tell the user.
- If the script says the bin directory is not in `PATH`, tell the user to add it.
- `octoguard --version` must work.
- Tell the user the skill loads in new sessions: they may need to restart the
  agent.

To update later: `brew upgrade octoguard` (or run the script again) and
`octoguard skills update`.
