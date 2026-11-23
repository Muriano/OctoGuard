package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnforce_UserScopeKeepsExistingContent(t *testing.T) {
	home := t.TempDir()
	md := filepath.Join(home, ".claude", "CLAUDE.md")
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(md), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(md, []byte("# Mine\n\nKeep me."), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := `{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"a && b < c"}]}]}}`
	if err := os.WriteFile(settings, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Enforce(Options{Agents: []string{"claude"}, Scope: ScopeUser, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Action != ActionInstalled || res[1].Action != ActionInstalled {
		t.Fatalf("results = %+v", res)
	}
	got, _ := os.ReadFile(md)
	if !strings.HasPrefix(string(got), "# Mine\n\nKeep me.\n\n"+blockBegin) || !strings.HasSuffix(string(got), blockEnd+"\n") {
		t.Fatalf("CLAUDE.md = %q", got)
	}
	raw, _ := os.ReadFile(settings)
	if strings.Contains(string(raw), "\\u0026") || !strings.Contains(string(raw), "a && b < c") {
		t.Fatalf("settings.json escaped & : %s", raw)
	}
	var s struct {
		Model string `json:"model"`
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	pre := s.Hooks.PreToolUse
	if s.Model != "opus" || len(pre) != 2 || pre[0].Hooks[0].Command != "a && b < c" ||
		pre[1].Matcher != "Bash" || pre[1].Hooks[0].Command != HookCommand("claude") {
		t.Fatalf("settings = %s", raw)
	}
	if bak, _ := os.ReadFile(settings + ".octoguard.bak"); string(bak) != orig {
		t.Fatalf("backup = %q", bak)
	}

	// Idempotent.
	res, err = Enforce(Options{Agents: []string{"claude"}, Scope: ScopeUser, Home: home})
	if err != nil || res[0].Action != ActionUnchanged || res[1].Action != ActionUnchanged {
		t.Fatalf("second run = %+v, %v", res, err)
	}
	if again, _ := os.ReadFile(md); string(again) != string(got) {
		t.Fatalf("CLAUDE.md changed on second run: %q", again)
	}
}

func TestEnforce_UpdatesOldBlock(t *testing.T) {
	root := t.TempDir()
	md := filepath.Join(root, "CLAUDE.md")
	old := "before\n" + blockBegin + "\nold text\n" + blockEnd + "\nafter\n"
	if err := os.WriteFile(md, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Enforce(Options{Agents: []string{"claude"}, Scope: ScopeProject, Root: root})
	if err != nil || res[0].Action != ActionUpdated {
		t.Fatalf("results = %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(md); string(got) != "before\n"+Block+"after\n" {
		t.Fatalf("CLAUDE.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "settings.json")); err != nil {
		t.Fatalf("project settings not written: %v", err)
	}
}

func TestHookResponse(t *testing.T) {
	out, err := HookResponse("claude", []byte(`{"tool_name":"Bash","tool_input":{"command":"gh pr checks 3 --watch"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	h := m["hookSpecificOutput"]
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" ||
		!strings.Contains(h["permissionDecisionReason"], "octoguard wait pr") {
		t.Fatalf("response = %s", out)
	}
	for _, in := range []string{
		`{"tool_name":"Bash","tool_input":{"command":"gh release create v1"}}`,
		`{"tool_name":"Read","tool_input":{"file_path":"x"}}`,
	} {
		if out, err := HookResponse("claude", []byte(in)); err != nil || out != nil {
			t.Fatalf("HookResponse(%s) = %s, %v; want allow", in, out, err)
		}
	}
}

func TestEnforce_Paths(t *testing.T) {
	for _, tc := range []struct {
		agent, scope, md, hook string
	}{
		{"codex", ScopeUser, ".codex/AGENTS.md", ".codex/hooks.json"},
		{"codex", ScopeProject, "AGENTS.md", ".codex/hooks.json"},
		{"gemini", ScopeUser, ".gemini/GEMINI.md", ".gemini/settings.json"},
		{"gemini", ScopeProject, "GEMINI.md", ".gemini/settings.json"},
		{"copilot", ScopeUser, ".copilot/copilot-instructions.md", ".copilot/hooks/octoguard.json"},
		{"copilot", ScopeProject, ".github/copilot-instructions.md", ".github/hooks/octoguard.json"},
	} {
		base := t.TempDir()
		res, err := Enforce(Options{Agents: []string{tc.agent}, Scope: tc.scope, Home: base, Root: base})
		if err != nil {
			t.Fatalf("%s %s: %v", tc.agent, tc.scope, err)
		}
		want := []Result{
			{Agent: tc.agent, Scope: tc.scope, Path: filepath.Join(base, tc.md), Action: ActionInstalled},
			{Agent: tc.agent, Scope: tc.scope, Path: filepath.Join(base, tc.hook), Action: ActionInstalled},
		}
		if len(res) != 2 || res[0] != want[0] || res[1] != want[1] {
			t.Fatalf("%s %s: results = %+v, want %+v", tc.agent, tc.scope, res, want)
		}
		if md, _ := os.ReadFile(want[0].Path); string(md) != Block {
			t.Fatalf("%s %s: md = %q", tc.agent, tc.scope, md)
		}
		if hook, _ := os.ReadFile(want[1].Path); !strings.Contains(string(hook), `"octoguard hook `+tc.agent+`"`) {
			t.Fatalf("%s %s: hook file = %s", tc.agent, tc.scope, hook)
		}
		res, err = Enforce(Options{Agents: []string{tc.agent}, Scope: tc.scope, Home: base, Root: base})
		if err != nil || res[0].Action != ActionUnchanged || res[1].Action != ActionUnchanged {
			t.Fatalf("%s %s: second run = %+v, %v", tc.agent, tc.scope, res, err)
		}
	}
}

func TestEnforce_GeminiKeepsSettings(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := `{"security":{"auth":"x"},"hooks":{"BeforeTool":[{"hooks":[{"type":"command","command":"other"}]}],"AfterTool":[]}}`
	if err := os.WriteFile(settings, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Enforce(Options{Agents: []string{"gemini"}, Scope: ScopeUser, Home: home}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(settings)
	var s struct {
		Security map[string]string `json:"security"`
		Hooks    struct {
			BeforeTool []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Name, Type, Command string
				} `json:"hooks"`
			} `json:"BeforeTool"`
			AfterTool []any `json:"AfterTool"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	bt := s.Hooks.BeforeTool
	if s.Security["auth"] != "x" || s.Hooks.AfterTool == nil || len(bt) != 2 || bt[0].Hooks[0].Command != "other" ||
		bt[1].Matcher != "run_shell_command" || bt[1].Hooks[0].Name != "octoguard" || bt[1].Hooks[0].Command != "octoguard hook gemini" {
		t.Fatalf("settings = %s", raw)
	}
	if bak, _ := os.ReadFile(settings + ".octoguard.bak"); string(bak) != orig {
		t.Fatalf("backup = %q", bak)
	}
}

func TestEnforce_CopilotHookFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".copilot", "hooks", "octoguard.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Enforce(Options{Agents: []string{"copilot"}, Scope: ScopeUser, Home: home})
	if err != nil || res[1].Action != ActionUpdated {
		t.Fatalf("results = %+v, %v", res, err)
	}
	raw, _ := os.ReadFile(path)
	var f struct {
		Version int `json:"version"`
		Hooks   struct {
			PreToolUse []struct {
				Type, Bash string
				TimeoutSec int `json:"timeoutSec"`
			} `json:"preToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Version != 1 || len(f.Hooks.PreToolUse) != 1 || f.Hooks.PreToolUse[0].Type != "command" ||
		f.Hooks.PreToolUse[0].Bash != "octoguard hook copilot" || f.Hooks.PreToolUse[0].TimeoutSec == 0 {
		t.Fatalf("hook file = %s", raw)
	}
	if bak, _ := os.ReadFile(path + ".octoguard.bak"); string(bak) != `{"old":true}` {
		t.Fatalf("backup = %q", bak)
	}
}

func TestHookResponse_Agents(t *testing.T) {
	for _, tc := range []struct {
		agent, deny, allow, other string
		check                     func(map[string]any) bool
	}{
		{
			agent: "codex",
			deny:  `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"gh pr view 1"}}`,
			allow: `{"tool_name":"Bash","tool_input":{"command":"gh release list"}}`,
			other: `{"tool_name":"apply_patch","tool_input":{"command":"gh pr view 1"}}`,
			check: func(m map[string]any) bool {
				h, _ := m["hookSpecificOutput"].(map[string]any)
				return h["hookEventName"] == "PreToolUse" && h["permissionDecision"] == "deny" &&
					strings.Contains(h["permissionDecisionReason"].(string), "octoguard get pr")
			},
		},
		{
			agent: "gemini",
			deny:  `{"hook_event_name":"BeforeTool","tool_name":"run_shell_command","tool_input":{"command":"gh pr view 1","description":"d"}}`,
			allow: `{"tool_name":"run_shell_command","tool_input":{"command":"gh release list"}}`,
			other: `{"tool_name":"write_file","tool_input":{"command":"gh pr view 1"}}`,
			check: func(m map[string]any) bool {
				return m["decision"] == "deny" && strings.Contains(m["reason"].(string), "octoguard get pr")
			},
		},
		{
			agent: "copilot",
			deny:  `{"timestamp":1,"cwd":"/x","toolName":"bash","toolArgs":"{\"command\":\"gh pr view 1\",\"description\":\"d\"}"}`,
			allow: `{"toolName":"bash","toolArgs":"{\"command\":\"gh release list\"}"}`,
			other: `{"toolName":"view","toolArgs":"{\"command\":\"gh pr view 1\"}"}`,
			check: func(m map[string]any) bool {
				return m["permissionDecision"] == "deny" && strings.Contains(m["permissionDecisionReason"].(string), "octoguard get pr")
			},
		},
		{
			agent: "copilot",
			deny:  `{"tool_name":"Bash","tool_input":{"command":"gh pr view 1"}}`,
			allow: `{"toolName":"bash","toolArgs":{"command":"git status"}}`,
			other: `not json`,
			check: func(m map[string]any) bool { return m["permissionDecision"] == "deny" },
		},
	} {
		out, err := HookResponse(tc.agent, []byte(tc.deny))
		var m map[string]any
		if err != nil || json.Unmarshal(out, &m) != nil || !tc.check(m) {
			t.Fatalf("%s deny: %s, %v", tc.agent, out, err)
		}
		for _, in := range []string{tc.allow, tc.other} {
			if out, err := HookResponse(tc.agent, []byte(in)); err != nil || out != nil {
				t.Fatalf("%s: HookResponse(%s) = %s, %v; want allow", tc.agent, in, out, err)
			}
		}
	}
}
