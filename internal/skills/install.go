// Package skills embeds the octoguard bootstrap skill (SKILL.md, in the Agent
// Skills format) and installs it where each coding agent looks for skills.
package skills

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Skill is the embedded SKILL.md. skills/octoguard/SKILL.md at the repo root
// is a published copy of it, kept identical by a test.
//
//go:embed SKILL.md
var Skill []byte

// Marker identifies a SKILL.md written by octoguard. It sits right after the
// frontmatter, which must start the file. Files without it are never touched.
const Marker = "<!-- managed by octoguard -->"

// Scopes.
const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

// Actions reported per location.
const (
	ActionInstalled        = "installed"
	ActionUpdated          = "updated"
	ActionSkippedUnmanaged = "skipped_unmanaged"
	ActionNotInstalled     = "not_installed"
)

type location struct{ user, project string }

// locations holds the skill directory of each agent, relative to the home
// directory (user scope) or to the repository root (project scope). Checked
// against each agent's documentation on 2026-11-12:
//   - claude:  https://code.claude.com/docs/en/skills
//   - codex:   https://learn.chatgpt.com/docs/build-skills ($HOME/.agents/skills, repo .agents/skills)
//   - gemini:  https://geminicli.com/docs/cli/skills/
//   - copilot: https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/create-skills
var locations = map[string]location{
	"claude":  {user: ".claude/skills/octoguard", project: ".claude/skills/octoguard"},
	"codex":   {user: ".agents/skills/octoguard", project: ".agents/skills/octoguard"},
	"gemini":  {user: ".gemini/skills/octoguard", project: ".gemini/skills/octoguard"},
	"copilot": {user: ".copilot/skills/octoguard", project: ".github/skills/octoguard"},
}

// Agents lists the supported agents, in output order.
var Agents = []string{"claude", "codex", "gemini", "copilot"}

// Options selects where Install and Update act. Home is the base of the user
// scope and Root the base of the project scope.
type Options struct {
	Agents []string
	Scope  string
	Home   string
	Root   string
}

// Result is the outcome for one agent location.
type Result struct {
	Agent  string `json:"agent"`
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Action string `json:"action"`
}

// Install writes the skill for every selected agent. An existing file is
// replaced only if it carries Marker.
func Install(o Options) ([]Result, error) { return run(o, true) }

// Update rewrites the skill only where octoguard installed it before: absent
// files stay absent and files without Marker are left alone.
func Update(o Options) ([]Result, error) { return run(o, false) }

func run(o Options, create bool) ([]Result, error) {
	var base string
	switch o.Scope {
	case ScopeUser:
		base = o.Home
	case ScopeProject:
		base = o.Root
	default:
		return nil, fmt.Errorf("invalid scope %q: must be user or project", o.Scope)
	}
	if base == "" {
		return nil, fmt.Errorf("no base directory for scope %s", o.Scope)
	}
	results := make([]Result, 0, len(o.Agents))
	for _, agent := range o.Agents {
		loc, ok := locations[agent]
		if !ok {
			return results, fmt.Errorf("unknown agent %q", agent)
		}
		dir := loc.user
		if o.Scope == ScopeProject {
			dir = loc.project
		}
		path := filepath.Join(base, dir, "SKILL.md")
		action, err := write(path, create)
		if err != nil {
			return results, fmt.Errorf("%s: %w", agent, err)
		}
		results = append(results, Result{Agent: agent, Scope: o.Scope, Path: path, Action: action})
	}
	return results, nil
}

func write(path string, create bool) (string, error) {
	action := ActionUpdated
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return ActionNotInstalled, nil
		}
		action = ActionInstalled
	case err != nil:
		return "", err
	case !info.Mode().IsRegular():
		// A symlink or anything else that is not a plain file is not ours.
		return ActionSkippedUnmanaged, nil
	default:
		current, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if !bytes.Contains(current, []byte(Marker)) {
			return ActionSkippedUnmanaged, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, Skill, 0o644); err != nil {
		return "", err
	}
	return action, nil
}

// ProjectRoot returns the nearest directory from dir upwards that contains
// .git, or dir itself when there is none.
func ProjectRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}
