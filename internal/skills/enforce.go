package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Claude Code only loads a skill when the model decides to, and an agent busy
// with gh rarely does. Enforce makes octoguard the default instead: a block in
// CLAUDE.md, which is always in context, and a PreToolUse hook that blocks the
// gh commands octoguard replaces (see Guard).

// HookCommand is the command the PreToolUse hook runs.
const HookCommand = "octoguard hook claude"

const (
	blockBegin = "<!-- octoguard:begin (managed by octoguard skills install --enforce) -->"
	blockEnd   = "<!-- octoguard:end -->"
)

// Block is the instruction added to CLAUDE.md.
const Block = blockBegin + `
## GitHub: use octoguard, not gh

For every GitHub read, wait, quota check and PR/issue write use ` + "`octoguard`" + `
(get, wait, quota, pr create|comment|edit|merge, issue create|comment). Never
poll GitHub and never use ` + "`gh`" + ` for these: a hook blocks it. ` + "`gh`" + ` stays for
what octoguard lacks (releases, repo settings, auth, listings). First time:
` + "`octoguard guide quickstart`" + `.
` + blockEnd + "\n"

// Actions reported by Enforce besides those of Install.
const ActionUnchanged = "unchanged"

// Enforce installs the CLAUDE.md block and the PreToolUse hook for Claude Code
// in the scope of o. It is idempotent.
func Enforce(o Options) ([]Result, error) {
	var base, mdPath string
	switch o.Scope {
	case ScopeUser:
		base = o.Home
		mdPath = filepath.Join(base, ".claude", "CLAUDE.md")
	case ScopeProject:
		base = o.Root
		mdPath = filepath.Join(base, "CLAUDE.md")
	default:
		return nil, fmt.Errorf("invalid scope %q: must be user or project", o.Scope)
	}
	if base == "" {
		return nil, fmt.Errorf("no base directory for scope %s", o.Scope)
	}
	mdAction, err := writeBlock(mdPath)
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	settings := filepath.Join(base, ".claude", "settings.json")
	hookAction, err := addHook(settings)
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	return []Result{
		{Agent: "claude", Scope: o.Scope, Path: mdPath, Action: mdAction},
		{Agent: "claude", Scope: o.Scope, Path: settings, Action: hookAction},
	}, nil
}

// writeBlock adds Block to path, or replaces the one already there.
func writeBlock(path string) (string, error) {
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	text, action := string(current), ActionInstalled
	if i := strings.Index(text, blockBegin); i >= 0 {
		j := strings.Index(text[i:], blockEnd)
		if j < 0 {
			return "", fmt.Errorf("%s: octoguard block has no end marker %q", path, blockEnd)
		}
		end := i + j + len(blockEnd)
		if end < len(text) && text[end] == '\n' {
			end++
		}
		if text[i:end] == Block {
			return ActionUnchanged, nil
		}
		text, action = text[:i]+Block+text[end:], ActionUpdated
	} else {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" {
			text += "\n"
		}
		text += Block
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return action, os.WriteFile(path, []byte(text), 0o644)
}

// addHook registers HookCommand as a PreToolUse hook on Bash in a Claude Code
// settings file, keeping everything else. The previous file is kept as
// <path>.octoguard.bak.
func addHook(path string) (string, error) {
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	settings := map[string]any{}
	if len(bytes.TrimSpace(current)) > 0 {
		if err := json.Unmarshal(current, &settings); err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		if settings["hooks"] != nil {
			return "", fmt.Errorf("%s: \"hooks\" is not an object", path)
		}
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	pre, ok := hooks["PreToolUse"].([]any)
	if !ok && hooks["PreToolUse"] != nil {
		return "", fmt.Errorf("%s: \"hooks.PreToolUse\" is not a list", path)
	}
	for _, entry := range pre {
		e, _ := entry.(map[string]any)
		list, _ := e["hooks"].([]any)
		for _, h := range list {
			if hm, _ := h.(map[string]any); hm["command"] == HookCommand {
				return ActionUnchanged, nil
			}
		}
	}
	hooks["PreToolUse"] = append(pre, map[string]any{
		"matcher": "Bash",
		"hooks":   []any{map[string]any{"type": "command", "command": HookCommand}},
	})
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if len(current) > 0 {
		if err := os.WriteFile(path+".octoguard.bak", current, 0o600); err != nil {
			return "", err
		}
	}
	return ActionInstalled, os.WriteFile(path, buf.Bytes(), 0o644)
}

// HookResponse is the PreToolUse answer for a Claude Code hook input (the
// JSON Claude Code writes to the hook's stdin). It returns nil to allow.
func HookResponse(input []byte) ([]byte, error) {
	var in struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("hook: decode input: %w", err)
	}
	if in.ToolName != "Bash" {
		return nil, nil
	}
	deny, reason := Guard(in.ToolInput.Command)
	if !deny {
		return nil, nil
	}
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
}
