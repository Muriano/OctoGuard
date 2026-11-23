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

// An agent only loads a skill when the model decides to, and an agent busy
// with gh rarely does. Enforce makes octoguard the default instead: a block in
// the instructions file the agent always keeps in context, and a pre-tool hook
// that blocks the gh commands octoguard replaces (see Guard).

// HookCommand is the command the pre-tool hook of agent runs.
func HookCommand(agent string) string { return "octoguard hook " + agent }

const (
	blockBegin = "<!-- octoguard:begin (managed by octoguard skills install --enforce) -->"
	blockEnd   = "<!-- octoguard:end -->"
)

// Block is the instruction added to the instructions file of each agent.
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

// enforcement says where Enforce writes for one agent, relative to the home
// directory (user scope) or to the repository root (project scope).
type enforcement struct {
	userMD, projectMD     string // instructions file always in context
	userHook, projectHook string // hook configuration file
	// install writes the hook into a configuration file.
	install func(path, command string) (string, error)
}

// enforcements were checked against each agent's documentation and the
// installed versions on 2026-11-23 (Claude Code, Codex CLI 0.171, Gemini CLI
// 0.64, Copilot CLI 1.0.114):
//   - claude:  https://code.claude.com/docs/en/hooks
//   - codex:   https://developers.openai.com/codex/guides/agents-md, https://learn.chatgpt.com/docs/hooks
//   - gemini:  https://geminicli.com/docs/cli/gemini-md/, https://geminicli.com/docs/hooks/
//   - copilot: https://docs.github.com/en/copilot/reference/hooks-configuration
var enforcements = map[string]enforcement{
	"claude": {
		userMD: ".claude/CLAUDE.md", projectMD: "CLAUDE.md",
		userHook: ".claude/settings.json", projectHook: ".claude/settings.json",
		install: func(path, command string) (string, error) {
			return addHook(path, "PreToolUse", "Bash", map[string]any{"type": "command", "command": command})
		},
	},
	"codex": {
		userMD: ".codex/AGENTS.md", projectMD: "AGENTS.md",
		userHook: ".codex/hooks.json", projectHook: ".codex/hooks.json",
		install: func(path, command string) (string, error) {
			return addHook(path, "PreToolUse", "Bash", map[string]any{"type": "command", "command": command})
		},
	},
	"gemini": {
		userMD: ".gemini/GEMINI.md", projectMD: "GEMINI.md",
		userHook: ".gemini/settings.json", projectHook: ".gemini/settings.json",
		install: func(path, command string) (string, error) {
			return addHook(path, "BeforeTool", "run_shell_command",
				map[string]any{"name": "octoguard", "type": "command", "command": command})
		},
	},
	"copilot": {
		userMD: ".copilot/copilot-instructions.md", projectMD: ".github/copilot-instructions.md",
		userHook: ".copilot/hooks/octoguard.json", projectHook: ".github/hooks/octoguard.json",
		install: writeCopilotHook,
	},
}

// Enforce installs the instructions block and the pre-tool hook for every
// agent in o, in the scope of o. It is idempotent.
func Enforce(o Options) ([]Result, error) {
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
	var results []Result
	for _, agent := range o.Agents {
		e, ok := enforcements[agent]
		if !ok {
			return results, fmt.Errorf("unknown agent %q", agent)
		}
		md, hook := e.userMD, e.userHook
		if o.Scope == ScopeProject {
			md, hook = e.projectMD, e.projectHook
		}
		md, hook = filepath.Join(base, md), filepath.Join(base, hook)
		mdAction, err := writeBlock(md)
		if err != nil {
			return results, fmt.Errorf("%s: %w", agent, err)
		}
		hookAction, err := e.install(hook, HookCommand(agent))
		if err != nil {
			return results, fmt.Errorf("%s: %w", agent, err)
		}
		results = append(results,
			Result{Agent: agent, Scope: o.Scope, Path: md, Action: mdAction},
			Result{Agent: agent, Scope: o.Scope, Path: hook, Action: hookAction})
	}
	return results, nil
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

// addHook registers hook (a command hook) for event with matcher in a
// settings file shaped like Claude Code's ({"hooks": {event: [{matcher, hooks}]}}),
// keeping everything else. The previous file is kept as <path>.octoguard.bak.
func addHook(path, event, matcher string, hook map[string]any) (string, error) {
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
	pre, ok := hooks[event].([]any)
	if !ok && hooks[event] != nil {
		return "", fmt.Errorf("%s: \"hooks.%s\" is not a list", path, event)
	}
	for _, entry := range pre {
		e, _ := entry.(map[string]any)
		list, _ := e["hooks"].([]any)
		for _, h := range list {
			if hm, _ := h.(map[string]any); hm["command"] == hook["command"] {
				return ActionUnchanged, nil
			}
		}
	}
	hooks[event] = append(pre, map[string]any{"matcher": matcher, "hooks": []any{hook}})
	return writeJSON(path, current, settings)
}

// writeCopilotHook writes the Copilot CLI hook file, which octoguard owns
// whole. A different previous file is kept as <path>.octoguard.bak; one that
// is not a plain JSON file (a symlink, say) is left alone.
func writeCopilotHook(path, command string) (string, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s: not a regular file", path)
	}
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if len(current) > 0 && !json.Valid(current) {
		return "", fmt.Errorf("%s: not JSON", path)
	}
	file := map[string]any{"version": 1, "hooks": map[string]any{"preToolUse": []any{
		map[string]any{"type": "command", "bash": command, "timeoutSec": 10},
	}}}
	want, err := encodeJSON(file)
	if err != nil {
		return "", err
	}
	if bytes.Equal(current, want) {
		return ActionUnchanged, nil
	}
	action, err := writeJSON(path, current, file)
	if err == nil && len(current) > 0 {
		action = ActionUpdated
	}
	return action, err
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return buf.Bytes(), err
}

// writeJSON writes v to path, keeping the previous content (if any) as
// <path>.octoguard.bak.
func writeJSON(path string, previous []byte, v any) (string, error) {
	data, err := encodeJSON(v)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if len(previous) > 0 {
		if err := os.WriteFile(path+".octoguard.bak", previous, 0o600); err != nil {
			return "", err
		}
	}
	return ActionInstalled, os.WriteFile(path, data, 0o644)
}

// HookResponse is the pre-tool hook answer of agent for its hook input (the
// JSON the agent writes to the hook's stdin). It returns nil to allow.
func HookResponse(agent string, input []byte) ([]byte, error) {
	var in struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		// Copilot CLI (preToolUse): toolArgs is a JSON string.
		CopilotName string          `json:"toolName"`
		CopilotArgs json.RawMessage `json:"toolArgs"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		if agent == "copilot" {
			return nil, nil // Copilot denies every tool call when a hook fails
		}
		return nil, fmt.Errorf("hook: decode input: %w", err)
	}
	var shell bool
	args := in.ToolInput
	switch agent {
	case "claude", "codex":
		shell = in.ToolName == "Bash"
	case "gemini":
		shell = in.ToolName == "run_shell_command"
	case "copilot":
		if in.CopilotName != "" {
			shell, args = strings.EqualFold(in.CopilotName, "bash"), in.CopilotArgs
		} else {
			shell = strings.EqualFold(in.ToolName, "bash")
		}
	default:
		return nil, fmt.Errorf("hook: unknown agent %q", agent)
	}
	if !shell {
		return nil, nil
	}
	deny, reason := Guard(shellCommand(args))
	if !deny {
		return nil, nil
	}
	switch agent {
	case "gemini":
		return json.Marshal(map[string]any{"decision": "deny", "reason": reason})
	case "copilot":
		return json.Marshal(map[string]any{"permissionDecision": "deny", "permissionDecisionReason": reason})
	}
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
}

// shellCommand returns the command of shell tool arguments given as a JSON
// object or as a string holding one.
func shellCommand(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	var args struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(raw, &args)
	return args.Command
}
